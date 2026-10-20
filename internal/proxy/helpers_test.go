// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBackend is a loopback TCP server that writes "ID:<id>\n" on accept and
// then echoes. Closing its listener does not close accepted connections.
type fakeBackend struct {
	id string
	l  net.Listener

	mu    sync.Mutex
	conns []net.Conn
	wg    sync.WaitGroup
}

func newFakeBackend(t *testing.T, id string) *fakeBackend {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{id: id, l: l}
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			b.mu.Lock()
			b.conns = append(b.conns, c)
			b.mu.Unlock()
			b.wg.Add(1)
			go func() {
				defer b.wg.Done()
				defer c.Close()
				if _, err := io.WriteString(c, "ID:"+id+"\n"); err != nil {
					return
				}
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	t.Cleanup(b.shutdown)
	return b
}

func (b *fakeBackend) addr() string { return b.l.Addr().String() }

// closeListener stops accepting (simulates the node going away for new
// connections) but keeps existing connections alive.
func (b *fakeBackend) closeListener() { _ = b.l.Close() }

func (b *fakeBackend) shutdown() {
	_ = b.l.Close()
	b.mu.Lock()
	for _, c := range b.conns {
		_ = c.Close()
	}
	b.mu.Unlock()
	b.wg.Wait()
}

// client is a test connection through the proxy.
type client struct {
	c net.Conn
	r *bufio.Reader
}

// dialID connects and reads the backend ID line. It returns "" and the
// raw first bytes when the proxy refused with an ERR packet.
func dialID(t *testing.T, addr string) (*client, string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(c)
	first, err := r.Peek(3)
	if err != nil {
		_ = c.Close()
		return nil, ""
	}
	if string(first) == "ID:" { // backend greeting line; an ERR packet starts with a length header
		line, err := r.ReadString('\n')
		if err != nil {
			_ = c.Close()
			return nil, ""
		}
		_ = c.SetDeadline(time.Time{})
		cl := &client{c: c, r: r}
		t.Cleanup(func() { _ = c.Close() })
		return cl, strings.TrimPrefix(strings.TrimSpace(line), "ID:")
	}
	_ = c.Close()
	return nil, ""
}

// dialErr connects and returns the ERR packet message the proxy sent.
func dialErr(t *testing.T, addr string) (uint16, string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	_, p, err := readPacket(c, 1<<16)
	if err != nil {
		t.Fatalf("expected ERR packet, got %v", err)
	}
	if len(p) == 0 || p[0] != 0xff {
		t.Fatalf("expected ERR packet, got %q", p)
	}
	return parseErr(p)
}

// echo sends s and expects it back.
func (c *client) echo(s string) error {
	_ = c.c.SetDeadline(time.Now().Add(2 * time.Second))
	defer c.c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c.c, s+"\n"); err != nil {
		return err
	}
	got, err := c.r.ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(got) != s {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// waitClosed waits until the connection is closed by the proxy; returns
// the time it observed EOF or an error.
func (c *client) waitClosed(within time.Duration) (time.Time, bool) {
	_ = c.c.SetReadDeadline(time.Now().Add(within))
	var b [1]byte
	_, err := c.r.Read(b[:])
	if err == nil {
		return time.Time{}, false
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return time.Time{}, false
	}
	return time.Now(), true
}

func node(id, addr string) NodeConfig {
	return NodeConfig{ID: id, Address: addr, Role: "writer-eligible"}
}

// testConfig returns a fast-probing configuration with assume-synced sync.
func testConfig(nodes ...NodeConfig) Config {
	c := Config{
		WriteListen: "127.0.0.1:0",
		Nodes:       nodes,
		Probe: ProbeConfig{
			Sync:     SyncAssumeSynced,
			Interval: Duration(40 * time.Millisecond),
			Timeout:  Duration(40 * time.Millisecond),
		},
		Policy: PolicyConfig{
			StaleAfter:    Duration(500 * time.Millisecond),
			DrainDeadline: Duration(300 * time.Millisecond),
		},
	}
	c.applyDefaults()
	return c
}

func startProxy(t *testing.T, cfg Config, opts Options) *Proxy {
	t.Helper()
	p, err := New(cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
	return p
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout after %s waiting for %s", timeout, what)
}

func waitWriter(t *testing.T, p *Proxy, id string, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, "writer "+id, func() bool { return p.Status().Writer == id })
}

// goroutinesSettle waits until the goroutine count is at most base.
func goroutinesSettle(base int, timeout time.Duration) (int, bool) {
	deadline := time.Now().Add(timeout)
	n := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		if n = runtime.NumGoroutine(); n <= base {
			return n, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return n, false
}
