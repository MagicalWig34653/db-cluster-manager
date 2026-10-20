// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"context"
	"regexp"
	"strconv"
	"time"
)

// PMAAuthMode selects how users log in to phpMyAdmin (D04).
type PMAAuthMode string

const (
	// PMAAuthCookie shows the phpMyAdmin login form; the user enters
	// database credentials. This is the separate-credentials alternative.
	PMAAuthCookie PMAAuthMode = "cookie"
	// PMAAuthSignon is the default design: the manager authenticates the
	// user, authorizes it, and hands phpMyAdmin a one-time ticket mapped to
	// a database account with matching privileges. Only the interface below
	// is defined; no implementation exists yet.
	PMAAuthSignon PMAAuthMode = "signon"
)

// PMAParams configures a phpMyAdmin Swarm service.
type PMAParams struct {
	Name         string // service name, e.g. "pma-prod"
	Image        string // pinned image reference, never "latest"
	HostNode     string // Swarm node hostname the service is pinned to
	ProxyHost    string // DNS name / VIP of the cluster's SQL proxy endpoint (PMA_HOST)
	ProxyPort    int    // default 3306
	PublishPort  int    // optional published port on HostNode (0 = none, reach via ingress proxy)
	Network      string // overlay network shared with the proxy
	AbsoluteURI  string // external URL, optional
	Auth         PMAAuthMode
	UploadLimit  string // e.g. "64M", default "64M"
	MemoryBytes  int64  // default 256 MiB
	StopGrace    time.Duration
	ClusterLabel string // owner label value for the ownership inventory
}

// PMAServiceSpec is plain data for the orchestrator; nothing here talks to Docker.
type PMAServiceSpec struct {
	Name          string
	Image         string
	Constraints   []string
	Env           map[string]string
	Labels        map[string]string
	Networks      []string
	TargetPort    int
	PublishedPort int // 0 = not published
	MemoryBytes   int64
	StopGrace     time.Duration
	RestartPolicy string
	Replicas      uint64
}

