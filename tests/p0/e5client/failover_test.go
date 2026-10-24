// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"net"
	"testing"
	"time"
)

func listen(t *testing.T) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return l.Addr().String(), func() { l.Close() }
}

func TestFailoverDialUsesFirstReachableInOrder(t *testing.T) {
	a, stopA := listen(t)
	b, stopB := listen(t)
	defer stopB()
	dial := failoverDial([]string{a, b}, time.Second)
	c, err := dial(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RemoteAddr().String(); got != a {
		t.Fatalf("dialed %s, want first address %s", got, a)
	}
	c.Close()
	stopA()
	c, err = dial(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.RemoteAddr().String(); got != b {
		t.Fatalf("after the first address went away dialed %s, want %s", got, b)
	}
}

func TestFailoverDialReportsAllFailures(t *testing.T) {
	dial := failoverDial([]string{"127.0.0.1:1", "127.0.0.1:2"}, 200*time.Millisecond)
	if _, err := dial(context.Background(), ""); err == nil {
		t.Fatal("expected an error when no address is reachable")
	}
}

func TestSplitAddrs(t *testing.T) {
	if got := splitAddrs(" a:1, b:2 ,"); len(got) != 2 || got[0] != "a:1" || got[1] != "b:2" {
		t.Fatalf("splitAddrs = %q", got)
	}
}
