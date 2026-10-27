// SPDX-License-Identifier: AGPL-3.0-only

// Package restic drives the restic CLI through a replaceable runner. The
// manager and agent never parse human-readable CLI output or invoke unlock.
package restic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Backend string

const (
	BackendLocal Backend = "local"
	BackendSFTP  Backend = "sftp"
	BackendS3    Backend = "s3"
	BackendREST  Backend = "rest-server"
)

var (
	ErrInvalid          = errors.New("restic: invalid configuration or arguments")
	ErrLocked           = errors.New("restic: repository locked; inspect possible stale lock and reconcile the owner")
	ErrRepository       = errors.New("restic: repository unavailable")
	ErrAuthentication   = errors.New("restic: authentication failed")
	ErrApprovalRequired = errors.New("restic: explicit restore approval required")
	ErrTargetExists     = errors.New("restic: restore target already exists")
)

// Request contains the exact child process arguments and environment additions.
// Callers must not log Request.Env. Credentials are never included in Args.
type Request struct {
	Args []string
	Env  map[string]string
}

type Result struct {
	Stdout []byte
	Stderr []byte
}

type Runner interface {
	Run(context.Context, Request) (Result, error)
}

// ExecRunner is the production adapter. Only this child receives the supplied
// credentials. Its returned error and stderr are classified before exposure.
type ExecRunner struct{ Binary string }

func (r ExecRunner) Run(ctx context.Context, req Request) (Result, error) {
	bin := r.Binary
	if bin == "" {
		bin = "restic"
	}
	cmd := exec.CommandContext(ctx, bin, req.Args...)
	// Start from a small process environment. This prevents unrelated manager
	// or agent secrets from leaking into a backend helper's environment.
	baseEnv := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "LC_ALL": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true}
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if baseEnv[name] {
			cmd.Env = append(cmd.Env, item)
		}
	}
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	return Result{Stdout: out.Bytes(), Stderr: stderr.Bytes()}, err
}

// Config refers to an already materialized 0600 password file. Secret values
// are passed only in the restic child's environment and are never serialized.
type Config struct {
	Backend      Backend
	Repository   string
	PasswordFile string
	Credentials  map[string]string
}

type Client struct {
	cfg    Config
	runner Runner
}

func New(cfg Config, runner Runner) (*Client, error) {
	if runner == nil || cfg.PasswordFile == "" || !filepath.IsAbs(cfg.PasswordFile) || filepath.Clean(cfg.PasswordFile) != cfg.PasswordFile {
		return nil, ErrInvalid
	}
	if err := validateRepository(cfg.Backend, cfg.Repository); err != nil {
		return nil, err
	}
	allowed := map[string]Backend{"AWS_ACCESS_KEY_ID": BackendS3, "AWS_SECRET_ACCESS_KEY": BackendS3, "AWS_SESSION_TOKEN": BackendS3,
		"AWS_DEFAULT_REGION": BackendS3, "RESTIC_REST_USERNAME": BackendREST, "RESTIC_REST_PASSWORD": BackendREST}
	for k, v := range cfg.Credentials {
		if allowed[k] != cfg.Backend || strings.ContainsAny(v, "\x00\r\n") {
			return nil, ErrInvalid
		}
	}
	return &Client{cfg: cfg, runner: runner}, nil
}

func validateRepository(backend Backend, repo string) error {
	if repo == "" || strings.ContainsAny(repo, "\x00\r\n") || strings.Contains(repo, "?") || strings.Contains(repo, "#") {
		return ErrInvalid
	}
	switch backend {
	case BackendLocal:
		if filepath.IsAbs(repo) && filepath.Clean(repo) == repo {
			return nil
		}
	case BackendSFTP:
		if regexp.MustCompile(`^sftp:(?:[A-Za-z0-9._-]+@)?[A-Za-z0-9.-]+:/[^\s]*$`).MatchString(repo) {
			return nil
		}
	case BackendS3:
		if strings.HasPrefix(repo, "s3:") {
			location := strings.TrimPrefix(repo, "s3:")
			if !strings.Contains(location, "://") && !strings.Contains(location, "@") && !strings.Contains(location, " ") {
				return nil
			}
			if u, err := url.Parse(location); err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" {
				return nil
			}
		}
	case BackendREST:
		if strings.HasPrefix(repo, "rest:") {
			if u, err := url.Parse(strings.TrimPrefix(repo, "rest:")); err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && !strings.Contains(repo, " ") {
				return nil
			}
		}
	}
	return ErrInvalid
}