var (
	rePMAName   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	rePMAImage  = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*:[A-Za-z0-9._-]+(@sha256:[a-f0-9]{64})?$`)
	rePMAHost   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)
	rePMAUpload = regexp.MustCompile(`^[0-9]{1,5}[KMG]$`)
)

// BuildPMAServiceSpec validates p and returns the service spec. PMA_ARBITRARY
// is off so users cannot point phpMyAdmin at other servers (SSRF).
func BuildPMAServiceSpec(p PMAParams) (PMAServiceSpec, error) {
	if !rePMAName.MatchString(p.Name) {
		return PMAServiceSpec{}, invalid("service name", "%s", safeQuote(p.Name))
	}
	if !rePMAImage.MatchString(p.Image) || len(p.Image) > 200 || hasLatestTag(p.Image) {
		return PMAServiceSpec{}, invalid("image", "must be pinned (explicit non-latest tag): %s", safeQuote(p.Image))
	}
	if !rePMAHost.MatchString(p.HostNode) {
		return PMAServiceSpec{}, invalid("host node", "%s", safeQuote(p.HostNode))
	}
	if !rePMAHost.MatchString(p.ProxyHost) {
		return PMAServiceSpec{}, invalid("proxy host", "%s", safeQuote(p.ProxyHost))
	}
	if p.ProxyPort == 0 {
		p.ProxyPort = 3306
	}
	if p.ProxyPort < 1 || p.ProxyPort > 65535 || p.PublishPort < 0 || p.PublishPort > 65535 {
		return PMAServiceSpec{}, invalid("port", "out of range")
	}
	if p.Network != "" && !rePMAName.MatchString(p.Network) {
		return PMAServiceSpec{}, invalid("network", "%s", safeQuote(p.Network))
	}
	if p.Auth == "" {
		p.Auth = PMAAuthSignon
	}
	if p.Auth != PMAAuthSignon && p.Auth != PMAAuthCookie {
		return PMAServiceSpec{}, invalid("auth mode", "%s", safeQuote(string(p.Auth)))
	}
	if p.UploadLimit == "" {
		p.UploadLimit = "64M"
	}
	if !rePMAUpload.MatchString(p.UploadLimit) {
		return PMAServiceSpec{}, invalid("upload limit", "%s", safeQuote(p.UploadLimit))
	}
	if p.MemoryBytes == 0 {
		p.MemoryBytes = 256 << 20
	}
	if p.MemoryBytes < 64<<20 {
		return PMAServiceSpec{}, invalid("memory", "at least 64 MiB required")
	}
	if p.StopGrace == 0 {
		p.StopGrace = 10 * time.Second
	}
	env := map[string]string{
		"PMA_HOST":           p.ProxyHost,
		"PMA_PORT":           strconv.Itoa(p.ProxyPort),
		"PMA_ARBITRARY":      "0",
		"UPLOAD_LIMIT":       p.UploadLimit,
		"MAX_EXECUTION_TIME": "300",
	}
	if p.AbsoluteURI != "" {
		if len(p.AbsoluteURI) > 512 || !regexp.MustCompile(`^https?://[A-Za-z0-9.:/_-]+$`).MatchString(p.AbsoluteURI) {
			return PMAServiceSpec{}, invalid("absolute URI", "%s", safeQuote(p.AbsoluteURI))
		}
		env["PMA_ABSOLUTE_URI"] = p.AbsoluteURI
	}
	labels := map[string]string{
		"dbcm.managed":  "true",
		"dbcm.role":     "phpmyadmin",
		"dbcm.pma.auth": string(p.Auth),
	}
	if p.ClusterLabel != "" {
		if !rePMAName.MatchString(p.ClusterLabel) {
			return PMAServiceSpec{}, invalid("cluster label", "%s", safeQuote(p.ClusterLabel))
		}
		labels["dbcm.cluster"] = p.ClusterLabel
	}
	spec := PMAServiceSpec{
		Name:          p.Name,
		Image:         p.Image,
		Constraints:   []string{"node.hostname == " + p.HostNode},
		Env:           env,
		Labels:        labels,
		TargetPort:    80,
		PublishedPort: p.PublishPort,
		MemoryBytes:   p.MemoryBytes,
		StopGrace:     p.StopGrace,
		RestartPolicy: "on-failure",
		Replicas:      1,
	}
	if p.Network != "" {
		spec.Networks = []string{p.Network}
	}
	return spec, nil
}

func hasLatestTag(img string) bool {
	m := regexp.MustCompile(`:([A-Za-z0-9._-]+)(@|$)`).FindStringSubmatch(img)
	return m != nil && m[1] == "latest"
}

// SignonProvider is the documented extension point for automatic login
// (D04 default). It is NOT implemented in this package.
//
// Intended flow: the authenticated manager user asks for access to a cluster;
// the manager authorizes the request against RBAC, picks or creates a
// short-lived database account whose privileges match the user's role, and
// returns a single-use Ticket. The browser is redirected to phpMyAdmin's
// signon script with the ticket; phpMyAdmin redeems it server-to-server
// (Redeem) and receives the credentials, which never appear in URLs, logs or
// the browser. Tickets expire in seconds and are bound to the user session.
type SignonProvider interface {
	// Issue creates a one-time ticket for the caller (identified by ctx).
	Issue(ctx context.Context, req SignonRequest) (SignonTicket, error)
	// Redeem exchanges a ticket exactly once for database credentials.
	Redeem(ctx context.Context, ticketID string) (SignonCredentials, error)
}

// SignonRequest asks for phpMyAdmin access.
type SignonRequest struct {
	ClusterID string
	UserID    string // manager user
	Database  string // optional: restrict to one database
	ReadOnly  bool
}

// SignonTicket is returned to the manager front end.
type SignonTicket struct {
	ID        string
	ExpiresAt time.Time
	URL       string // phpMyAdmin signon URL carrying only the ticket ID
}

// SignonCredentials are the redeemed database credentials.
type SignonCredentials struct {
	User     string
	Password Secret
}
