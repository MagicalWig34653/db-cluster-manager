// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const (
	uuidA = "559e29b2-0000-4000-8000-000000000001"
	uuidB = "ca05d274-0000-4000-8000-000000000002"
)

func testIdentity() Identity {
	return Identity{InstallationID: "inst-1", ClusterID: "cluster-1", NodeUUID: uuidA}
}

func writeRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeMarker(t *testing.T, root string, id Identity) {
	t.Helper()
	b, err := json.Marshal(markerFile{Format: markerFormat, Identity: id})
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, filepath.Join(root, MarkerFileName), string(b))
}

// TestVerifyMatrix mirrors the decision matrix of tests/p0/marker-guard.sh
// recorded in tests/p0/results/e2-host-loss.md, step 4.
func TestVerifyMatrix(t *testing.T) {
	exp := testIdentity()
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
		exp   Identity
		want  error // nil means allow
	}{
		{"matching marker, empty datadir", func(t *testing.T, r string) {
			writeMarker(t, r, exp)
			_ = os.MkdirAll(filepath.Join(r, "data"), 0o755)
		}, exp, nil},
		{"matching marker, non-empty datadir", func(t *testing.T, r string) {
			writeMarker(t, r, exp)
			writeRaw(t, filepath.Join(r, "data", "ibdata1"), "x")
		}, exp, nil},
		{"lab marker with extra node field", func(t *testing.T, r string) {
			writeRaw(t, filepath.Join(r, MarkerFileName),
				`{"node_uuid":"`+uuidA+`","installation_id":"inst-1","cluster_id":"cluster-1","node":"galera-3"}`)
		}, exp, nil},
		{"foreign node_uuid", func(t *testing.T, r string) {
			id := exp
			id.NodeUUID = uuidB
			writeMarker(t, r, id)
		}, exp, ErrIdentityMismatch},
		{"foreign installation", func(t *testing.T, r string) {
			id := exp
			id.InstallationID = "other"
			writeMarker(t, r, id)
		}, exp, ErrForeignMarker},
		{"foreign cluster", func(t *testing.T, r string) {
			id := exp
			id.ClusterID = "other"
			writeMarker(t, r, id)
		}, exp, ErrForeignMarker},
		{"marker missing a field", func(t *testing.T, r string) {
			writeRaw(t, filepath.Join(r, MarkerFileName), `{"node_uuid":"`+uuidA+`","installation_id":"inst-1"}`)
		}, exp, ErrMarkerCorrupt},
		{"marker not JSON", func(t *testing.T, r string) {
			writeRaw(t, filepath.Join(r, MarkerFileName), "garbage")
		}, exp, ErrMarkerCorrupt},
		{"marker is a directory", func(t *testing.T, r string) {
			_ = os.MkdirAll(filepath.Join(r, MarkerFileName), 0o755)
		}, exp, ErrMarkerCorrupt},
		{"marker is a symlink", func(t *testing.T, r string) {
			other := filepath.Join(t.TempDir(), "m.json")
			writeMarker(t, filepath.Dir(other), exp)
			if err := os.Symlink(filepath.Join(filepath.Dir(other), MarkerFileName), filepath.Join(r, MarkerFileName)); err != nil {
				t.Fatal(err)
			}
		}, exp, ErrMarkerCorrupt},
		{"marker missing, non-empty datadir", func(t *testing.T, r string) {
			writeRaw(t, filepath.Join(r, "data", "ibdata1"), "x")
		}, exp, ErrUnknownData},
		{"marker missing, file at volume root", func(t *testing.T, r string) {
			writeRaw(t, filepath.Join(r, "stray"), "x")
		}, exp, ErrUnknownData},
		{"marker missing, empty datadir", func(t *testing.T, r string) {
			_ = os.MkdirAll(filepath.Join(r, "data", "sub"), 0o755)
			_ = os.MkdirAll(filepath.Join(r, lostAndFound), 0o755)
			writeRaw(t, filepath.Join(r, lostAndFound, "#123"), "x")
		}, exp, ErrEmptyVolume},
		{"marker missing, only leftover temp marker", func(t *testing.T, r string) {
			writeRaw(t, filepath.Join(r, ".dbcm-identity-123.tmp"), "partial")
		}, exp, ErrEmptyVolume},
		{"expected identity not configured", func(t *testing.T, r string) {
			writeMarker(t, r, exp)
		}, Identity{InstallationID: "inst-1", ClusterID: "cluster-1"}, ErrIdentityNotConfigured},
		{"expected node uuid malformed", func(t *testing.T, r string) {
			writeMarker(t, r, exp)
		}, Identity{InstallationID: "inst-1", ClusterID: "cluster-1", NodeUUID: "not-a-uuid"}, ErrIdentityNotConfigured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			err := Verify(root, tc.exp)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Verify: want allow, got %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify: want %v, got %v", tc.want, err)
			}
			var ie *IdentityError
			if !errors.As(err, &ie) {
				t.Fatalf("Verify: error %T is not *IdentityError", err)
			}
			for _, other := range []error{ErrIdentityNotConfigured, ErrForeignMarker, ErrIdentityMismatch,
				ErrMarkerCorrupt, ErrUnknownData, ErrEmptyVolume} {
				if other != tc.want && errors.Is(err, other) {
					t.Fatalf("error matches %v as well as %v", other, tc.want)
				}
			}
		})
	}
}

