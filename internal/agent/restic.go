// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
)

// ResticArgs reference mounted secrets by name. No password or backend
// credential value is accepted in a fenced command or recorded step.
type ResticArgs struct {
	Backend          restic.Backend   `json:"backend"`
	Repository       string           `json:"repository"`
	PasswordSecret   string           `json:"password_secret"`
	CredentialSecret string           `json:"credential_secret,omitempty"`
	Paths            []string         `json:"paths,omitempty"`
	Excludes         []string         `json:"excludes,omitempty"`
	Retention        restic.Retention `json:"retention,omitempty"`
	ReadDataSubset   string           `json:"read_data_subset,omitempty"`
	Snapshot         string           `json:"snapshot,omitempty"`
	Target           string           `json:"target,omitempty"`
	Approved         bool             `json:"approved,omitempty"`
}

func checkResticArgs(a *ResticArgs) error {
	if !secretRe.MatchString(a.PasswordSecret) || (a.CredentialSecret != "" && !secretRe.MatchString(a.CredentialSecret)) {
		return invalid("restic secret names required")
	}
	_, err := restic.New(restic.Config{Backend: a.Backend, Repository: a.Repository, PasswordFile: "/run/secrets/placeholder"}, restic.ExecRunner{})
	if err != nil {
		return invalid("restic backend or repository invalid")
	}
	return nil
}

func withinRestic(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (e *Executor) resticPathAllowed(path string, target bool) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return false
	}
	for _, blocked := range []string{e.cfg.SecretsDir, "/run/secrets", "/var/run/docker.sock", "/run/docker.sock"} {
		if withinRestic(blocked, path) || withinRestic(path, blocked) {
			return false
		}
	}
	// A source must exist and resolve under an allow-listed root. A new
	// restore target resolves its parent instead, and the target must be absent.
	probe := path
	if target {
		probe = filepath.Dir(path)
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	resolved, err := filepath.EvalSymlinks(probe)
	if err != nil {
		return false
	}
	for _, root := range e.cfg.ResticAllowedPaths {
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err == nil && withinRestic(root, path) && withinRestic(resolvedRoot, resolved) {
			return true
		}
	}
	return false
}

func (e *Executor) runRestic(ctx context.Context, kind Kind, a ResticArgs) Result {
	for _, p := range a.Paths {
		if !e.resticPathAllowed(p, false) {
			return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: "restic source is outside the host allow-list or protected"}
		}
	}
	for _, p := range a.Excludes {
		allowed := false
		if filepath.IsAbs(p) && filepath.Clean(p) == p {
			for _, root := range e.cfg.ResticAllowedPaths {
				if withinRestic(root, p) {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: "restic exclusion is outside the host allow-list"}
		}
	}
	if kind == KindResticRestore && (!a.Approved || !e.resticPathAllowed(a.Target, true)) {
		return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: "restore needs approval and a new allowed target"}
	}
	passwordFile := filepath.Join(e.cfg.SecretsDir, a.PasswordSecret)
	st, err := os.Lstat(passwordFile)
	if err != nil || !st.Mode().IsRegular() {
		return Result{Status: StatusRefused, Code: CodeInvalidSecret, Message: "restic password secret unavailable"}
	}
	credentials := map[string]string{}
	if a.CredentialSecret != "" {
		raw, res, ok := e.readSecret(a.CredentialSecret)
		if !ok {
			return res
		}
		if err := json.Unmarshal([]byte(raw), &credentials); err != nil {
			return Result{Status: StatusRefused, Code: CodeInvalidSecret, Message: "restic backend credential secret is invalid"}
		}
	}
	runner := e.cfg.ResticRunner
	if runner == nil {
		runner = restic.ExecRunner{}
	}
	c, err := restic.New(restic.Config{Backend: a.Backend, Repository: a.Repository, PasswordFile: passwordFile, Credentials: credentials}, runner)
	if err != nil {
		return Result{Status: StatusRefused, Code: CodeInvalidSecret, Message: "restic configuration invalid"}
	}
	var output any
	switch kind {
	case KindResticBackup:
		if len(a.Paths) == 0 {
			return Result{Status: StatusRefused, Code: CodeInvalidPath, Message: "restic source required"}
		}
		excludes := append([]string{"**/.ssh", "**/.aws", "**/secrets", "**/docker.sock", e.cfg.SecretsDir, "/run/secrets", "/var/run/docker.sock"}, a.Excludes...)
		output, err = c.Backup(ctx, a.Paths, excludes)
	case KindResticSnapshots:
		output, err = c.Snapshots(ctx)
	case KindResticForget:
		output, err = c.Forget(ctx, a.Retention)
	case KindResticCheck:
		output, err = c.Check(ctx, a.ReadDataSubset)
	case KindResticRestore:
		err = c.Restore(ctx, a.Snapshot, a.Target, a.Approved)
	case KindResticStats:
		output, err = c.Stats(ctx)
	}
	if err != nil {
		return Result{Status: StatusFailed, Code: CodeIO, Message: err.Error()}
	}
	return succeeded(output)
}
