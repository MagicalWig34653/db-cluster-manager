// SPDX-License-Identifier: AGPL-3.0-only

package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Credential is sensitive. Never serialize it into a Plan or an operation
// definition; the manager seals it in a separate document.
type Credential struct {
	PrivateKey string `json:"privateKey"`
	KnownHosts string `json:"knownHosts"`
}

// CommandRunner is the only host execution boundary. Fakes implement it
// without opening a socket. Stdin carries a short-lived join token.
type CommandRunner interface {
	Run(context.Context, Host, Credential, string, string) (string, error)
}

// SSHRunner requires a pinned OpenSSH known_hosts entry for host:22. It
// disables password and agent authentication and never accepts an unknown key.
type SSHRunner struct{ Timeout time.Duration }

func (r SSHRunner) Run(ctx context.Context, h Host, cred Credential, command, stdin string) (string, error) {
	if !safeName.MatchString(h.Address) || !safeName.MatchString(h.User) || strings.TrimSpace(cred.KnownHosts) == "" {
		return "", ErrInvalid
	}
	key, err := ssh.ParsePrivateKey([]byte(cred.PrivateKey))
	if err != nil {
		return "", fmt.Errorf("provision: invalid SSH private key")
	}
	f, err := os.CreateTemp("", "cluster-manager-known-hosts-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := io.WriteString(f, cred.KnownHosts); err != nil {
		return "", err
	}
	callback, err := knownhosts.New(f.Name())
	if err != nil {
		return "", fmt.Errorf("provision: invalid known_hosts")
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	address := net.JoinHostPort(h.Address, "22")
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
	if err != nil {
		return "", fmt.Errorf("provision: SSH dial failed")
	}
	defer conn.Close()
	config := &ssh.ClientConfig{User: h.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: callback, Timeout: timeout}
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		return "", fmt.Errorf("provision: SSH host-key or authentication failed")
	}
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("provision: SSH session failed")
	}
	defer session.Close()
	session.Stdin = strings.NewReader(stdin)
	var out limitedBuffer
	session.Stdout, session.Stderr = &out, &out
	done := make(chan error, 1)
	go func() { done <- session.Run("sh -c " + shellQuote(command)) }()
	select {
	case <-ctx.Done():
		_ = client.Close()
		return "", ctx.Err()
	case err := <-done:
		if err != nil {
			return "", errors.New("provision: remote command failed")
		}
		if out.full {
			return "", errors.New("provision: remote output too large")
		}
		return out.String(), nil
	}
}

type limitedBuffer struct {
	bytes.Buffer
	full bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > 64<<10 {
		b.full = true
		return n, nil
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
