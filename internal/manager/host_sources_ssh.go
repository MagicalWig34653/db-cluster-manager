// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/api"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
	"github.com/MagicalWig34653/db-cluster-manager/internal/restic"
	"github.com/MagicalWig34653/db-cluster-manager/internal/runner"
)

// HostSourceSSHClient dispatches only restic-source/<id> steps to a reviewed
// SSH host identity. The normal agent client handles every other target.
// The manager runner still supplies term fencing, journal and source lock.
type HostSourceSSHClient struct {
	Next     runner.AgentClient
	Store    *Store
	Keys     KeyProvider
	Commands provision.CommandRunner
}

var _ runner.AgentClient = HostSourceSSHClient{}

func (c HostSourceSSHClient) Send(ctx context.Context, target string, cmd agent.Command) (agent.Result, error) {
	if !strings.HasPrefix(target, "restic-source/") {
		if c.Next == nil {
			return agent.Result{}, api.ErrUnavailable
		}
		return c.Next.Send(ctx, target, cmd)
	}
	id := strings.TrimPrefix(target, "restic-source/")
	if id == "" || strings.Contains(id, "/") || c.Store == nil || c.Keys == nil || c.Commands == nil {
		return agent.Result{}, agent.ErrInvalidCommand
	}
	h, ok, err := Get[api.HostSource](c.Store, CollHostSources, id, Consistent())
	if err != nil || !ok || h.Value.Kind != "external-host" || h.Value.Transport != "ssh-plan" {
		return agent.Result{}, agent.ErrInvalidCommand
	}
	secret, ok, err := GetSealed[HostSourceSecrets](c.Store, c.Keys, CollHostSourceSecrets, id, Consistent())
	if err != nil || !ok || secret.SSHPrivateKey == "" || secret.SSHKnownHosts == "" {
		return agent.Result{}, api.ErrUnavailable
	}
	var args agent.ResticArgs
	if json.Unmarshal(cmd.Args, &args) != nil || args.Backend != h.Value.Backend || args.Repository != h.Value.Repository ||
		args.PasswordSecret != h.Value.PasswordSecret || args.CredentialSecret != h.Value.CredentialSecret {
		return agent.Result{}, agent.ErrInvalidCommand
	}
	remote := &sshResticRunner{host: provision.Host{ID: h.Value.HostID, Address: h.Value.SSHAddress, User: h.Value.SSHUser},
		credential: provision.Credential{PrivateKey: secret.SSHPrivateKey, KnownHosts: secret.SSHKnownHosts},
		password:   secret.Password, commands: c.Commands}
	client, err := restic.New(restic.Config{Backend: h.Value.Backend, Repository: h.Value.Repository,
		PasswordFile: "/tmp/dbcm-remote-password", Credentials: secret.Credentials}, remote)
	if err != nil {
		return agent.Result{}, agent.ErrInvalidCommand
	}
	result := agent.Result{OpID: cmd.OpID, StepID: cmd.StepID, Kind: cmd.Kind, Status: agent.StatusSucceeded}
	var output any
	switch cmd.Kind {
	case KindResticSSHBackup:
		if !slices.Equal(args.Paths, h.Value.Paths) || !slices.Equal(args.Excludes, h.Value.Excludes) {
			return agent.Result{}, agent.ErrInvalidCommand
		}
		output, err = client.Backup(ctx, args.Paths, append([]string{"**/.ssh", "**/.aws", "**/secrets", "**/docker.sock", "/run/secrets", "/var/run/docker.sock"}, args.Excludes...))
	case KindResticSSHForget:
		if args.Retention != h.Value.Retention {
			return agent.Result{}, agent.ErrInvalidCommand
		}
		output, err = client.Forget(ctx, args.Retention)
	case KindResticSSHCheck:
		if args.ReadDataSubset != h.Value.CheckSubset {
			return agent.Result{}, agent.ErrInvalidCommand
		}
		output, err = client.Check(ctx, args.ReadDataSubset)
	case KindResticSSHRestore:
		if !args.Approved || !validHostPath(args.Target) || !regexp.MustCompile(`^[a-fA-F0-9]{6,64}$`).MatchString(args.Snapshot) {
			return agent.Result{}, agent.ErrInvalidCommand
		}
		remote.newTarget = args.Target
		_, err = remote.Run(ctx, restic.Request{Args: []string{"-r", h.Value.Repository, "restore", "--json", args.Snapshot, "--target", args.Target, "--overwrite", "never"},
			Env: map[string]string{"RESTIC_PASSWORD_FILE": "/tmp/dbcm-remote-password"}})
	default:
		return agent.Result{}, agent.ErrInvalidCommand
	}
	if err != nil {
		result.Status, result.Code, result.Message = agent.StatusFailed, agent.CodeIO, err.Error()
		return result, nil
	}
	if output != nil {
		result.Output, _ = json.Marshal(output)
	}
	return result, nil
}

