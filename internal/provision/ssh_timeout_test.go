// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestSSHRunnerBoundsHandshakeAndSession(t *testing.T) {
	for _, stage := range []string{"banner", "session"} {
		for _, cancelRun := range []bool{false, true} {
			name := stage + "/timeout"
			if cancelRun {
				name = stage + "/cancel"
			}
			t.Run(name, func(t *testing.T) {
				_, key, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				signer, err := ssh.NewSignerFromKey(key)
				if err != nil {
					t.Fatal(err)
				}
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				accepted := make(chan net.Conn, 1)
				ready := make(chan struct{})
				go func() {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					defer conn.Close()
					accepted <- conn
					if stage == "banner" {
						close(ready)
						_, _ = io.Copy(io.Discard, conn)
						return
					}
					cfg := &ssh.ServerConfig{NoClientAuth: true}
					cfg.AddHostKey(signer)
					sc, channels, requests, err := ssh.NewServerConn(conn, cfg)
					if err != nil {
						return
					}
					defer sc.Close()
					go ssh.DiscardRequests(requests)
					for range channels {
						close(ready)
						_ = sc.Wait()
						return
					}
				}()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				host, portText, _ := net.SplitHostPort(ln.Addr().String())
				port, _ := strconv.Atoi(portText)
				timeout := 300 * time.Millisecond
				if cancelRun {
					timeout = time.Minute
				}
				done := make(chan error, 1)
				go func() {
					_, err := (SSHRunner{Port: port, Timeout: timeout}).Run(ctx, Host{Address: host, User: "u"}, Credential{
						Password: "test", KnownHosts: knownhosts.Line([]string{ln.Addr().String()}, signer.PublicKey())}, "true", "")
					done <- err
				}()
				var conn net.Conn
				select {
				case conn = <-accepted:
				case <-time.After(2 * time.Second):
					t.Fatal("no connection")
				}
				defer conn.Close()
				select {
				case <-ready:
				case <-time.After(2 * time.Second):
					t.Fatal("stage not reached")
				}
				if cancelRun {
					cancel()
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("stalled setup succeeded")
					}
					if cancelRun && !errors.Is(err, context.Canceled) {
						t.Fatalf("lost cancellation: %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("SSH setup ignored timeout/cancellation")
				}
			})
		}
	}
}