// Event is the common JSON shape emitted by backup, forget and check.
type Event struct {
	MessageType string  `json:"message_type"`
	SnapshotID  string  `json:"snapshot_id,omitempty"`
	PercentDone float64 `json:"percent_done,omitempty"`
	FilesNew    int64   `json:"files_new,omitempty"`
	DataAdded   int64   `json:"data_added,omitempty"`
	NumErrors   int64   `json:"num_errors,omitempty"`
}

type Snapshot struct {
	ID    string   `json:"id"`
	Time  string   `json:"time"`
	Paths []string `json:"paths"`
}

type StatsResult struct {
	TotalSize      int64 `json:"total_size"`
	TotalFileCount int64 `json:"total_file_count"`
}

type Retention struct {
	KeepLast    int  `json:"keepLast,omitempty"`
	KeepDaily   int  `json:"keepDaily,omitempty"`
	KeepWeekly  int  `json:"keepWeekly,omitempty"`
	KeepMonthly int  `json:"keepMonthly,omitempty"`
	KeepYearly  int  `json:"keepYearly,omitempty"`
	Prune       bool `json:"prune,omitempty"`
}

func safePath(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsAny(p, "\x00\r\n") && p != "/"
}

func (c *Client) run(ctx context.Context, args ...string) (Result, error) {
	env := map[string]string{"RESTIC_PASSWORD_FILE": c.cfg.PasswordFile}
	for k, v := range c.cfg.Credentials {
		env[k] = v
	}
	argv := []string{"-r", c.cfg.Repository, args[0], "--json"}
	argv = append(argv, args[1:]...)
	res, err := c.runner.Run(ctx, Request{Args: argv, Env: env})
	if err != nil {
		return Result{}, classify(err, res.Stderr)
	}
	return res, nil
}

func classify(err error, stderr []byte) error {
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 10:
			return ErrRepository
		case 11:
			return ErrLocked
		case 12:
			return ErrAuthentication
		}
	}
	s := strings.ToLower(string(stderr))
	switch {
	case strings.Contains(s, "lock"):
		return ErrLocked
	case strings.Contains(s, "wrong password"), strings.Contains(s, "unauthorized"), strings.Contains(s, "access denied"):
		return ErrAuthentication
	case strings.Contains(s, "repository"), strings.Contains(s, "connection"):
		return ErrRepository
	default:
		// The runner error can include a binary path, but never pass through
		// child stderr because backends may echo secrets there.
		_ = err
		return errors.New("restic: command failed")
	}
}

func parseEvents(raw []byte) ([]Event, error) {
	var events []Event
	s := bufio.NewScanner(bytes.NewReader(raw))
	s.Buffer(make([]byte, 4096), 4<<20)
	for s.Scan() {
		if len(bytes.TrimSpace(s.Bytes())) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(s.Bytes(), &e); err != nil || e.MessageType == "" {
			return nil, fmt.Errorf("%w: malformed JSON event", ErrInvalid)
		}
		events = append(events, e)
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("%w: JSON event too large", ErrInvalid)
	}
	return events, nil
}

