// SPDX-License-Identifier: AGPL-3.0-only

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sleeper struct {
	mu sync.Mutex
	ds []time.Duration
}

func (s *sleeper) Sleep(d time.Duration) { s.mu.Lock(); s.ds = append(s.ds, d); s.mu.Unlock() }

func hostPort(t *testing.T, srv *httptest.Server) (host, port string) {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	return u.Hostname(), u.Port()
}

func testWebhook(t *testing.T, target string, mod func(*WebhookConfig)) (*Webhook, *sleeper) {
	t.Helper()
	sl := &sleeper{}
	cfg := WebhookConfig{
		URL: target, Secret: []byte("s3cret"), Timeout: time.Second,
		MaxRetries: 2, Backoff: 100 * time.Millisecond, AllowPrivate: true,
		Sleep: sl.Sleep, Now: func() time.Time { return t0 },
	}
	if mod != nil {
		mod(&cfg)
	}
	w, err := NewWebhook(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return w, sl
}

func TestWebhookPostsSignedJSON(t *testing.T) {
	var gotBody []byte
	var gotHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHdr = r.Header.Clone()
	}))
	defer srv.Close()
	w, _ := testWebhook(t, srv.URL, nil)
	ev := Event{ID: "ev-1", Time: t0, Severity: SeverityError, Kind: "node.down", Resource: "cluster/a", Message: "m"}
	if err := w.Send(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	var back Event
	if err := json.Unmarshal(gotBody, &back); err != nil || back.ID != "ev-1" || back.Kind != "node.down" {
		t.Fatalf("body %s err %v", gotBody, err)
	}
	if gotHdr.Get("Content-Type") != "application/json" {
		t.Fatal("content type")
	}
	ts := gotHdr.Get(HeaderTimestamp)
	if ts == "" || !VerifySignature([]byte("s3cret"), ts, gotBody, gotHdr.Get(HeaderSignature)) {
		t.Fatalf("signature invalid: %q %q", ts, gotHdr.Get(HeaderSignature))
	}
	if VerifySignature([]byte("other"), ts, gotBody, gotHdr.Get(HeaderSignature)) {
		t.Fatal("wrong secret must fail")
	}
	if VerifySignature([]byte("s3cret"), ts, append(gotBody, ' '), gotHdr.Get(HeaderSignature)) {
		t.Fatal("tampered body must fail")
	}
}

func TestWebhookRetriesWithBackoff(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	w, sl := testWebhook(t, srv.URL, nil)
	if err := w.Send(context.Background(), Event{}); err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(sl.ds) != 2 || sl.ds[0] != 100*time.Millisecond || sl.ds[1] != 200*time.Millisecond {
		t.Fatalf("attempts %d sleeps %v", n, sl.ds)
	}
}

func TestWebhookGivesUpAfterBoundedRetries(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	w, _ := testWebhook(t, srv.URL, nil)
	if err := w.Send(context.Background(), Event{}); err == nil {
		t.Fatal("expected error")
	}
	if n != 3 {
		t.Fatalf("attempts %d, want 1+MaxRetries", n)
	}
}

func TestWebhookNoRetryOnClientError(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(400)
	}))
	defer srv.Close()
	w, sl := testWebhook(t, srv.URL, nil)
	if err := w.Send(context.Background(), Event{}); err == nil || n != 1 || len(sl.ds) != 0 {
		t.Fatalf("err %v attempts %d", err, n)
	}
}

func TestWebhookRedirectNotFollowed(t *testing.T) {
	var hits int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	w, _ := testWebhook(t, srv.URL, nil)
	if err := w.Send(context.Background(), Event{}); err == nil {
		t.Fatal("redirect must be an error")
	}
	if hits != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestWebhookTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	w, _ := testWebhook(t, srv.URL, func(c *WebhookConfig) { c.Timeout = 50 * time.Millisecond; c.MaxRetries = 0 })
	start := time.Now()
	if err := w.Send(context.Background(), Event{}); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not enforced")
	}
}

func TestWebhookSSRFBlockedByDefault(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	_, port := hostPort(t, srv)

	resolve := func(m map[string][]string) Resolver {
		return func(_ context.Context, host string) ([]net.IP, error) {
			var out []net.IP
			for _, s := range m[host] {
				out = append(out, net.ParseIP(s))
			}
			if out == nil {
				return nil, errors.New("nxdomain")
			}
			return out, nil
		}
	}
	cases := map[string]string{
		"loopback literal":   srv.URL,
		"link-local literal": "http://169.254.169.254:" + port + "/",
		"private literal":    "http://10.1.2.3:" + port + "/",
		"ipv6 loopback":      "http://[::1]:" + port + "/",
		"ipv6 ula":           "http://[fd00::1]:" + port + "/",
		"mapped loopback":    "http://[::ffff:127.0.0.1]:" + port + "/",
		"name to loopback":   "http://a.test:" + port + "/",
		"name to private":    "http://b.test:" + port + "/",
		"mixed answers":      "http://c.test:" + port + "/",
		"cgnat":              "http://100.64.0.1:" + port + "/",
		"unspecified":        "http://0.0.0.0:" + port + "/",
	}
	res := resolve(map[string][]string{
		"a.test": {"127.0.0.1"}, "b.test": {"192.168.1.1"}, "c.test": {"8.8.8.8", "127.0.0.1"},
	})
	for name, target := range cases {
		w, sl := testWebhook(t, target, func(c *WebhookConfig) { c.AllowPrivate = false; c.Resolver = res })
		err := w.Send(context.Background(), Event{})
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s: want ErrBlockedAddress, got %v", name, err)
		}
		if len(sl.ds) != 0 {
			t.Errorf("%s: blocked targets must not be retried", name)
		}
	}
	if hits != 0 {
		t.Fatal("blocked request reached the server")
	}
}

func TestWebhookAllowPrivateAndResolverPinning(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Host == "" {
			t.Error("host header missing")
		}
	}))
	defer srv.Close()
	_, port := hostPort(t, srv)
	var lookups int32
	res := func(_ context.Context, host string) ([]net.IP, error) {
		atomic.AddInt32(&lookups, 1)
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	w, _ := testWebhook(t, "http://hook.test:"+port+"/x", func(c *WebhookConfig) { c.Resolver = res })
	if err := w.Send(context.Background(), Event{}); err != nil {
		t.Fatal(err)
	}
	if hits != 1 || lookups != 1 {
		t.Fatalf("hits %d lookups %d (validated IP must be the dialed IP)", hits, lookups)
	}
}

func TestWebhookConfigValidation(t *testing.T) {
	for name, cfg := range map[string]WebhookConfig{
		"no secret":  {URL: "https://example.com/"},
		"bad scheme": {URL: "ftp://example.com/", Secret: []byte("x")},
		"no host":    {URL: "https:///x", Secret: []byte("x")},
		"empty":      {},
		"userinfo":   {URL: "https://u:p@example.com/", Secret: []byte("x")},
	} {
		if _, err := NewWebhook(cfg); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestIPAllowed(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.0.0.1": false, "172.16.0.1": false, "192.168.0.1": false,
		"169.254.1.1": false, "::1": false, "fe80::1": false, "fc00::1": false,
		"0.0.0.0": false, "224.0.0.1": false, "100.64.0.1": false, "::ffff:10.0.0.1": false,
	} {
		if got := ipAllowed(net.ParseIP(ip)); got != want {
			t.Errorf("%s: got %v want %v", ip, got, want)
		}
	}
}
