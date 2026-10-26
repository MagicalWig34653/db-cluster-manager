// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/version"
)

const help = `cluster-manager CLI
usage: cli [--config PATH] [--context NAME] [--output table|json] COMMAND

version | help | completion bash|zsh|fish
contexts list|use NAME|set NAME URL ORIGIN
login [--token-stdin|--token-file PATH] | logout | whoami
clusters list|get ID|create --file PATH|plan ID --file PATH|delete ID|approve OPERATION DIGEST
operations list|watch ID [--interval SECONDS]|abort ID|approve ID DIGEST
backups list|run CLUSTER [--file PATH]|verify CLUSTER [--result-file PATH [--backup ID] [--host ID]]
hosts list
services plan --file PATH|create --file PATH
vuln findings list|approve PROPOSAL DIGEST
plan export --file PATH [--format json|yaml]

Login accepts an existing manager session token via stdin, environment or file.
There is no general Bearer API token endpoint. No secret command-line flag exists.
Exit codes: 0 success; 2 usage/config; 3 authentication/authorization;
4 missing; 5 conflict; 6 unavailable/network; 7 no verified restore; 1 other.
`

type config struct {
	Current  string                  `json:"currentContext"`
	Contexts map[string]contextEntry `json:"contexts"`
}
type contextEntry struct {
	URL         string `json:"url"`
	Origin      string `json:"origin"`
	SessionFile string `json:"sessionFile,omitempty"`
}
type client struct {
	base, origin, token string
	http                *http.Client
}
type apiError struct {
	status int
	code   string
}

func (e apiError) Error() string { return fmt.Sprintf("API %d: %s", e.status, e.code) }
func exitCode(err error) int {
	var e apiError
	if errors.As(err, &e) {
		switch e.status {
		case 401, 403:
			return 3
		case 404:
			return 4
		case 409:
			return 5
		case 503:
			return 6
		default:
			return 1
		}
	}
	return 6
}

func (c client) request(method, path string, body []byte) ([]byte, error) {
	u, err := url.Parse(c.base)
	if err != nil {
		return nil, err
	}
	suffix, err := url.Parse("/api/v1" + path)
	if err != nil {
		return nil, err
	}
	u.Path = suffix.Path
	u.RawQuery = suffix.RawQuery
	req, err := http.NewRequest(method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		name := "__Host-dcm_session"
		if u.Scheme == "http" {
			name = "dcm_session"
		}
		req.AddCookie(&http.Cookie{Name: name, Value: c.token})
	}
	if method != "GET" {
		req.Header.Set("Origin", c.origin)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var v struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &v)
		if v.Error == "" {
			v.Error = http.StatusText(resp.StatusCode)
		}
		return nil, apiError{resp.StatusCode, v.Error}
	}
	return data, nil
}

func validToken(t string) bool {
	return t != "" && len(t) <= 4096 && !strings.ContainsAny(t, " \t\n\r;,=")
}
func configPath() string {
	if x := os.Getenv("CLI_CONFIG"); x != "" {
		return x
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "cluster-manager", "cli.json")
}
func loadConfig(path string) (config, error) {
	c := config{Contexts: map[string]contextEntry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	if c.Contexts == nil {
		c.Contexts = map[string]contextEntry{}
	}
	return c, err
}
func atomicPrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cli-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func saveConfig(path string, c config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomicPrivate(path, append(b, '\n'))
}
func sessionPath(configFile, name string, e contextEntry) string {
	if e.SessionFile != "" {
		return e.SessionFile
	}
	return filepath.Join(filepath.Dir(configFile), "sessions", url.PathEscape(name)+".token")
}

func validateContext(e contextEntry) (contextEntry, error) {
	u, err := url.Parse(e.URL)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" && !(u.Scheme == "http" && u.Hostname() == "localhost") {
		return e, fmt.Errorf("context URL must be HTTPS or localhost HTTP origin without credentials")
	}
	if e.Origin == "" {
		e.Origin = u.Scheme + "://" + u.Host
	}
	o, err := url.Parse(e.Origin)
	if err != nil || o.Scheme != u.Scheme || o.Host != u.Host || o.Path != "" || o.User != nil || o.RawQuery != "" || o.Fragment != "" {
		return e, fmt.Errorf("context origin must match URL")
	}
	return e, nil
}
func tokenFrom(r io.Reader, path string) (string, error) {
	var b []byte
	var err error
	if path != "" {
		b, err = os.ReadFile(path)
	} else {
		b, err = io.ReadAll(io.LimitReader(r, 4097))
	}
	if err != nil {
		return "", err
	}
	if len(b) > 4096 {
		return "", fmt.Errorf("session token too large")
	}
	t := strings.TrimSpace(string(b))
	if !validToken(t) {
		return "", fmt.Errorf("invalid session token")
	}
	return t, nil
}
func inputFile(args []string, required bool) ([]byte, error) {
	fs := flag.NewFlagSet("input", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	p := fs.String("file", "", "JSON file")
	if fs.Parse(args) != nil || len(fs.Args()) != 0 || required && *p == "" {
		return nil, fmt.Errorf("expected --file PATH")
	}
	if *p == "" {
		return nil, nil
	}
	b, err := os.ReadFile(*p)
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("input is not JSON")
	}
	return b, nil
}
func emit(w io.Writer, format string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if format == "json" {
		var b bytes.Buffer
		if err := json.Indent(&b, data, "", "  "); err != nil {
			return err
		}
		_, err := fmt.Fprintln(w, b.String())
		return err
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		_, e := fmt.Fprintln(w, string(data))
		return e
	}
	if m, ok := v.(map[string]any); ok {
		if a, yes := m["items"].([]any); yes {
			v = a
		}
	}
	rows, ok := v.([]any)
	if !ok {
		rows = []any{v}
	}
	keys := map[string]bool{}
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			for k := range m {
				keys[k] = true
			}
		}
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(names, "\t"))
	for _, r := range rows {
		m, _ := r.(map[string]any)
		vals := make([]string, len(names))
		for i, k := range names {
			if x, ok := m[k]; ok {
				if s, ok := x.(string); ok {
					vals[i] = s
				} else {
					b, _ := json.Marshal(x)
					vals[i] = string(b)
				}
			}
		}
		fmt.Fprintln(tw, strings.Join(vals, "\t"))
	}
	return tw.Flush()
}