type sshResticRunner struct {
	host       provision.Host
	credential provision.Credential
	password   string
	commands   provision.CommandRunner
	newTarget  string
}

func sshQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

type resticExit int

func (e resticExit) Error() string { return "restic: remote command failed" }
func (e resticExit) ExitCode() int { return int(e) }

// Run sends secret values only over the authenticated SSH session's stdin.
// The remote shell creates a 0600 temporary password file and passes backend
// variables only to its restic child. Neither the SSH command nor argv holds
// a credential. The pinned key check is done by provision.SSHRunner.
func (r *sshResticRunner) Run(ctx context.Context, req restic.Request) (restic.Result, error) {
	if r.commands == nil || strings.ContainsAny(r.password, "\x00\r\n") || len(req.Args) < 3 {
		return restic.Result{}, agent.ErrInvalidCommand
	}
	names := make([]string, 0, len(req.Env))
	for k := range req.Env {
		if k != "RESTIC_PASSWORD_FILE" {
			if !regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`).MatchString(k) {
				return restic.Result{}, agent.ErrInvalidCommand
			}
			names = append(names, k)
		}
	}
	slices.Sort(names)
	var script strings.Builder
	script.WriteString("set -eu\numask 077\nIFS= read -r DBCM_RESTIC_PW\nDBCM_RESTIC_PW_FILE=$(mktemp)\ntrap 'rm -f \"$DBCM_RESTIC_PW_FILE\"' EXIT\nprintf '%s' \"$DBCM_RESTIC_PW\" > \"$DBCM_RESTIC_PW_FILE\"\nunset DBCM_RESTIC_PW\n")
	stdin := r.password + "\n"
	for i, name := range names {
		value := req.Env[name]
		if strings.ContainsAny(value, "\x00\r\n") {
			return restic.Result{}, agent.ErrInvalidCommand
		}
		fmt.Fprintf(&script, "IFS= read -r DBCM_RESTIC_C%d\n", i)
		stdin += value + "\n"
	}
	if r.newTarget != "" {
		if !validHostPath(r.newTarget) {
			return restic.Result{}, agent.ErrInvalidCommand
		}
		q := sshQuote(r.newTarget)
		fmt.Fprintf(&script, "test ! -e %s && test ! -L %s && mkdir -m 700 -- %s\n", q, q, q)
	}
	script.WriteString("if env -i PATH=/usr/local/bin:/usr/bin:/bin RESTIC_PASSWORD_FILE=\"$DBCM_RESTIC_PW_FILE\"")
	for i, name := range names {
		fmt.Fprintf(&script, " \"%s=$DBCM_RESTIC_C%d\"", name, i)
	}
	script.WriteString(" restic")
	for _, arg := range req.Args {
		script.WriteByte(' ')
		script.WriteString(sshQuote(arg))
	}
	if len(req.Args) > 2 && req.Args[2] == "backup" {
		// The SSH transport bounds output at 64 KiB. Restic's quiet mode
		// suppresses frequent status events while retaining its JSON summary.
		script.WriteString(" --quiet")
	}
	script.WriteString(" 2>/dev/null; then exit 0; else DBCM_RESTIC_RC=$?; printf '\\n__DBCM_RESTIC_EXIT__:%s\\n' \"$DBCM_RESTIC_RC\"; exit 0; fi\n")
	out, err := r.commands.Run(ctx, r.host, r.credential, script.String(), stdin)
	if err != nil {
		return restic.Result{}, errors.New("restic: SSH command failed")
	}
	const marker = "\n__DBCM_RESTIC_EXIT__:"
	if pos := strings.LastIndex(out, marker); pos >= 0 {
		code, parseErr := strconv.Atoi(strings.TrimSpace(out[pos+len(marker):]))
		if parseErr == nil && code != 0 {
			return restic.Result{Stdout: []byte(out[:pos])}, resticExit(code)
		}
	}
	return restic.Result{Stdout: []byte(out)}, nil
}
