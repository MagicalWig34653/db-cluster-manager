// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true, "patch": true, "head": true, "options": true, "trace": true,
}

// openAPIOperations extracts "METHOD /path" pairs from the paths section of
// an OpenAPI YAML file without a YAML library. It relies on the file's
// layout: path keys indented by two spaces under a top-level "paths:", and
// operation keys indented by four spaces under each path.
func openAPIOperations(t *testing.T, file string) []string {
	t.Helper()
	fh, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var ops []string
	inPaths := false
	current := ""
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			inPaths = line == "paths:"
			current = ""
			continue
		}
		if !inPaths {
			continue
		}
		switch {
		case indent == 2 && strings.HasPrefix(trimmed, "/") && strings.HasSuffix(trimmed, ":"):
			current = strings.TrimSuffix(trimmed, ":")
		case indent == 4 && current != "" && strings.HasSuffix(trimmed, ":"):
			m := strings.TrimSuffix(trimmed, ":")
			if httpMethods[m] {
				ops = append(ops, strings.ToUpper(m)+" "+current)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return ops
}

func TestRoutesMatchOpenAPI(t *testing.T) {
	spec := openAPIOperations(t, filepath.Join("..", "..", "api", "openapi.yaml"))
	if len(spec) == 0 {
		t.Fatal("no operations found in api/openapi.yaml")
	}
	var code []string
	for _, r := range Routes() {
		code = append(code, r.Method+" "+r.Path)
	}
	for _, op := range code {
		if !slices.Contains(spec, op) {
			t.Errorf("route %s is registered but missing from api/openapi.yaml", op)
		}
	}
	for _, op := range spec {
		if !slices.Contains(code, op) {
			t.Errorf("operation %s is documented in api/openapi.yaml but not registered", op)
		}
	}
	seen := map[string]bool{}
	for _, op := range code {
		if seen[op] {
			t.Errorf("route %s registered twice", op)
		}
		seen[op] = true
	}
}

func TestOpenAPIParser(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.yaml")
	src := "openapi: 3.1.0\npaths:\n  /a:\n    get:\n      responses: {}\n    parameters: []\n  /b/{id}:\n    post:\n      summary: x\ncomponents:\n  schemas:\n    /notapath:\n      get: {}\n"
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got := openAPIOperations(t, p)
	if !slices.Equal(got, []string{"GET /a", "POST /b/{id}"}) {
		t.Fatalf("ops = %v", got)
	}
}
