// SPDX-License-Identifier: AGPL-3.0-only

// Command e5client is a lab-only MariaDB client for experiment E5 (the
// internal/proxy L4 proxy in front of a real Galera cluster). It uses
// go-sql-driver/mysql, the same driver as internal/galeraprobe.
//
//	e5client gencert -dir D -name N           CA + server certificate (ECDSA P-256) for the proxy
//	e5client status -url http://127.0.0.1:9306/status
//	e5client query  -addr H:P -user U -password-file F [-tls off|CAFILE] [-server-name N] SQL
//	e5client load   -addr H:P -user U -password-file F [-tls ...] -duration 90s -interval 100ms
//	                [-mode persistent|per-connection] [-label L]
//	e5client bench  -addr H:P -user U -password-file F [-n 300]
//
// -addr accepts a comma-separated list; with several addresses the client
// tries them in order for every new connection (client-side failover, E8).
//
// load writes one row per tick into e5.load (node = @@hostname of the
// serving node) and prints one line per state change (first success, error,
// recovery, node change) plus a JSON summary: successes, errors, writes per
// node, the longest interval without a successful write and the transitions.
// In persistent mode one connection is kept and replaced after an error; in
// per-connection mode every write opens a new connection.
//
// Passwords are read from files and never printed. Lab use only.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: e5client gencert|status|query|load|bench ...")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "gencert":
		err = gencert(os.Args[2:])
	case "status":
		err = status(os.Args[2:])
	case "query":
		err = query(os.Args[2:])
	case "load":
		err = load(os.Args[2:])
	case "bench":
		err = bench(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "e5client:", err)
		os.Exit(1)
	}
}

type dbFlags struct {
	addr, user, pwFile, tlsMode, serverName string
	timeout                                 time.Duration
}

func (d *dbFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&d.addr, "addr", "127.0.0.1:3306", "server address")
	fs.StringVar(&d.user, "user", "", "user")
	fs.StringVar(&d.pwFile, "password-file", "", "password file")
	fs.StringVar(&d.tlsMode, "tls", "off", `"off" (no TLS) or a CA file (TLS required and verified)`)
	fs.StringVar(&d.serverName, "server-name", "", "TLS server name (default: host of -addr)")
	fs.DurationVar(&d.timeout, "timeout", 3*time.Second, "connect/read/write timeout")
}

func (d *dbFlags) open() (*sql.DB, error) {
	pw, err := os.ReadFile(d.pwFile)
	if err != nil {
		return nil, err
	}
	c := mysql.NewConfig()
	c.User, c.Passwd, c.Net, c.Addr = d.user, strings.TrimSpace(string(pw)), "tcp", d.addr
	if addrs := splitAddrs(d.addr); len(addrs) > 1 {
		// Client-side failover (experiment E8): try the addresses in the
		// order given, per connection attempt, like a multi-host client.
		mysql.RegisterDialContext("failover", failoverDial(addrs, time.Second))
		c.Net, c.Addr = "failover", addrs[0]
	}
	c.Timeout, c.ReadTimeout, c.WriteTimeout = d.timeout, d.timeout, d.timeout
	if d.tlsMode != "off" {
		pemBytes, err := os.ReadFile(d.tlsMode)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, errors.New("no CA certificate in " + d.tlsMode)
		}
		sn := d.serverName
		if sn == "" {
			sn, _, _ = net.SplitHostPort(d.addr)
		}
		c.TLS = &tls.Config{RootCAs: pool, ServerName: sn, MinVersion: tls.VersionTLS12}
	}
	conn, err := mysql.NewConnector(c)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(conn), nil
}

// splitAddrs splits a comma-separated address list.
func splitAddrs(s string) []string {
	var out []string
	for _, a := range strings.Split(s, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// failoverDial returns a dial function that tries the addresses in order
// with a per-address timeout and returns the first connection that works.
func failoverDial(addrs []string, per time.Duration) func(context.Context, string) (net.Conn, error) {
	return func(ctx context.Context, _ string) (net.Conn, error) {
		var errs []error
		for _, a := range addrs {
			d := net.Dialer{Timeout: per}
			c, err := d.DialContext(ctx, "tcp", a)
			if err == nil {
				return c, nil
			}
			errs = append(errs, err)
		}
		return nil, errors.Join(errs...)
	}
}

func gencert(args []string) error {
	fs := flag.NewFlagSet("gencert", flag.ExitOnError)
	dir := fs.String("dir", ".", "output directory")
	name := fs.String("name", "dbcm-e5-proxy", "server DNS name")
	_ = fs.Parse(args)
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	caT := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dbcm E5 lab CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caT, caT, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	t := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: *name},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour), DNSNames: []string{*name, "localhost"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, t, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(key)
	files := map[string][]byte{
		"ca.pem":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		"cert.pem": append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...),
		"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}),
	}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(*dir, n), b, 0o644); err != nil {
			return err
		}
	}
	fmt.Printf("certificate for %s (and localhost, 127.0.0.1) written to %s: ca.pem cert.pem key.pem\n", *name, *dir)
	return nil
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:9306/status", "status URL")
	_ = fs.Parse(args)
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(*url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	fmt.Printf("HTTP %d\n%s", resp.StatusCode, b)
	return nil
}

func query(args []string) error {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	var d dbFlags
	d.register(fs)
	_ = fs.Parse(args)
	db, err := d.open()
	if err != nil {
		return err
	}
	defer db.Close()
	for _, q := range fs.Args() {
		rows, err := db.Query(q)
		if err != nil {
			return err
		}
		cols, _ := rows.Columns()
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			out := make([]string, len(vals))
			for i, v := range vals {
				out[i] = v.String
				if !v.Valid {
					out[i] = "NULL"
				}
			}
			fmt.Println(strings.Join(out, "\t"))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
	}
	return nil
}

