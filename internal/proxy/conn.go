// SPDX-License-Identifier: AGPL-3.0-only

package proxy

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// tconn is one proxied client connection.
type tconn struct {
	ep      Endpoint
	node    string
	ip      string
	started time.Time
	client  net.Conn

	mu       sync.Mutex
	backend  net.Conn
	closed   bool
	drain    *time.Timer
	drainEnd time.Time
}

// attach records the backend connection; false if already closed.
func (c *tconn) attach(b net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = b.Close()
		return false
	}
	c.backend = b
	return true
}

// close closes both sides; idempotent.
func (c *tconn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drain != nil {
		c.drain.Stop()
		c.drain = nil
	}
	if c.closed {
		return
	}
	c.closed = true
	_ = c.client.Close()
	if c.backend != nil {
		_ = c.backend.Close()
	}
}

// scheduleClose closes the connection at deadline (immediately if passed).
// An existing earlier deadline is kept. No goroutine runs until it fires.
func (c *tconn) scheduleClose(deadline, now time.Time) {
	c.mu.Lock()
	if c.closed || (c.drain != nil && !deadline.Before(c.drainEnd)) {
		c.mu.Unlock()
		return
	}
	if c.drain != nil {
		c.drain.Stop()
	}
	c.drainEnd = deadline
	if d := deadline.Sub(now); d > 0 {
		c.drain = time.AfterFunc(d, c.close)
		c.mu.Unlock()
		return
	}
	c.drain = nil
	c.mu.Unlock()
	c.close()
}

// cancelDrain stops a pending drain (the node is routed to again).
func (c *tconn) cancelDrain() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drain != nil {
		c.drain.Stop()
		c.drain = nil
		c.drainEnd = time.Time{}
	}
}

func (c *tconn) draining() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.drain != nil
}

const copyBufSize = 32 << 10

var bufPool = sync.Pool{New: func() any { b := make([]byte, copyBufSize); return &b }}

// pipe relays bytes in both directions until either side ends, then closes
// both. idle is the maximum time without traffic in either direction; it
// also bounds every write. Each direction uses one fixed-size buffer.
func pipe(a, b net.Conn, idle time.Duration) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	done := make(chan struct{})
	go func() {
		defer close(done)
		copyIdle(b, a, idle, &last)
		_ = a.Close()
		_ = b.Close()
	}()
	copyIdle(a, b, idle, &last)
	_ = a.Close()
	_ = b.Close()
	<-done
}

func copyIdle(dst, src net.Conn, idle time.Duration, last *atomic.Int64) {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		_ = src.SetReadDeadline(time.Unix(0, last.Load()).Add(idle))
		n, err := src.Read(buf)
		if n > 0 {
			now := time.Now()
			last.Store(now.UnixNano())
			_ = dst.SetWriteDeadline(now.Add(idle))
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && time.Since(time.Unix(0, last.Load())) < idle {
				continue // the other direction was active
			}
			return
		}
	}
}
