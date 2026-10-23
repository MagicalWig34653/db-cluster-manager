// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// A minimal, deterministic YAML writer for generated configuration. The
// repository has no YAML dependency and the output only needs block maps,
// block sequences and scalars. Strings are written as JSON strings, which
// are valid YAML double-quoted scalars, so no value can break the structure.

// ynode is a YAML value: exactly one of scalar, mapping and seq is used.
type ynode struct {
	scalar  *string // already rendered
	mapping []ykv
	seq     []*ynode
}

type ykv struct {
	key string
	val *ynode
}

func yString(s string) *ynode {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	r := strings.TrimSuffix(b.String(), "\n")
	return &ynode{scalar: &r}
}

func yInt(i int) *ynode   { s := strconv.Itoa(i); return &ynode{scalar: &s} }
func yBool(b bool) *ynode { s := strconv.FormatBool(b); return &ynode{scalar: &s} }

func yStrings(in []string) *ynode {
	n := &ynode{seq: []*ynode{}}
	for _, s := range in {
		n.seq = append(n.seq, yString(s))
	}
	return n
}

func yMap() *ynode { return &ynode{mapping: []ykv{}} }

// set appends a key; keys keep their insertion order, so callers insert in
// a fixed order.
func (n *ynode) set(key string, v *ynode) *ynode {
	n.mapping = append(n.mapping, ykv{key, v})
	return n
}

var plainKey = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

func yKey(k string) string {
	if plainKey.MatchString(k) {
		return k
	}
	return *yString(k).scalar
}

// render writes the node at the top level.
func (n *ynode) render() string {
	var b strings.Builder
	n.write(&b, 0)
	return b.String()
}

func (n *ynode) write(b *strings.Builder, indent int) {
	pad := strings.Repeat("  ", indent)
	switch {
	case n.mapping != nil:
		for _, kv := range n.mapping {
			b.WriteString(pad + yKey(kv.key) + ":")
			kv.val.writeValue(b, indent)
		}
	case n.seq != nil:
		for _, e := range n.seq {
			b.WriteString(pad + "-")
			e.writeItem(b, indent)
		}
	}
}

// writeValue writes the value after "key:".
func (n *ynode) writeValue(b *strings.Builder, indent int) {
	switch {
	case n.scalar != nil:
		b.WriteString(" " + *n.scalar + "\n")
	case n.mapping != nil && len(n.mapping) == 0, n.seq != nil && len(n.seq) == 0:
		if n.mapping != nil {
			b.WriteString(" {}\n")
		} else {
			b.WriteString(" []\n")
		}
	default:
		b.WriteString("\n")
		n.write(b, indent+1)
	}
}

// writeItem writes the value after "-".
func (n *ynode) writeItem(b *strings.Builder, indent int) {
	switch {
	case n.scalar != nil:
		b.WriteString(" " + *n.scalar + "\n")
	case n.mapping != nil && len(n.mapping) == 0:
		b.WriteString(" {}\n")
	case n.seq != nil && len(n.seq) == 0:
		b.WriteString(" []\n")
	case n.mapping != nil:
		// First key on the dash line: "- key: value".
		var sub strings.Builder
		n.write(&sub, indent+1)
		b.WriteString(" " + strings.TrimPrefix(sub.String(), strings.Repeat("  ", indent+1)))
	default:
		b.WriteString("\n")
		n.write(b, indent+1)
	}
}