type transition struct {
	At    string `json:"at"`
	Event string `json:"event"`
}

type summary struct {
	Label         string         `json:"label"`
	Mode          string         `json:"mode"`
	Duration      string         `json:"duration"`
	Attempts      int            `json:"attempts"`
	OK            int            `json:"ok"`
	Errors        int            `json:"errors"`
	PerNode       map[string]int `json:"per_node"`
	MaxGap        string         `json:"max_gap_without_success"`
	MaxGapFrom    string         `json:"max_gap_from,omitempty"`
	Reconnects    int            `json:"reconnects"`
	NodeChanges   int            `json:"node_changes"`
	Transitions   []transition   `json:"transitions"`
	ErrorExamples []string       `json:"error_examples,omitempty"`
}

func load(args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	var d dbFlags
	d.register(fs)
	dur := fs.Duration("duration", 60*time.Second, "run time")
	interval := fs.Duration("interval", 100*time.Millisecond, "time between writes")
	mode := fs.String("mode", "persistent", "persistent or per-connection")
	label := fs.String("label", "load", "label stored with each row")
	_ = fs.Parse(args)
	db, err := d.open()
	if err != nil {
		return err
	}
	defer db.Close()
	if *mode == "per-connection" {
		db.SetMaxIdleConns(0)
	} else {
		db.SetMaxIdleConns(1)
	}
	db.SetMaxOpenConns(1)
	s := summary{Label: *label, Mode: *mode, Duration: dur.String(), PerNode: map[string]int{}}
	start := time.Now()
	lastOK := start
	var conn *sql.Conn
	lastNode, lastState := "", ""
	event := func(e string) {
		t := transition{At: time.Now().UTC().Format("15:04:05.000"), Event: e}
		s.Transitions = append(s.Transitions, t)
		fmt.Printf("%s %s\n", t.At, e)
	}
	var maxGap time.Duration
	for time.Since(start) < *dur {
		tick := time.Now()
		s.Attempts++
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		var node string
		err := func() error {
			if *mode == "per-connection" {
				// One new connection per write: the INSERT and the
				// SELECT must use the same connection, or a round-robin
				// read endpoint alternates them over two nodes.
				c, err := db.Conn(ctx)
				if err != nil {
					return err
				}
				defer c.Close()
				if _, err = c.ExecContext(ctx, "INSERT INTO e5.load (node, label) SELECT @@hostname, ?", *label); err != nil {
					return err
				}
				return c.QueryRowContext(ctx, "SELECT @@hostname").Scan(&node)
			}
			if conn == nil {
				c, err := db.Conn(ctx)
				if err != nil {
					return err
				}
				conn = c
				if s.Attempts > 1 {
					s.Reconnects++
				}
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO e5.load (node, label) SELECT @@hostname, ?", *label); err != nil {
				return err
			}
			return conn.QueryRowContext(ctx, "SELECT @@hostname").Scan(&node)
		}()
		cancel()
		if err != nil {
			s.Errors++
			if conn != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				_ = conn.Close()
				conn = nil
			}
			msg := err.Error()
			if len(s.ErrorExamples) < 8 && !slices.Contains(s.ErrorExamples, msg) {
				s.ErrorExamples = append(s.ErrorExamples, msg)
			}
			if lastState != "error" {
				event("error: " + msg)
				lastState = "error"
			}
		} else {
			now := time.Now()
			if g := now.Sub(lastOK); g > maxGap {
				maxGap = g
				s.MaxGapFrom = lastOK.UTC().Format("15:04:05.000")
			}
			lastOK = now
			s.OK++
			s.PerNode[node]++
			switch {
			case lastState == "":
				event("first success on " + node)
			case lastState == "error":
				event("recovered on " + node)
			case node != lastNode:
				// A round-robin read endpoint changes node on every
				// connection: keep the first changes, count the rest.
				if s.NodeChanges++; s.NodeChanges <= 10 {
					event("node changed " + lastNode + " -> " + node)
				}
			}
			lastState, lastNode = "ok", node
		}
		if w := *interval - time.Since(tick); w > 0 {
			time.Sleep(w)
		}
	}
	if conn != nil {
		_ = conn.Close()
	}
	if g := time.Since(lastOK); g > maxGap {
		maxGap = g
		s.MaxGapFrom = lastOK.UTC().Format("15:04:05.000")
	}
	s.MaxGap = maxGap.Round(time.Millisecond).String()
	b, _ := json.Marshal(s)
	fmt.Printf("SUMMARY %s\n", b)
	return nil
}

func bench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	var d dbFlags
	d.register(fs)
	n := fs.Int("n", 300, "round trips")
	_ = fs.Parse(args)
	db, err := d.open()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	t0 := time.Now()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	connect := time.Since(t0)
	defer conn.Close()
	lat := make([]time.Duration, 0, *n)
	var one int
	for range *n {
		t := time.Now()
		if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			return err
		}
		lat = append(lat, time.Since(t))
	}
	slices.Sort(lat)
	q := func(p float64) time.Duration { return lat[int(p*float64(len(lat)-1))].Round(time.Microsecond) }
	fmt.Printf("addr %s connect+auth %s, %d x SELECT 1: p50 %s p90 %s p99 %s max %s\n",
		d.addr, connect.Round(time.Microsecond), *n, q(0.5), q(0.9), q(0.99), lat[len(lat)-1].Round(time.Microsecond))
	return nil
}
