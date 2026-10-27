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
	"slices"
	"strconv"
	"strings"
	"sync"
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
type SSHRunner struct {
	Timeout time.Duration
	// Port defaults to 22 (tests only).
	Port int
}

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
	port := r.Port
	if port <= 0 {
		port = 22
	}
	address := net.JoinHostPort(h.Address, strconv.Itoa(port))
	algos, err := hostKeyAlgorithms(cred.KnownHosts)
	if err != nil {
		return "", fmt.Errorf("provision: invalid known_hosts")
	}
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
	if err != nil {
		return "", fmt.Errorf("provision: SSH dial failed")
	}
	defer conn.Close()
	config := &ssh.ClientConfig{User: h.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: callback, HostKeyAlgorithms: algos, Timeout: timeout}
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

// limitedBuffer caps remote output at 64 KiB. It is shared by the stdout and
// stderr copiers of ssh.Session, so it is mutex-guarded, and it does not
// embed bytes.Buffer: io.Copy would otherwise use the promoted ReadFrom,
// which bypasses Write (and with it the cap) and races (lab E11).
type limitedBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	full bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.buf.Len()+n > 64<<10 {
		b.full = true
		return n, nil
	}
	_, _ = b.buf.Write(p)
	return n, nil
}

func (b *limitedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// hostKeyAlgorithms returns the host key algorithms of the pinned known_hosts
// entries. Without this the client offers its own default preference
// (ecdsa first); a server with several host keys then answers with a key that
// is not pinned and the connection fails although the pinned key is valid
// (lab E11, OpenSSH on Debian 12).
func hostKeyAlgorithms(knownHosts string) ([]string, error) {
	var out []string
	for _, line := range strings.Split(knownHosts, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, _, key, _, _, err := ssh.ParseKnownHosts([]byte(line))
		if err != nil {
			return nil, err
		}
		var algos []string
		switch key.Type() {
		case ssh.KeyAlgoRSA:
			algos = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
		default:
			algos = []string{key.Type()}
		}
		for _, a := range algos {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no host key")
	}
	return out, nil
}
