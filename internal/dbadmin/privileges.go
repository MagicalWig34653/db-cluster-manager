// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"sort"
	"strings"
)

// Privilege is a database-level MariaDB privilege from the allowlist.
// Global privileges (SUPER, FILE, PROCESS, ...) are deliberately absent.
type Privilege string

// canonicalPrivileges defines the allowlist and the canonical output order.
var canonicalPrivileges = []Privilege{
	"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER", "INDEX",
	"REFERENCES", "CREATE TEMPORARY TABLES", "LOCK TABLES", "EXECUTE",
	"CREATE VIEW", "SHOW VIEW", "CREATE ROUTINE", "ALTER ROUTINE", "EVENT",
	"TRIGGER", "DELETE HISTORY",
}

var privilegeRank = func() map[Privilege]int {
	m := map[Privilege]int{}
	for i, p := range canonicalPrivileges {
		m[p] = i
	}
	return m
}()

// ParsePrivilege normalizes and validates a privilege name.
func ParsePrivilege(s string) (Privilege, error) {
	p := Privilege(strings.Join(strings.Fields(strings.ToUpper(s)), " "))
	if _, ok := privilegeRank[p]; !ok {
		return "", invalid("privilege", "%s is not an allowed database-level privilege", safeQuote(s))
	}
	return p, nil
}

// Preset names a predefined privilege set.
type Preset string

const (
	PresetReadOnly  Preset = "read-only"
	PresetReadWrite Preset = "read-write"
	PresetAdmin     Preset = "admin" // ALL PRIVILEGES on one database
	PresetCustom    Preset = "custom"
)

var presetPrivileges = map[Preset][]Privilege{
	PresetReadOnly:  {"SELECT", "SHOW VIEW"},
	PresetReadWrite: {"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE TEMPORARY TABLES", "LOCK TABLES", "EXECUTE", "SHOW VIEW"},
}

// PrivilegeSet selects privileges on a single database.
type PrivilegeSet struct {
	Preset      Preset
	Custom      []Privilege // only for PresetCustom
	GrantOption bool        // WITH GRANT OPTION
}

// Resolve returns the canonical privilege list, or all=true for the admin
// preset (rendered as ALL PRIVILEGES).
func (s PrivilegeSet) Resolve() (privs []Privilege, all bool, err error) {
	switch s.Preset {
	case PresetAdmin:
		if len(s.Custom) > 0 {
			return nil, false, invalid("privilege set", "admin preset takes no custom privileges")
		}
		return nil, true, nil
	case PresetReadOnly, PresetReadWrite:
		if len(s.Custom) > 0 {
			return nil, false, invalid("privilege set", "%s preset takes no custom privileges", s.Preset)
		}
		return append([]Privilege(nil), presetPrivileges[s.Preset]...), false, nil
	case PresetCustom:
		if len(s.Custom) == 0 {
			return nil, false, invalid("privilege set", "custom set is empty")
		}
		seen := map[Privilege]bool{}
		for _, p := range s.Custom {
			n, err := ParsePrivilege(string(p))
			if err != nil {
				return nil, false, err
			}
			if !seen[n] {
				seen[n] = true
				privs = append(privs, n)
			}
		}
		sort.Slice(privs, func(i, j int) bool { return privilegeRank[privs[i]] < privilegeRank[privs[j]] })
		return privs, false, nil
	}
	return nil, false, invalid("privilege set", "unknown preset %s", safeQuote(string(s.Preset)))
}

func joinPrivs(p []Privilege) string {
	s := make([]string, len(p))
	for i := range p {
		s[i] = string(p[i])
	}
	return strings.Join(s, ", ")
}
