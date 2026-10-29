// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// JobStatus is the state of a build job.
type JobStatus string

const (
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
)

// Job records one approved build: its inputs, the digests that identify the
// result, and the redacted log. It contains no secret.
type Job struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Status JobStatus     `json:"status"`
	Spec   WebServerSpec `json:"spec"`
	// Source is empty for the typed web server spec and "composer" for jobs
	// from the image composer; Recipe then holds the normalized recipe.
	Source      string          `json:"source,omitempty"`
	Recipe      json.RawMessage `json:"recipe,omitempty"`
	SpecHash    string          `json:"specHash"`
	ContextHash string          `json:"contextHash"`
	PlanDigest  string          `json:"planDigest"`
	Reference   string          `json:"reference"`
	Platforms   []string        `json:"platforms"`
	BaseDigests []string        `json:"baseDigests,omitempty"`
	ImageDigest string          `json:"imageDigest,omitempty"`
	SBOM        SBOMRef         `json:"sbom"`
	// Error is the redacted failure text.
	Error string `json:"error,omitempty"`
	// Log is the redacted build log, capped at the service's limit; it is
	// omitted from list results (see LogSize).
	Log          string    `json:"log,omitempty"`
	LogSize      int       `json:"logSize"`
	LogTruncated bool      `json:"logTruncated,omitempty"`
	RequestedBy  string    `json:"requestedBy"`
	CreatedAt    time.Time `json:"createdAt"`
	FinishedAt   time.Time `json:"finishedAt,omitzero"`
}

// ErrJobNotFound is returned for an unknown job ID.
var ErrJobNotFound = errors.New("imagebuild: job not found")
var ErrJobExists = errors.New("imagebuild: job already exists")

// JobStore persists job records.
type JobStore interface {
	Create(ctx context.Context, j Job) error // insert only
	Put(ctx context.Context, j Job) error    // insert or replace
	Get(ctx context.Context, id string) (Job, error)
	List(ctx context.Context) ([]Job, error) // newest first
}

// Create implements JobStore with an atomic insert.
func (s *MemoryJobStore) Create(_ context.Context, j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]Job{}
	}
	if _, ok := s.m[j.ID]; ok {
		return ErrJobExists
	}
	s.m[j.ID] = j
	return nil
}

// MemoryJobStore is an in-memory JobStore for tests (a fake).
type MemoryJobStore struct {
	mu sync.Mutex
	m  map[string]Job
}

// Put implements JobStore.
func (s *MemoryJobStore) Put(_ context.Context, j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]Job{}
	}
	s.m[j.ID] = j
	return nil
}

// Get implements JobStore.
func (s *MemoryJobStore) Get(_ context.Context, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.m[id]
	if !ok {
		return Job{}, ErrJobNotFound
	}
	return j, nil
}

// List implements JobStore.
func (s *MemoryJobStore) List(context.Context) ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.m))
	for _, j := range s.m {
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// Redactor removes secrets from text. It knows exact secret values (and the
// base64 form docker uses for `user:password`) and also masks common
// credential patterns, so a secret the redactor was never told about is
// still masked when it appears in a recognizable form.
type Redactor struct{ values []string }

const mask = "[REDACTED]"

// NewRedactor registers exact values to remove; empty values are ignored.
func NewRedactor(values ...string) *Redactor {
	r := &Redactor{}
	for _, v := range values {
		r.Add(v)
	}
	return r
}

// Add registers one more exact value.
func (r *Redactor) Add(v string) {
	if v != "" {
		r.values = append(r.values, v)
	}
}

// AddBasicAuth registers username:password in its base64 form.
func (r *Redactor) AddBasicAuth(user, password string) {
	if password != "" {
		r.Add(base64.StdEncoding.EncodeToString([]byte(user + ":" + password)))
	}
}

var redactPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic)\s+)\S+`), "${1}" + mask},
	{regexp.MustCompile(`(?i)((?:password|passwd|token|secret|api[_-]?key)["']?\s*[=:]\s*["']?)[^\s"',;&]+`), "${1}" + mask},
	{regexp.MustCompile(`(://[^/\s:@]+:)[^@\s/]+@`), "${1}" + mask + "@"},
	{regexp.MustCompile(`("auth"\s*:\s*")[^"]+"`), "${1}" + mask + `"`},
}

// Redact returns s with known values and credential patterns masked.
func (r *Redactor) Redact(s string) string {
	// Longest values first so a value containing another is fully masked.
	vals := append([]string(nil), r.values...)
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		s = strings.ReplaceAll(s, v, mask)
	}
	for _, p := range redactPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// logCapture is an io.Writer that redacts complete lines and keeps at most
// max bytes. Lines are redacted whole, so a secret split across writes is
// still caught.
type logCapture struct {
	r         *Redactor
	max       int
	pending   strings.Builder
	out       strings.Builder
	truncated bool
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.pending.Write(p)
	s := l.pending.String()
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			break
		}
		l.addLine(s[:i+1])
		s = s[i+1:]
	}
	l.pending.Reset()
	l.pending.WriteString(s)
	// An unterminated line cannot grow without bound.
	if l.pending.Len() > 64<<10 {
		l.addLine(l.pending.String() + "\n")
		l.pending.Reset()
	}
	return len(p), nil
}

func (l *logCapture) addLine(line string) {
	line = l.r.Redact(line)
	if l.truncated || l.out.Len()+len(line) > l.max {
		l.truncated = true
		return
	}
	l.out.WriteString(line)
}

// finish flushes a trailing unterminated line.
func (l *logCapture) finish() {
	if l.pending.Len() > 0 {
		l.addLine(l.pending.String())
		l.pending.Reset()
	}
}
