// SPDX-License-Identifier: AGPL-3.0-only

package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Proposal is the AI's structured answer after strict parsing. It is
// still untrusted: the caller validates every field against the finding
// and the typed allow-lists before anything becomes a plan. Rationale is
// display-only text and is never interpolated into a recipe.
type Proposal struct {
	Kind          string      `json:"kind"`
	Component     string      `json:"component"`
	TargetVersion string      `json:"targetVersion,omitempty"`
	Mitigation    *Mitigation `json:"mitigation,omitempty"`
	Affected      string      `json:"affected"`
	Benefit       string      `json:"benefit"`
	Risk          string      `json:"risk"`
	Rationale     string      `json:"rationale"`
}

// Mitigation is a proposed setting change.
type Mitigation struct {
	Setting string `json:"setting"`
	Value   string `json:"value"`
}

// ErrSchema is returned when the answer does not match the schema.
var ErrSchema = errors.New("ai: answer does not match the proposal schema")

// Limits of the answer.
const (
	MaxAnswerBytes    = 16 << 10
	MaxRationaleRunes = 1500
)

func schemaErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrSchema, fmt.Sprintf(format, a...))
}

// ParseProposal parses text as exactly one JSON object of the proposal
// schema: no unknown or duplicate members, no trailing data, enumerated
// values only, kind-specific members required or forbidden. A Markdown
// code fence around the object is tolerated; any other surrounding text is
// rejected.
func ParseProposal(text string) (Proposal, error) {
	if len(text) > MaxAnswerBytes {
		return Proposal{}, schemaErr("answer too large")
	}
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return Proposal{}, schemaErr("not a single JSON object")
	}
	if err := checkDuplicateKeys([]byte(s)); err != nil {
		return Proposal{}, err
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var p Proposal
	if err := dec.Decode(&p); err != nil {
		return Proposal{}, schemaErr("%v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Proposal{}, schemaErr("trailing data")
	}
	enum := func(field, v string, allowed ...string) error {
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return schemaErr("%s must be one of %s", field, strings.Join(allowed, ", "))
	}
	for _, e := range []error{
		enum("kind", p.Kind, "patched_rebuild", "mitigation", "no_op"),
		enum("affected", p.Affected, "yes", "no", "unknown"),
		enum("benefit", p.Benefit, "high", "medium", "low", "none"),
		enum("risk", p.Risk, "high", "medium", "low"),
	} {
		if e != nil {
			return Proposal{}, e
		}
	}
	if !pkgRe.MatchString(p.Component) {
		return Proposal{}, schemaErr("component malformed")
	}
	if !utf8.ValidString(p.Rationale) || utf8.RuneCountInString(p.Rationale) > MaxRationaleRunes {
		return Proposal{}, schemaErr("rationale too long")
	}
	p.Rationale = Clean(p.Rationale)
	switch p.Kind {
	case "patched_rebuild":
		if !versionRe.MatchString(p.TargetVersion) || p.Mitigation != nil {
			return Proposal{}, schemaErr("patched_rebuild needs targetVersion and no mitigation")
		}
	case "mitigation":
		if p.Mitigation == nil || p.TargetVersion != "" || !settingRe.MatchString(p.Mitigation.Setting) ||
			len(p.Mitigation.Value) == 0 || len(p.Mitigation.Value) > 128 || strings.ContainsAny(p.Mitigation.Value, "\n\r\x00") {
			return Proposal{}, schemaErr("mitigation needs one setting and a single-line value and no targetVersion")
		}
	case "no_op":
		if p.Mitigation != nil || p.TargetVersion != "" {
			return Proposal{}, schemaErr("no_op takes neither targetVersion nor mitigation")
		}
	}
	return p, nil
}

// checkDuplicateKeys rejects objects with a repeated member name at any
// depth (a parser differential could otherwise hide a second value).
func checkDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		t, err := dec.Token()
		if err != nil {
			return schemaErr("%v", err)
		}
		d, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return schemaErr("%v", err)
				}
				k, _ := kt.(string)
				if seen[k] {
					return schemaErr("duplicate member %q", k)
				}
				seen[k] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
		}
		if err != nil {
			return schemaErr("%v", err)
		}
		return nil
	}
	return walk()
}
