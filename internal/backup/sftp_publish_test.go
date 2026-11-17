// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"errors"
	"io"
	"testing"

	"github.com/pkg/sftp"
)

type publishProbe struct {
	posixErr                 error
	renameCalls, removeCalls int
	old                      string
}

func (p *publishProbe) PosixRename(string, string) error { return p.posixErr }
func (p *publishProbe) Rename(string, string) error      { p.renameCalls++; return io.EOF }
func (p *publishProbe) Remove(string) error              { p.removeCalls++; p.old = ""; return nil }

func TestSFTPPublishPreservesExistingObject(t *testing.T) {
	for _, err := range []error{io.EOF, &sftp.StatusError{Code: 8}, errors.New("permission denied")} {
		p := &publishProbe{posixErr: err, old: "previous backup"}
		if err := publishSFTP(p, "tmp", "destination"); err == nil {
			t.Fatal("replacement unexpectedly succeeded")
		}
		if p.old != "previous backup" || p.removeCalls != 0 || p.renameCalls != 0 {
			t.Fatalf("unsafe fallback: %+v", p)
		}
	}
}
