// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"context"
	"errors"
	"strings"
	"sync"
)

type call struct {
	SQL  string
	Args []any
}

// fakeExec records statements and serves canned query results.
type fakeExec struct {
	mu       sync.Mutex
	execs    []call
	queries  []call
	users    map[string]bool     // "user@host" -> exists
	grants   map[string][]string // "user@host" -> SHOW GRANTS lines
	failOn   string              // substring: Exec fails when SQL contains it
	failWith error
}

func newFake() *fakeExec {
	return &fakeExec{users: map[string]bool{}, grants: map[string][]string{}}
}

func (f *fakeExec) Exec(_ context.Context, stmt string, args ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, call{stmt, args})
	if f.failOn != "" && strings.Contains(stmt, f.failOn) {
		if f.failWith != nil {
			return f.failWith
		}
		return errors.New("ERROR 1064: failed near " + stmt)
	}
	return nil
}

func (f *fakeExec) Query(_ context.Context, stmt string, args ...any) ([][]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, call{stmt, args})
	switch {
	case strings.HasPrefix(stmt, "SELECT COUNT(*) FROM mysql.user"):
		if f.users[args[0].(string)+"@"+args[1].(string)] {
			return [][]string{{"1"}}, nil
		}
		return [][]string{{"0"}}, nil
	case strings.HasPrefix(stmt, "SHOW GRANTS FOR "):
		acct := strings.ReplaceAll(strings.TrimPrefix(stmt, "SHOW GRANTS FOR "), "'", "")
		var rows [][]string
		for _, l := range f.grants[acct] {
			rows = append(rows, []string{l})
		}
		if rows == nil {
			return nil, errors.New("ERROR 1141: There is no such grant")
		}
		return rows, nil
	}
	return nil, nil
}

func (f *fakeExec) sqls() []string {
	var out []string
	for _, c := range f.execs {
		out = append(out, c.SQL)
	}
	return out
}

type fakeWriter struct{ node string }

func (w *fakeWriter) CurrentWriter(context.Context) (string, error) { return w.node, nil }

type fakeSink struct {
	got  map[SecretRef]string
	fail error
}

func (s *fakeSink) Store(_ context.Context, ref SecretRef, v Secret) error {
	if s.fail != nil {
		return s.fail
	}
	if s.got == nil {
		s.got = map[SecretRef]string{}
	}
	s.got[ref] = v.Reveal()
	return nil
}

func newAdmin() (*Admin, *fakeExec, *fakeSink, *fakeWriter) {
	f, s, w := newFake(), &fakeSink{}, &fakeWriter{node: "n1"}
	return &Admin{Exec: f, NodeID: "n1", Writer: w, Sink: s}, f, s, w
}
