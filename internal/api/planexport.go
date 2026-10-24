// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Plan export (R6 "export as code"): every plan endpoint answers JSON by
// default and YAML with ?format=yaml. Both carry the same document,
// including the expert view (agentSteps). The YAML is produced from the
// JSON encoding without a YAML library: block maps and sequences in the
// JSON key order, strings as JSON strings (valid YAML double-quoted
// scalars), so no value can change the structure.

// planFormat returns "json" or "yaml"; ok is false for anything else.
func planFormat(r *http.Request) (string, bool) {
	switch f := r.URL.Query().Get("format"); f {
	case "", "json":
		return "json", true
	case "yaml":
		return "yaml", true
	default:
		return "", false
	}
}

// writePlanDoc writes v in the requested format.
func writePlanDoc(w http.ResponseWriter, r *http.Request, status int, v any) {
	f, ok := planFormat(r)
	if !ok {
		writeError(w, http.StatusBadRequest, errBadRequest)
		return
	}
	if f == "json" {
		writeJSON(w, status, v)
		return
	}
	raw, err := json.Marshal(v)
	if err == nil {
		var y string
		if y, err = JSONToYAML(raw); err == nil {
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(y))
			return
		}
	}
	writeError(w, http.StatusInternalServerError, errInternal)
}

type yval struct {
	scalar *string
	keys   []string
	vals   []*yval
	isMap  bool
	items  []*yval
	isSeq  bool
}

// JSONToYAML converts one JSON document into deterministic block YAML.
func JSONToYAML(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := yparse(dec)
	if err != nil {
		return "", err
	}
	if dec.More() {
		return "", fmt.Errorf("api: trailing JSON data")
	}
	var b strings.Builder
	switch {
	case v.scalar != nil:
		b.WriteString(*v.scalar + "\n")
	case (v.isMap && len(v.keys) == 0) || (v.isSeq && len(v.items) == 0):
		b.WriteString(map[bool]string{true: "{}\n", false: "[]\n"}[v.isMap])
	default:
		v.write(&b, 0)
	}
	return b.String(), nil
}

func yparse(dec *json.Decoder) (*yval, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			v := &yval{isMap: true}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("api: object key %v", kt)
				}
				sub, err := yparse(dec)
				if err != nil {
					return nil, err
				}
				v.keys, v.vals = append(v.keys, k), append(v.vals, sub)
			}
			_, err := dec.Token()
			return v, err
		case '[':
			v := &yval{isSeq: true}
			for dec.More() {
				sub, err := yparse(dec)
				if err != nil {
					return nil, err
				}
				v.items = append(v.items, sub)
			}
			_, err := dec.Token()
			return v, err
		}
		return nil, fmt.Errorf("api: unexpected %v", t)
	case string:
		q, _ := json.Marshal(t)
		s := string(q)
		return &yval{scalar: &s}, nil
	case json.Number:
		s := t.String()
		return &yval{scalar: &s}, nil
	case bool:
		s := fmt.Sprint(t)
		return &yval{scalar: &s}, nil
	case nil:
		s := "null"
		return &yval{scalar: &s}, nil
	}
	return nil, fmt.Errorf("api: unexpected token %v", tok)
}

func ykey(k string) string {
	plain := k != ""
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			plain = false
			break
		}
	}
	if plain && k[0] != '-' {
		return k
	}
	q, _ := json.Marshal(k)
	return string(q)
}

func (v *yval) inline() (string, bool) {
	switch {
	case v.scalar != nil:
		return *v.scalar, true
	case v.isMap && len(v.keys) == 0:
		return "{}", true
	case v.isSeq && len(v.items) == 0:
		return "[]", true
	}
	return "", false
}

func (v *yval) write(b *strings.Builder, indent int) {
	pad := strings.Repeat("  ", indent)
	if v.isMap {
		for i, k := range v.keys {
			b.WriteString(pad + ykey(k) + ":")
			if s, ok := v.vals[i].inline(); ok {
				b.WriteString(" " + s + "\n")
				continue
			}
			b.WriteString("\n")
			v.vals[i].write(b, indent+1)
		}
		return
	}
	for _, it := range v.items {
		if s, ok := it.inline(); ok {
			b.WriteString(pad + "- " + s + "\n")
			continue
		}
		var sub strings.Builder
		it.write(&sub, indent+1)
		b.WriteString(pad + "- " + strings.TrimPrefix(sub.String(), strings.Repeat("  ", indent+1)))
	}
}
