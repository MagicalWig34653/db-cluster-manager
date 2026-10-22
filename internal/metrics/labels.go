// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// NameLabel is the reserved label that carries the metric name.
const NameLabel = "__name__"

// Label is one name/value pair.
type Label struct {
	Name, Value string
}

// Labels is a set of labels sorted by name, including NameLabel. A series is
// identified by its complete Labels.
type Labels []Label

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// NewLabels builds a sorted label set from a metric name and name/value pairs.
func NewLabels(name string, kv ...string) (Labels, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("%w: odd number of label arguments", ErrInvalidLabels)
	}
	l := make(Labels, 0, len(kv)/2+1)
	l = append(l, Label{NameLabel, name})
	for i := 0; i < len(kv); i += 2 {
		l = append(l, Label{kv[i], kv[i+1]})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].Name < l[j].Name })
	return l, nil
}

// MustLabels is NewLabels for tests and static definitions; it panics on error.
func MustLabels(name string, kv ...string) Labels {
	l, err := NewLabels(name, kv...)
	if err != nil {
		panic(err)
	}
	return l
}

// Get returns the value of a label or "" if it is absent.
func (l Labels) Get(name string) string {
	for _, x := range l {
		if x.Name == name {
			return x.Value
		}
	}
	return ""
}

// Name returns the metric name.
func (l Labels) Name() string { return l.Get(NameLabel) }

// Key is a canonical, unambiguous identity string. Valid labels are UTF-8, so
// the bytes 0xff and 0xfe never occur inside names or values.
func (l Labels) Key() string {
	var b strings.Builder
	for _, x := range l {
		b.WriteString(x.Name)
		b.WriteByte(0xff)
		b.WriteString(x.Value)
		b.WriteByte(0xfe)
	}
	return b.String()
}

