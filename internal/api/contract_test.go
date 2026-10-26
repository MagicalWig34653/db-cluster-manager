// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
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
	for _, r := range ManagedRuntimeRoutes() {
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

// TestHostSchemaListsEveryField keeps the Host schema of api/openapi.yaml in
// step with the JSON fields of api.Host.
func TestHostSchemaListsEveryField(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	start := slices.Index(lines, "    Host:")
	if start < 0 {
		t.Fatal("schema Host not found")
	}
	var body []string
	for _, l := range lines[start+1:] {
		if strings.HasPrefix(l, "    ") && !strings.HasPrefix(l, "     ") && strings.HasSuffix(l, ":") {
			break
		}
		body = append(body, l)
	}
	spec := strings.Join(body, "\n")
	rt := reflect.TypeOf(Host{})
	for i := range rt.NumField() {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name != "" && !strings.Contains(spec, "\n        "+name+":") {
			t.Errorf("api.Host field %q is missing from the Host schema", name)
		}
	}
	// The new fields serialize under their documented names.
	b, _ := json.Marshal(Host{AgentVersion: "v1"})
	if !strings.Contains(string(b), `"agentVersion":"v1"`) || strings.Contains(string(b), "agentSeenAt") {
		t.Errorf("host JSON: %s", b)
	}
}

// TestServiceSchemasListEveryField keeps the service schemas of
// api/openapi.yaml in step with the JSON fields of their Go types.
func TestServiceSchemasListEveryField(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for schema, v := range map[string]any{
		"ClusterOperationInput": galera.OperationRequest{}, "ClusterOperationPlan": ClusterOperationPlan{},
		"ServiceInfo": ServiceInfo{}, "ServicePlan": ServicePlan{}, "ServiceStarted": ServiceStarted{},
		"ServiceHealth": ServiceHealth{}, "ServiceUpdateInput": ServiceUpdateInput{},
		"ServiceUpdateStrategy": ServiceUpdateStrategy{}, "AgentStep": AgentStep{}, "ServiceInput": ServiceInput{},
	} {
		start := slices.Index(lines, "    "+schema+":")
		if start < 0 {
			t.Errorf("schema %s not found", schema)
			continue
		}
		var body []string
		for _, l := range lines[start+1:] {
			if strings.HasPrefix(l, "    ") && !strings.HasPrefix(l, "     ") && strings.HasSuffix(l, ":") {
				break
			}
			body = append(body, l)
		}
		spec := strings.Join(body, "\n")
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
			if name != "" && !strings.Contains(spec, "\n        "+name+":") {
				t.Errorf("api.%s field %q is missing from the %s schema", rt.Name(), name, schema)
			}
		}
	}
}
