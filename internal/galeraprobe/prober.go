// SPDX-License-Identifier: AGPL-3.0-only

package galeraprobe

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// DefaultTimeout bounds one probe (connect, authenticate, query) when
// Config.Timeout is zero.
const DefaultTimeout = 2 * time.Second

// maxStatusRows bounds the rows read from one answer. SHOW GLOBAL STATUS
// LIKE 'wsrep_%' returns well under 100 rows.
const maxStatusRows = 512

// statusQuery needs no privilege beyond a logged-in account.
const statusQuery = "SHOW GLOBAL STATUS LIKE 'wsrep_%'"

// Sentinel error kinds. Errors returned by Prober match them with errors.Is.
var (
	// ErrAccessDenied: the server rejected the monitoring account.
	ErrAccessDenied = errors.New("galeraprobe: access denied")
	// ErrTLSUnavailable: TLS was required but the server does not offer it.
	ErrTLSUnavailable = errors.New("galeraprobe: server does not offer TLS")
)

// Config configures a Prober.
type Config struct {
	// Credentials is required.
	Credentials CredentialSource
	// TLS, if not nil, makes TLS mandatory: the probe fails if the server
	// does not offer it (no downgrade to plaintext). Nil means plaintext.
	TLS *tls.Config
	// Timeout bounds one whole probe; default DefaultTimeout. The caller's
	// context deadline applies as well; the earlier one wins.
	Timeout time.Duration
}

// Prober queries wsrep status. It is safe for concurrent use and holds no
// connection between probes.
type Prober struct {
	cfg Config
}

// New validates cfg and returns a Prober.
func New(cfg Config) (*Prober, error) {
	if cfg.Credentials == nil {
		return nil, errors.New("galeraprobe: Credentials are required")
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("galeraprobe: negative Timeout")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.TLS != nil {
		cfg.TLS = cfg.TLS.Clone()
	}
	return &Prober{cfg: cfg}, nil
}

// probeError carries a redacted message and a matchable kind. The underlying
// driver error is deliberately not exposed through Unwrap.
type probeError struct {
	msg  string
	kind error
}

func (e *probeError) Error() string        { return e.msg }
func (e *probeError) Is(target error) bool { return e.kind != nil && target == e.kind }

// Status connects to addr (host:port), runs SHOW GLOBAL STATUS LIKE
// 'wsrep_%' and returns the variables. Every call opens and closes its own
// connection.
func (p *Prober) Status(ctx context.Context, addr string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	cred, err := p.cfg.Credentials.Credentials(ctx)
	if err != nil {
		return nil, &probeError{msg: "galeraprobe: credentials: " + redact(err.Error(), "")}
	}
	vars, err := p.query(ctx, addr, cred)
	if err != nil {
		return nil, p.classify(addr, err, cred.Password)
	}
	return vars, nil
}

func (p *Prober) query(ctx context.Context, addr string, cred Credentials) (map[string]string, error) {
	mc := mysql.NewConfig()
	mc.User, mc.Passwd = cred.User, cred.Password
	mc.Net, mc.Addr = "tcp", addr
	mc.Timeout, mc.ReadTimeout, mc.WriteTimeout = p.cfg.Timeout, p.cfg.Timeout, p.cfg.Timeout
	mc.AllowNativePasswords = true
	mc.AllowCleartextPasswords = false
	mc.AllowOldPasswords = false
	mc.AllowFallbackToPlaintext = false
	if p.cfg.TLS != nil {
		t := p.cfg.TLS.Clone()
		if t.ServerName == "" {
			if host, _, err := net.SplitHostPort(addr); err == nil {
				t.ServerName = host
			}
		}
		mc.TLS = t
	}
	conn, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(conn)
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0)
	rows, err := db.QueryContext(ctx, statusQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vars := map[string]string{}
	for rows.Next() {
		if len(vars) >= maxStatusRows {
			return nil, errors.New("too many status rows")
		}
		var k, v sql.NullString
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		if k.Valid && v.Valid {
			vars[k.String] = v.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return vars, nil
}

func (p *Prober) classify(addr string, err error, secret string) error {
	var kind error
	var me *mysql.MySQLError
	switch {
	case errors.Is(err, mysql.ErrNoTLS):
		kind = ErrTLSUnavailable
	case errors.As(err, &me) && (me.Number == 1045 || me.Number == 1044 || me.Number == 1698 || me.Number == 1227):
		kind = ErrAccessDenied
	}
	return &probeError{msg: fmt.Sprintf("galeraprobe: %s: %s", addr, redact(err.Error(), secret)), kind: kind}
}

func redact(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}

// Probe returns the node's Galera health. A transport, authentication or
// timeout failure returns the zero (Unknown) health and the error. An answer
// that is incomplete or unparsable returns Unknown health with reasons and a
// nil error; Unknown is never healthy.
func (p *Prober) Probe(ctx context.Context, addr string) (galera.NodeHealth, error) {
	vars, err := p.Status(ctx, addr)
	if err != nil {
		return galera.NodeHealth{Reasons: []string{err.Error()}}, err
	}
	return galera.HealthFromStatus(vars), nil
}