func (c *Client) Backup(ctx context.Context, paths, excludes []string) ([]Event, error) {
	if len(paths) == 0 {
		return nil, ErrInvalid
	}
	args := []string{"backup"}
	if c.cfg.Backend == BackendLocal {
		for _, p := range paths {
			rel, err := filepath.Rel(p, c.cfg.Repository)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				excludes = append(excludes, c.cfg.Repository)
				break
			}
		}
	}
	for _, p := range excludes {
		if p == "" || strings.HasPrefix(p, "-") || strings.ContainsAny(p, "\x00\r\n") {
			return nil, ErrInvalid
		}
		args = append(args, "--exclude", p)
	}
	for _, p := range paths {
		if !safePath(p) {
			return nil, ErrInvalid
		}
		args = append(args, p)
	}
	res, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	events, err := parseEvents(res.Stdout)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 || events[len(events)-1].MessageType != "summary" {
		return nil, fmt.Errorf("%w: backup summary missing", ErrInvalid)
	}
	return events, nil
}

func (c *Client) Snapshots(ctx context.Context) ([]Snapshot, error) {
	res, err := c.run(ctx, "snapshots")
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		return nil, fmt.Errorf("%w: malformed snapshots JSON", ErrInvalid)
	}
	return out, nil
}

type ForgetGroup struct {
	Host   string     `json:"host"`
	Paths  []string   `json:"paths"`
	Keep   []Snapshot `json:"keep"`
	Remove []Snapshot `json:"remove"`
}

func (c *Client) Forget(ctx context.Context, policy Retention) ([]ForgetGroup, error) {
	args := []string{"forget"}
	for _, p := range []struct {
		flag string
		n    int
	}{{"--keep-last", policy.KeepLast}, {"--keep-daily", policy.KeepDaily}, {"--keep-weekly", policy.KeepWeekly}, {"--keep-monthly", policy.KeepMonthly}, {"--keep-yearly", policy.KeepYearly}} {
		if p.n < 0 {
			return nil, ErrInvalid
		}
		if p.n > 0 {
			args = append(args, p.flag, strconv.Itoa(p.n))
		}
	}
	if len(args) == 1 {
		return nil, ErrInvalid
	}
	if policy.Prune {
		args = append(args, "--prune")
	}
	res, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	// restic prints an initial JSON array for forget. With --prune, some
	// releases append non-JSON prune progress; success is its exit code.
	var groups []ForgetGroup
	if err := json.NewDecoder(bytes.NewReader(res.Stdout)).Decode(&groups); err != nil {
		return nil, fmt.Errorf("%w: malformed forget JSON", ErrInvalid)
	}
	return groups, nil
}

var subsetRe = regexp.MustCompile(`^(?:[1-9][0-9]?|100)%$`)

func (c *Client) Check(ctx context.Context, subset string) ([]Event, error) {
	if !subsetRe.MatchString(subset) {
		return nil, ErrInvalid
	}
	res, err := c.run(ctx, "check", "--read-data-subset", subset)
	if err != nil {
		return nil, err
	}
	events, err := parseEvents(res.Stdout)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 || events[len(events)-1].MessageType != "summary" || events[len(events)-1].NumErrors > 0 {
		return nil, errors.New("restic: repository check failed")
	}
	return events, nil
}

func (c *Client) Stats(ctx context.Context) (StatsResult, error) {
	res, err := c.run(ctx, "stats")
	if err != nil {
		return StatsResult{}, err
	}
	var out StatsResult
	if err := json.Unmarshal(res.Stdout, &out); err != nil {
		return StatsResult{}, fmt.Errorf("%w: malformed stats JSON", ErrInvalid)
	}
	return out, nil
}

func (c *Client) Restore(ctx context.Context, snapshot, target string, approved bool) error {
	if !approved {
		return ErrApprovalRequired
	}
	if !regexp.MustCompile(`^[a-fA-F0-9]{6,64}$`).MatchString(snapshot) || !safePath(target) {
		return ErrInvalid
	}
	if _, err := os.Lstat(target); err == nil {
		return ErrTargetExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrInvalid
	}
	// Claim the new path atomically. A concurrent creator cannot turn this
	// into a restore over live data between validation and the restic call.
	if err := os.Mkdir(target, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrTargetExists
		}
		return ErrInvalid
	}
	_, err := c.run(ctx, "restore", snapshot, "--target", target, "--overwrite", "never")
	return err
}