func run(args []string, in io.Reader, out, stderr io.Writer, transport http.RoundTripper) int {
	fs := flag.NewFlagSet("cli", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := fs.String("config", configPath(), "config file")
	ctx := fs.String("context", "", "context")
	format := fs.String("output", "table", "output format")
	if fs.Parse(args) != nil {
		return 2
	}
	a := fs.Args()
	if len(a) == 0 {
		fmt.Fprint(stderr, help)
		return 2
	}
	if *format != "table" && *format != "json" {
		fmt.Fprintln(stderr, "output must be table or json")
		return 2
	}
	if a[0] == "help" {
		fmt.Fprint(out, help)
		return 0
	}
	if a[0] == "version" && len(a) == 1 {
		_ = json.NewEncoder(out).Encode(version.Get("cli"))
		return 0
	}
	if a[0] == "completion" && len(a) == 2 {
		return completion(a[1], out, stderr)
	}
	if *cf == "" {
		fmt.Fprintln(stderr, "config path unavailable")
		return 2
	}
	cfg, err := loadConfig(*cf)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if a[0] == "contexts" {
		return contexts(a[1:], *cf, &cfg, out, stderr)
	}
	name := *ctx
	if name == "" {
		name = cfg.Current
	}
	e, ok := cfg.Contexts[name]
	if !ok {
		fmt.Fprintln(stderr, "unknown context")
		return 2
	}
	e, err = validateContext(e)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	cl := client{base: e.URL, origin: e.Origin, http: &http.Client{Timeout: 30 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if a[0] == "login" {
		lf := flag.NewFlagSet("login", flag.ContinueOnError)
		lf.SetOutput(io.Discard)
		stdin := lf.Bool("token-stdin", false, "stdin")
		file := lf.String("token-file", "", "file")
		if lf.Parse(a[1:]) != nil || len(lf.Args()) != 0 || *stdin && *file != "" {
			fmt.Fprintln(stderr, "usage: login [--token-stdin|--token-file PATH]")
			return 2
		}
		if !*stdin && *file == "" {
			cl.token = os.Getenv("CLI_SESSION_TOKEN")
			if cl.token == "" {
				*file = os.Getenv("CLI_SESSION_TOKEN_FILE")
			}
		}
		if cl.token == "" {
			if *file == "" && !*stdin {
				fmt.Fprintln(stderr, "no token source")
				return 2
			}
			cl.token, err = tokenFrom(in, *file)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
		}
		if !validToken(cl.token) {
			fmt.Fprintln(stderr, "invalid session token")
			return 2
		}
		if _, err = cl.request("GET", "/auth/session", nil); err != nil {
			fmt.Fprintln(stderr, err)
			return exitCode(err)
		}
		if err = atomicPrivate(sessionPath(*cf, name, e), []byte(cl.token+"\n")); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintln(out, "Session validated and stored.")
		return 0
	}
	if a[0] == "logout" {
		if len(a) != 1 {
			return 2
		}
		err = os.Remove(sessionPath(*cf, name, e))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintln(out, "Local session removed.")
		return 0
	}
	if x := os.Getenv("CLI_SESSION_TOKEN"); x != "" {
		cl.token = x
	} else if x := os.Getenv("CLI_SESSION_TOKEN_FILE"); x != "" {
		cl.token, err = tokenFrom(nil, x)
	} else {
		cl.token, err = tokenFrom(nil, sessionPath(*cf, name, e))
	}
	if err != nil || cl.token == "" {
		fmt.Fprintln(stderr, "no session; run login or set CLI_SESSION_TOKEN(_FILE)")
		return 3
	}
	if !validToken(cl.token) {
		fmt.Fprintln(stderr, "invalid session token")
		return 2
	}
	method, path, body, raw, err := command(a)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if method == "VERIFY" {
		return verify(cl, path, out, stderr)
	}
	if method == "INSPECT" {
		data, err := cl.request("POST", "/restores/inspect", body)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitCode(err)
		}
		if err := atomicPrivate(path, data); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		var summary map[string]any
		if json.Unmarshal(data, &summary) != nil {
			fmt.Fprintln(stderr, "invalid inspection response")
			return 1
		}
		delete(summary, "inspectionPassword")
		safe, _ := json.Marshal(summary)
		if err := emit(out, *format, safe); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if method == "WATCH" {
		return watch(cl, path, body, *format, out, stderr)
	}
	data, err := cl.request(method, path, body)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitCode(err)
	}
	if raw {
		_, err = out.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			fmt.Fprintln(out)
		}
	} else {
		err = emit(out, *format, data)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
func contexts(a []string, path string, c *config, out, stderr io.Writer) int {
	switch {
	case len(a) == 1 && a[0] == "list":
		names := make([]string, 0, len(c.Contexts))
		for n := range c.Contexts {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			m := " "
			if n == c.Current {
				m = "*"
			}
			fmt.Fprintf(out, "%s %s %s\n", m, n, c.Contexts[n].URL)
		}
		return 0
	case len(a) == 2 && a[0] == "use":
		if _, ok := c.Contexts[a[1]]; !ok {
			fmt.Fprintln(stderr, "unknown context")
			return 2
		}
		c.Current = a[1]
	case len(a) == 4 && a[0] == "set":
		if a[1] == "" || strings.ContainsAny(a[1], "/\\ ") {
			return 2
		}
		e, err := validateContext(contextEntry{URL: a[2], Origin: a[3]})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		c.Contexts[a[1]] = e
		if c.Current == "" {
			c.Current = a[1]
		}
	default:
		fmt.Fprintln(stderr, "usage: contexts list|use NAME|set NAME URL ORIGIN")
		return 2
	}
	if err := saveConfig(path, *c); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	return 0
}
func id(s string) (string, error) {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\?#") {
		return "", fmt.Errorf("invalid ID")
	}
	return url.PathEscape(s), nil
}
func command(a []string) (method, path string, body []byte, raw bool, err error) {
	if len(a) == 1 && a[0] == "whoami" {
		return "GET", "/auth/session", nil, false, nil
	}
	if len(a) < 2 {
		return "", "", nil, false, fmt.Errorf("missing subcommand")
	}
	kind, sub, rest := a[0], a[1], a[2:]
	lists := map[string]string{"clusters": "/clusters", "operations": "/operations", "backups": "/backups", "hosts": "/hosts"}
	if sub == "list" && len(rest) == 0 && lists[kind] != "" {
		return "GET", lists[kind], nil, false, nil
	}
	if kind == "vuln" && sub == "findings" && len(rest) == 1 && rest[0] == "list" {
		return "GET", "/vuln/findings", nil, false, nil
	}
	if len(rest) > 0 {
		x, e := id(rest[0])
		if e != nil {
			return "", "", nil, false, e
		}
		switch kind + " " + sub {
		case "clusters get":
			if len(rest) == 1 {
				return "GET", "/clusters/" + x, nil, false, nil
			}
		case "clusters delete":
			if len(rest) == 1 {
				return "DELETE", "/clusters/" + x, nil, false, nil
			}
		case "operations abort":
			if len(rest) == 1 {
				return "POST", "/operations/" + x + "/abort", nil, false, nil
			}
		case "operations watch":
			if len(rest) == 1 {
				return "WATCH", x, nil, false, nil
			}
			if len(rest) == 3 && rest[1] == "--interval" {
				return "WATCH", x, []byte(rest[2]), false, nil
			}
		case "backups verify":
			if len(rest) == 1 {
				return "VERIFY", x, nil, false, nil
			}
			fs := flag.NewFlagSet("verify", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			result := fs.String("result-file", "", "private inspection result")
			backup := fs.String("backup", "", "backup ID")
			host := fs.String("host", "", "host ID")
			if fs.Parse(rest[1:]) != nil || len(fs.Args()) != 0 || *result == "" {
				return "", "", nil, false, fmt.Errorf("usage: backups verify CLUSTER --result-file PATH [--backup ID] [--host ID]")
			}
			body, _ = json.Marshal(map[string]string{"cluster": rest[0], "backup": *backup, "host": *host})
			return "INSPECT", *result, body, false, nil
		case "backups run":
			if len(rest) == 1 {
				return "POST", "/clusters/" + x + "/backups", nil, false, nil
			}
			body, err = inputFile(rest[1:], true)
			return "POST", "/clusters/" + x + "/backups", body, false, err
		case "clusters plan":
			body, err = inputFile(rest[1:], true)
			return "POST", "/clusters/" + x + "/operation-plan", body, false, err
		case "operations approve", "clusters approve", "vuln approve":
			if len(rest) == 2 {
				body, _ = json.Marshal(map[string]string{"digest": rest[1]})
				if kind == "vuln" {
					return "POST", "/vuln/proposals/" + x + "/approve", body, false, nil
				}
				return "POST", "/operations/" + x + "/approve", body, false, nil
			}
		}
	}
	switch kind + " " + sub {
	case "clusters create":
		body, err = inputFile(rest, true)
		return "POST", "/clusters", body, false, err
	case "services plan", "services create":
		body, err = inputFile(rest, true)
		if sub == "plan" {
			return "POST", "/services/plan", body, false, err
		}
		return "POST", "/services", body, false, err
	case "plan export":
		fs := flag.NewFlagSet("export", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		file := fs.String("file", "", "input")
		f := fs.String("format", "yaml", "format")
		if fs.Parse(rest) != nil || len(fs.Args()) != 0 || *file == "" || *f != "yaml" && *f != "json" {
			return "", "", nil, false, fmt.Errorf("usage: plan export --file PATH [--format json|yaml]")
		}
		body, err = os.ReadFile(*file)
		if err != nil {
			return
		}
		if !json.Valid(body) {
			return "", "", nil, false, fmt.Errorf("input is not JSON")
		}
		return "POST", "/services/plan?format=" + *f, body, true, nil
	}
	return "", "", nil, false, fmt.Errorf("unknown command; run cli help")
}
func verify(c client, cluster string, out, stderr io.Writer) int {
	b, err := c.request("GET", "/backups", nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitCode(err)
	}
	var v struct {
		Items []struct {
			Cluster string `json:"cluster"`
			At      string `json:"lastVerifiedRestoreAt"`
		} `json:"items"`
	}
	if json.Unmarshal(b, &v) != nil {
		fmt.Fprintln(stderr, "invalid backup response")
		return 1
	}
	for _, x := range v.Items {
		if x.Cluster == cluster {
			if x.At == "" || strings.HasPrefix(x.At, "0001-") {
				fmt.Fprintln(stderr, "no passed restore verification recorded")
				return 7
			}
			fmt.Fprintf(out, "%s\t%s\n", x.Cluster, x.At)
			return 0
		}
	}
	fmt.Fprintln(stderr, "cluster backup summary missing")
	return 4
}
func watch(c client, target string, interval []byte, format string, out, stderr io.Writer) int {
	d := 2 * time.Second
	if len(interval) > 0 {
		n, err := strconv.Atoi(string(interval))
		if err != nil || n < 1 || n > 3600 {
			fmt.Fprintln(stderr, "interval must be 1..3600 seconds")
			return 2
		}
		d = time.Duration(n) * time.Second
	}
	last := ""
	for {
		b, err := c.request("GET", "/operations", nil)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitCode(err)
		}
		var v struct {
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(b, &v) != nil {
			fmt.Fprintln(stderr, "invalid operations response")
			return 1
		}
		found := false
		for _, item := range v.Items {
			var op struct{ ID, State string }
			if json.Unmarshal(item, &op) != nil || op.ID != target {
				continue
			}
			found = true
			if string(item) != last {
				if err := emit(out, format, item); err != nil {
					fmt.Fprintln(stderr, err)
					return 1
				}
				last = string(item)
			}
			switch op.State {
			case "succeeded":
				return 0
			case "failed", "aborted", "unknown":
				return 1
			}
		}
		if !found {
			fmt.Fprintln(stderr, "operation missing")
			return 4
		}
		time.Sleep(d)
	}
}
