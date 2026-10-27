// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Lab E11: a real OpenSSH server offers ecdsa, ed25519 and rsa host keys. The
// operator pins one of them (ssh-keyscan -t ed25519). The Go client used its
// default algorithm preference (ecdsa first), the server answered with a key
// that is not in known_hosts, and every SSH-plan operation failed with
// "host-key or authentication failed". The runner must offer only the pinned
// key types.
func TestSSHRunnerNegotiatesPinnedHostKeyType(t *testing.T) {
	_, edPriv, _ := ed25519.GenerateKey(rand.Reader)
	edSigner, _ := ssh.NewSignerFromKey(edPriv)
	ecPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecSigner, _ := ssh.NewSignerFromKey(ecPriv)
	_, clientPriv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(clientPriv)
	clientPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if _, err := ssh.ParsePrivateKey([]byte(clientPEM)); err != nil {
		t.Skipf("PKCS8 key not parseable: %v", err)
	}

	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(ecSigner) // offered first by default clients
	cfg.AddHostKey(edSigner)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer l.Close()
	go serveOnce(l, cfg)

	host, portText, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(portText)
	known := knownhosts.Line([]string{net.JoinHostPort(host, portText)}, edSigner.PublicKey()) + "\n"
	out, err := SSHRunner{Port: port}.Run(context.Background(), Host{Address: host, User: "u"}, Credential{PrivateKey: clientPEM, KnownHosts: known}, "echo ok", "")
	if err != nil || !strings.Contains(out, "ok") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestHostKeyAlgorithmsFollowKnownHosts(t *testing.T) {
	ed, _, _ := ed25519.GenerateKey(rand.Reader)
	edPub, _ := ssh.NewPublicKey(ed)
	ecPriv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecPub, _ := ssh.NewPublicKey(&ecPriv.PublicKey)
	got, err := hostKeyAlgorithms(knownhosts.Line([]string{"h"}, edPub) + "\n")
	if err != nil || len(got) != 1 || got[0] != ssh.KeyAlgoED25519 {
		t.Fatalf("got %v err=%v", got, err)
	}
	got, _ = hostKeyAlgorithms(knownhosts.Line([]string{"h"}, edPub) + "\n" + knownhosts.Line([]string{"h"}, ecPub) + "\n")
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if _, err := hostKeyAlgorithms("# only a comment\n"); err == nil {
		t.Fatal("empty known_hosts accepted")
	}
}

// Lab E11 (race detector): ssh.Session copies stdout and stderr with io.Copy,
// which used the embedded bytes.Buffer.ReadFrom of limitedBuffer. That bypassed
// the 64 KiB cap and raced between the two copiers (flaky, lost output).
func TestLimitedBufferEnforcesCapThroughIOCopy(t *testing.T) {
	var b limitedBuffer
	if _, err := io.Copy(&b, strings.NewReader(strings.Repeat("x", 100<<10))); err != nil {
		t.Fatal(err)
	}
	if !b.full || b.Len() > 64<<10 {
		t.Fatalf("cap bypassed: len=%d full=%v", b.Len(), b.full)
	}
}

func serveOnce(l net.Listener, cfg *ssh.ServerConfig) {
	c, err := l.Accept()
	if err != nil {
		return
	}
	defer c.Close()
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		for r := range creqs {
			if r.Type == "exec" {
				_ = r.Reply(true, nil)
				_, _ = ch.Write([]byte("ok\n"))
				_ = ch.CloseWrite()
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, 0)
				_, _ = ch.SendRequest("exit-status", false, status)
				_ = ch.Close()
				break
			}
			_ = r.Reply(false, nil)
		}
	}
}