// String renders name{a="b",c="d"} for logs and errors.
func (l Labels) String() string {
	var b strings.Builder
	b.WriteString(l.Name())
	first := true
	for _, x := range l {
		if x.Name == NameLabel {
			continue
		}
		if first {
			b.WriteByte('{')
			first = false
		} else {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", x.Name, x.Value)
	}
	if !first {
		b.WriteByte('}')
	}
	return b.String()
}

// Equal reports whether both label sets are identical.
func (l Labels) Equal(o Labels) bool {
	if len(l) != len(o) {
		return false
	}
	for i := range l {
		if l[i] != o[i] {
			return false
		}
	}
	return true
}

func (l Labels) validate(lim *Options) error {
	if len(l) == 0 || l[0].Name != NameLabel && l.Name() == "" {
		return fmt.Errorf("%w: missing metric name", ErrInvalidLabels)
	}
	if len(l) > lim.MaxLabelsPerSeries {
		return fmt.Errorf("%w: %d labels exceed the limit of %d", ErrInvalidLabels, len(l), lim.MaxLabelsPerSeries)
	}
	for i, x := range l {
		if i > 0 && l[i-1].Name >= x.Name {
			return fmt.Errorf("%w: labels not sorted or duplicate name %q", ErrInvalidLabels, x.Name)
		}
		if x.Name == NameLabel {
			if !metricNameRE.MatchString(x.Value) {
				return fmt.Errorf("%w: bad metric name %q", ErrInvalidLabels, x.Value)
			}
		} else if !labelNameRE.MatchString(x.Name) || strings.HasPrefix(x.Name, "__") {
			return fmt.Errorf("%w: bad label name %q", ErrInvalidLabels, x.Name)
		}
		if len(x.Name) > lim.MaxLabelNameLen {
			return fmt.Errorf("%w: label name %q longer than %d", ErrInvalidLabels, x.Name, lim.MaxLabelNameLen)
		}
		if len(x.Value) > lim.MaxLabelValueLen {
			return fmt.Errorf("%w: value of %q longer than %d bytes", ErrInvalidLabels, x.Name, lim.MaxLabelValueLen)
		}
		if !utf8.ValidString(x.Value) {
			return fmt.Errorf("%w: value of %q is not valid UTF-8", ErrInvalidLabels, x.Name)
		}
	}
	return nil
}

// MatchType selects the comparison of a Matcher.
type MatchType int

const (
	MatchEqual MatchType = iota
	MatchNotEqual
	MatchRegexp
	MatchNotRegexp
)

// Matcher matches one label. An absent label has the value "". Regular
// expressions are fully anchored.
type Matcher struct {
	Type  MatchType
	Name  string
	Value string
	re    *regexp.Regexp
}

// NewMatcher builds a matcher and compiles regular expressions.
func NewMatcher(t MatchType, name, value string) (Matcher, error) {
	m := Matcher{Type: t, Name: name, Value: value}
	if t == MatchRegexp || t == MatchNotRegexp {
		re, err := regexp.Compile("^(?:" + value + ")$")
		if err != nil {
			return Matcher{}, fmt.Errorf("%w: %v", ErrInvalidSelector, err)
		}
		m.re = re
	}
	return m, nil
}

// Matches reports whether labels satisfy the matcher.
func (m Matcher) Matches(l Labels) bool {
	v := l.Get(m.Name)
	switch m.Type {
	case MatchEqual:
		return v == m.Value
	case MatchNotEqual:
		return v != m.Value
	case MatchRegexp:
		return m.re != nil && m.re.MatchString(v)
	case MatchNotRegexp:
		return m.re != nil && !m.re.MatchString(v)
	}
	return false
}

// Selector is a conjunction of matchers.
type Selector []Matcher

// Matches reports whether all matchers match.
func (s Selector) Matches(l Labels) bool {
	for _, m := range s {
		if !m.Matches(l) {
			return false
		}
	}
	return true
}

// ParseSelector parses `name{label="v",other!="x",re=~"a|b",nre!~"c"}`. The
// name and the braces are each optional, but at least one matcher is required.
func ParseSelector(s string) (Selector, error) {
	s = strings.TrimSpace(s)
	var sel Selector
	name := s
	body := ""
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if !strings.HasSuffix(s, "}") {
			return nil, fmt.Errorf("%w: missing closing brace", ErrInvalidSelector)
		}
		name, body = strings.TrimSpace(s[:i]), s[i+1:len(s)-1]
	}
	if name != "" {
		if !metricNameRE.MatchString(name) {
			return nil, fmt.Errorf("%w: bad metric name %q", ErrInvalidSelector, name)
		}
		sel = append(sel, Matcher{Type: MatchEqual, Name: NameLabel, Value: name})
	}
	rest := strings.TrimSpace(body)
	for rest != "" {
		i := strings.IndexAny(rest, "=!")
		if i <= 0 {
			return nil, fmt.Errorf("%w: expected operator in %q", ErrInvalidSelector, rest)
		}
		ln := strings.TrimSpace(rest[:i])
		if !labelNameRE.MatchString(ln) {
			return nil, fmt.Errorf("%w: bad label name %q", ErrInvalidSelector, ln)
		}
		var t MatchType
		switch {
		case strings.HasPrefix(rest[i:], "=~"):
			t, rest = MatchRegexp, rest[i+2:]
		case strings.HasPrefix(rest[i:], "!~"):
			t, rest = MatchNotRegexp, rest[i+2:]
		case strings.HasPrefix(rest[i:], "!="):
			t, rest = MatchNotEqual, rest[i+2:]
		case strings.HasPrefix(rest[i:], "="):
			t, rest = MatchEqual, rest[i+1:]
		default:
			return nil, fmt.Errorf("%w: bad operator in %q", ErrInvalidSelector, rest)
		}
		rest = strings.TrimLeft(rest, " ")
		val, n, err := scanQuoted(rest)
		if err != nil {
			return nil, err
		}
		rest = strings.TrimSpace(rest[n:])
		if rest != "" {
			if rest[0] != ',' {
				return nil, fmt.Errorf("%w: expected comma, got %q", ErrInvalidSelector, rest)
			}
			rest = strings.TrimSpace(rest[1:])
		}
		m, err := NewMatcher(t, ln, val)
		if err != nil {
			return nil, err
		}
		sel = append(sel, m)
	}
	if len(sel) == 0 {
		return nil, fmt.Errorf("%w: empty selector", ErrInvalidSelector)
	}
	return sel, nil
}

// scanQuoted reads a double-quoted string with Go escapes and returns the
// value and the number of bytes consumed.
func scanQuoted(s string) (string, int, error) {
	if s == "" || s[0] != '"' {
		return "", 0, fmt.Errorf("%w: expected quoted value", ErrInvalidSelector)
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			var v string
			if _, err := fmt.Sscanf(s[:i+1], "%q", &v); err != nil {
				return "", 0, fmt.Errorf("%w: bad quoted value %s", ErrInvalidSelector, s[:i+1])
			}
			return v, i + 1, nil
		}
	}
	return "", 0, fmt.Errorf("%w: unterminated string", ErrInvalidSelector)
}
