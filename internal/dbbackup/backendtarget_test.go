// SPDX-License-Identifier: AGPL-3.0-only

package dbbackup

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
)

func encSection(id string, fill byte) string {
	return fmt.Sprintf(`"encryption":{"key_id":%q,"key":%q}`, id, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{fill}, 32)))
}

func TestLocalTargetWithClientSideEncryption(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`{"type":"local","dir":%q,%s}`, dir, encSection("dk1", 7))
	c, err := ParseBackendConfig([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	be, err := c.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Put(ctx, "x.manifest.json", strings.NewReader(`{"secret":"CLEARTEXT-MARKER"}`)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "x.manifest.json"))
	if bytes.Contains(raw, []byte("CLEARTEXT-MARKER")) {
		t.Fatal("the directory holds plaintext")
	}
	got, err := readAllBackend(t, be, "x.manifest.json")
	if err != nil || !strings.Contains(string(got), "CLEARTEXT-MARKER") {
		t.Fatalf("read back: %q %v", got, err)
	}
	// Another key cannot read it; rotated config with Previous can.
	other, _ := ParseBackendConfig([]byte(fmt.Sprintf(`{"type":"local","dir":%q,%s}`, dir, encSection("dk2", 9))))
	ob, _ := other.Open()
	if _, err := ob.Get(ctx, "x.manifest.json"); !errors.Is(err, backup.ErrUnknownKey) {
		t.Fatalf("other key: %v", err)
	}
	rot := fmt.Sprintf(`{"type":"local","dir":%q,"encryption":{"key_id":"dk2","key":%q,"previous":{"dk1":%q}}}`, dir,
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)), base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	rc, _ := ParseBackendConfig([]byte(rot))
	rb, _ := rc.Open()
	if got, err := readAllBackend(t, rb, "x.manifest.json"); err != nil || !strings.Contains(string(got), "CLEARTEXT-MARKER") {
		t.Fatalf("rotated read: %q %v", got, err)
	}
}

func TestBackendConfigEncryptionValidation(t *testing.T) {
	for name, js := range map[string]string{
		"short key":   `{"type":"local","dir":"/x","encryption":{"key_id":"a","key":"AAAA"}}`,
		"no key id":   `{"type":"local","dir":"/x","encryption":{"key":"` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `"}}`,
		"not base64":  `{"type":"local","dir":"/x","encryption":{"key_id":"a","key":"%%%"}}`,
		"bad prev":    `{"type":"local","dir":"/x","encryption":{"key_id":"a","key":"` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `","previous":{"b":"AA"}}}`,
		"unknown key": `{"type":"local","dir":"/x","encryption":{"key_id":"a","key":"x","cipher":"rot13"}}`,
	} {
		if _, err := ParseBackendConfig([]byte(js)); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Retention works through the encrypted wrapper: the manifest goes first,
// the stream second, and the kept set stays readable.
func TestRetentionThroughEncryptedTarget(t *testing.T) {
	ctx := context.Background()
	keys := backup.StaticKeys{CurrentID: "dk", Keys: map[string][]byte{"dk": bytes.Repeat([]byte{3}, 32)}}
	dir, err := backup.NewDirBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	be := &backup.EncryptedBackend{Inner: dir, Keys: keys}
	var sets []BackupSet
	for i := range 3 {
		obj := fmt.Sprintf("c-b%d.xbs.dbcms", i)
		for _, n := range []string{obj, ManifestName(obj)} {
			if err := be.Put(ctx, n, strings.NewReader("data "+n)); err != nil {
				t.Fatal(err)
			}
		}
		sets = append(sets, BackupSet{BackupID: fmt.Sprintf("b%d", i), ClusterID: "c", Created: t0.Add(time.Duration(i) * time.Hour), Objects: []string{obj}})
	}
	keep, remove := RetentionSelect(sets, backup.Policy{KeepLast: 1})
	if len(keep) != 1 || len(remove) != 2 || keep[0].BackupID != "b2" {
		t.Fatalf("keep %v remove %v", keep, remove)
	}
	if err := ApplyRetention(ctx, be, remove); err != nil {
		t.Fatal(err)
	}
	left, _ := be.List(ctx, "")
	if len(left) != 2 {
		t.Fatalf("left %+v", left)
	}
	if got, err := readAllBackend(t, be, "c-b2.xbs.dbcms"); err != nil || string(got) != "data c-b2.xbs.dbcms" {
		t.Fatalf("kept object: %q %v", got, err)
	}
}
