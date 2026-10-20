// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestSecretNeverFormats(t *testing.T) {
	s := NewSecret("hunter2hunter2")
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	log.Info("x", "pw", s)
	j, _ := json.Marshal(struct{ P Secret }{s})
	out := fmt.Sprintf("%v|%+v|%#v|%s|%q|%x|%s|%s", s, s, s, s, s, s, buf.String(), j)
	if strings.Contains(out, "hunter2") {
		t.Fatalf("leak: %s", out)
	}
	if s.Reveal() != "hunter2hunter2" {
		t.Fatal("reveal")
	}
}

func TestGeneratePasswordDefaults(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := GeneratePassword(PasswordOptions{})
		if err != nil {
			t.Fatal(err)
		}
		v := p.Reveal()
		if len(v) != 24 || seen[v] {
			t.Fatalf("bad or duplicate password %d", len(v))
		}
		seen[v] = true
		if err := ValidatePassword(v); err != nil {
			t.Fatal(err)
		}
		if !strings.ContainsAny(v, lower) || !strings.ContainsAny(v, upper) || !strings.ContainsAny(v, digit) {
			t.Fatalf("missing class in %q", v)
		}
	}
}

func TestGeneratePasswordOptions(t *testing.T) {
	p, err := GeneratePassword(PasswordOptions{Length: 40, Charset: "ab01XY-_"})
	if err != nil || len(p.Reveal()) != 40 || strings.Trim(p.Reveal(), "ab01XY-_") != "" {
		t.Fatalf("%v %q", err, p.Reveal())
	}
	bad := []PasswordOptions{
		{Length: 5}, {Length: 500}, {Charset: "abcdefgh'"}, {Charset: "abcdefg\\h"}, {Charset: "abc"},
		{Charset: "aabbccdd"}, {Charset: "abcdefg\x00"}, {Charset: "abcdefgé"},
	}
	for _, o := range bad {
		if _, err := GeneratePassword(o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
}

func TestGeneratePasswordDeterministicSource(t *testing.T) {
	// Bytes >= 256 - 256%n are skipped (no modulo bias): n=8 divides 256, none skipped.
	src := bytes.NewReader(bytes.Repeat([]byte{0, 1, 2, 3, 4, 5, 6, 7}, 100))
	p, err := GeneratePassword(PasswordOptions{Length: 16, Charset: "abcdefgh", Rand: src})
	if err != nil || p.Reveal() != "abcdefghabcdefgh" {
		t.Fatalf("%q %v", p.Reveal(), err)
	}
	if _, err := GeneratePassword(PasswordOptions{Rand: bytes.NewReader(nil)}); err == nil {
		t.Fatal("short random source accepted")
	}
}

func TestValidatePassword(t *testing.T) {
	for _, p := range []string{"short", "has'quote123", `back\slash123`, "has space 123", "tick`tick123", "nul\x00nul1234", "ünïcödé1234", `dq"dq123456`, strings.Repeat("a", 129)} {
		if ValidatePassword(p) == nil {
			t.Errorf("%q accepted", p)
		}
	}
	if err := ValidatePassword("Abc123!#$%&()*+,-./:;<=>?@[]^_{|}~"); err != nil {
		t.Error(err)
	}
}