func TestVerifyMissingVolumeRootIsEmpty(t *testing.T) {
	err := Verify(filepath.Join(t.TempDir(), "absent"), testIdentity())
	if !errors.Is(err, ErrEmptyVolume) {
		t.Fatalf("want ErrEmptyVolume, got %v", err)
	}
}

func TestInitializeEmptyVolume(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "data"), 0o755)
	writeRaw(t, filepath.Join(root, ".dbcm-identity-old.tmp"), "partial")
	tok := NewIdentityToken{TokenID: "tok-1", Identity: testIdentity()}
	if err := Initialize(root, tok); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := Verify(root, tok.Identity); err != nil {
		t.Fatalf("Verify after Initialize: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(root, markerTempPattern)); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
	st, err := os.Stat(filepath.Join(root, MarkerFileName))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("marker mode %v", st.Mode().Perm())
	}
	// Idempotent for the same identity.
	if err := Initialize(root, tok); err != nil {
		t.Fatalf("second Initialize: %v", err)
	}
}

func TestInitializeRefusals(t *testing.T) {
	tok := NewIdentityToken{TokenID: "tok-1", Identity: testIdentity()}
	t.Run("volume with data", func(t *testing.T) {
		root := t.TempDir()
		writeRaw(t, filepath.Join(root, "data", "ibdata1"), "x")
		if err := Initialize(root, tok); !errors.Is(err, ErrUnknownData) {
			t.Fatalf("want ErrUnknownData, got %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, MarkerFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("marker written on refused volume")
		}
	})
	t.Run("other node marker", func(t *testing.T) {
		root := t.TempDir()
		id := testIdentity()
		id.NodeUUID = uuidB
		writeMarker(t, root, id)
		if err := Initialize(root, tok); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("want ErrIdentityMismatch, got %v", err)
		}
	})
	t.Run("foreign marker", func(t *testing.T) {
		root := t.TempDir()
		id := testIdentity()
		id.InstallationID = "other"
		writeMarker(t, root, id)
		if err := Initialize(root, tok); !errors.Is(err, ErrForeignMarker) {
			t.Fatalf("want ErrForeignMarker, got %v", err)
		}
	})
	t.Run("incomplete token", func(t *testing.T) {
		if err := Initialize(t.TempDir(), NewIdentityToken{Identity: testIdentity()}); !errors.Is(err, ErrIdentityNotConfigured) {
			t.Fatalf("want ErrIdentityNotConfigured, got %v", err)
		}
	})
	t.Run("missing root", func(t *testing.T) {
		if err := Initialize(filepath.Join(t.TempDir(), "absent"), tok); err == nil {
			t.Fatal("want error for missing volume root")
		}
	})
}

func TestIdentityErrorMessage(t *testing.T) {
	root := t.TempDir()
	id := testIdentity()
	id.InstallationID = "other"
	writeMarker(t, root, id)
	err := Verify(root, testIdentity())
	if err == nil || err.Error() == "" {
		t.Fatal("want descriptive error")
	}
	var ie *IdentityError
	if !errors.As(err, &ie) || ie.Found == nil || ie.Found.InstallationID != "other" {
		t.Fatalf("Found not reported: %+v", ie)
	}
}
