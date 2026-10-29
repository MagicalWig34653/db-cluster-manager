// SPDX-License-Identifier: AGPL-3.0-only

package imagecompose

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"slices"
	"strings"
)

// crossRules checks relations between settings and components that a single
// field schema cannot express. It runs on the normalized recipe.
func crossRules(n Recipe) []Issue {
	var is []Issue
	has := map[string]Selection{}
	for _, s := range n.Components {
		has[s.ID] = s
	}
	at := func(id, key string) string { return "components[" + id + "].settings." + key }

	// PHP run mode.
	if p, ok := has["php"]; ok && gs(p.Settings, "mode") == "worker" {
		for _, id := range webServerIDs {
			if _, w := has[id]; w {
				is = append(is, Issue{at("php", "mode"), "worker mode runs a PHP CLI script as the main process and cannot be combined with " + id})
			}
		}
	}

	// Trusted proxy lists of the web servers.
	for _, id := range webServerIDs {
		s, ok := has[id]
		if !ok {
			continue
		}
		if m := gs(s.Settings, "realIpMode"); m != "" && m != "off" {
			for _, c := range cidrList(gs(s.Settings, "trustedProxies")) {
				if _, _, err := net.ParseCIDR(c); err != nil {
					if ip := net.ParseIP(c); ip == nil || ip.To4() == nil {
						is = append(is, Issue{at(id, "trustedProxies"), fmt.Sprintf("%q is not an IPv4 address or CIDR range", c)})
					}
				}
			}
		}
	}

	// Rate limit burst only makes sense with a limit.
	if s, ok := has["nginx"]; ok && gi(s.Settings, "rateLimit") == 0 {
		if gi(s.Settings, "rateLimitBurst") != 20 {
			is = append(is, Issue{at("nginx", "rateLimitBurst"), "has no effect while rateLimit is 0"})
		}
	}

	// SFTP: host key and authorized keys are different files.
	if s, ok := has["sftp"]; ok && gs(s.Settings, "hostKeyPath") == gs(s.Settings, "authorizedKeysPath") {
		is = append(is, Issue{at("sftp", "authorizedKeysPath"), "must differ from the host key file"})
	}

	// Job runner: the job's tool must be in the image.
	if s, ok := has["jobrunner"]; ok {
		tool := map[string]string{"restic-backup": "restic", "rclone-sync": "rclone"}[gs(s.Settings, "job")]
		if tool != "" && !slices.Contains(has["backuptools"].Features, tool) {
			is = append(is, Issue{at("jobrunner", "job"), fmt.Sprintf("needs the %s tool: select it in Backup and sync tools", tool)})
		}
	}

	// System settings.
	if s, ok := has["system"]; ok {
		is = append(is, systemCross(s, n, has)...)
	}
	return is
}

func systemCross(s Selection, n Recipe, has map[string]Selection) []Issue {
	var is []Issue
	at := func(key string) string { return "components[system].settings." + key }
	switch gs(s.Settings, "healthcheck") {
	case "http":
		if httpPort(n) == 0 {
			is = append(is, Issue{at("healthcheck"), "http needs a web server or an application runtime with a listen port"})
		}
	case "fpm-ping":
		php, hasPHP := has["php"]
		_, hasNginx := has["nginx"]
		if !hasNginx || !hasPHP || gs(php.Settings, "mode") == "worker" {
			is = append(is, Issue{at("healthcheck"), "fpm-ping needs nginx together with PHP in fpm mode"})
		}
	}
	if text := gs(s.Settings, "extraCaPem"); text != "" {
		rest := []byte(text)
		count := 0
		for {
			var blk *pem.Block
			blk, rest = pem.Decode(rest)
			if blk == nil {
				break
			}
			count++
			if blk.Type != "CERTIFICATE" {
				is = append(is, Issue{at("extraCaPem"), "only CERTIFICATE blocks are accepted"})
				continue
			}
			cert, err := x509.ParseCertificate(blk.Bytes)
			switch {
			case err != nil:
				is = append(is, Issue{at("extraCaPem"), fmt.Sprintf("certificate %d cannot be parsed", count)})
			case !cert.IsCA:
				is = append(is, Issue{at("extraCaPem"), fmt.Sprintf("certificate %d is not a CA certificate", count)})
			}
		}
		if count == 0 || len(strings.TrimSpace(string(rest))) > 0 {
			is = append(is, Issue{at("extraCaPem"), "must consist of PEM certificates only"})
		}
	}
	return is
}
