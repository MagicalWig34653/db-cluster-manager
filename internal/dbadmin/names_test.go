// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"errors"
	"strings"
	"testing"
)

var maliciousNames = []string{
	"", "a`b", "a`; DROP DATABASE x; --", "a'b", `a"b`, "a;b", "a--b c", "a/*x*/b", "a#b",
	"a\x00b", "a b", "a\nb", "a\\b", "-rf", "-", "аpp", // Cyrillic a
	"app​", "｀x", // zero width space, fullwidth backtick
	"ｅxample", "名前", strings.Repeat("a", 65), strings.Repeat("a", 5000), "a=b", "a@b", "a(b)", "a*",
}

func TestMaliciousDatabaseNamesRejected(t *testing.T) {
	for _, n := range maliciousNames {
		if err := ValidateDatabaseName(n); !errors.Is(err, ErrInvalid) {
			t.Errorf("database name %q accepted", n)
		}
		if _, err := QuoteIdent(n); err == nil {
			t.Errorf("QuoteIdent(%q) accepted", n)
		}
	}
}

func TestMaliciousUserAndHostRejected(t *testing.T) {
	for _, n := range maliciousNames {
		if n == "a*" || n == strings.Repeat("a", 65) {
			continue
		}
		if n != "a@b" && n != "a(b)" && ValidateUserName(n) == nil {
			t.Errorf("user %q accepted", n)
		}
		if _, err := QuoteAccount(n, "%"); err == nil && n != "a.b" {
			t.Errorf("QuoteAccount user %q accepted", n)
		}
		if n != "-" && n != "-rf" && n != "" && ValidateHost(n) == nil && !strings.HasPrefix(n, "a") {
			t.Errorf("host %q accepted", n)
		}
	}
	for _, h := range []string{"", "a'b", "a b", "%'; --", "h\x00", "а.example", strings.Repeat("h", 256), "a\\b", "a`b", "-x"} {
		if ValidateHost(h) == nil {
			t.Errorf("host %q accepted", h)
		}
	}
	for _, u := range []string{"a@b", "a(b)", "a/b", "a,b"} {
		if ValidateUserName(u) == nil {
			t.Errorf("user %q accepted", u)
		}
	}
}

func TestValidNamesQuoted(t *testing.T) {
	q, err := QuoteIdent("shop_db-1$")
	if err != nil || q != "`shop_db-1$`" {
		t.Fatalf("got %q %v", q, err)
	}
	for _, h := range []string{"%", "10.0.%", "db-1.example.org", "::1", "192.168.1.5", "localhost"} {
		a, err := QuoteAccount("app.user", h)
		if err != nil || a != "'app.user'@'"+h+"'" {
			t.Errorf("host %q: %q %v", h, a, err)
		}
	}
}

func TestQuoteHelpersEscapeDefenseInDepth(t *testing.T) {
	if got := quoteIdentRaw("a`b"); got != "`a``b`" {
		t.Error(got)
	}
	if got := quoteStringRaw(`a'b\c`); got != `'a''b\\c'` {
		t.Error(got)
	}
}

func TestGrantDBEscapesWildcards(t *testing.T) {
	got, err := quoteGrantDB("my_db")
	if err != nil || got != "`my\\_db`" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := quoteGrantDB("my%db"); err == nil {
		t.Fatal("percent accepted")
	}
}
