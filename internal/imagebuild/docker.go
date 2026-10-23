// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

// Command is one external process invocation.
type Command struct {
	Name   string
	Args   []string
	Dir    string
	Env    []string // appended to the minimal inherited environment
	Stdout io.Writer
	Stderr io.Writer
}

// CommandRunner executes commands. ExecRunner is the real one; tests use a
// fake that records the command.
type CommandRunner interface {
	Run(ctx context.Context, c Command) error
}

// ExecRunner runs commands with os/exec. It is compiled but never executed
// by the tests of this package.
type ExecRunner struct{}

// Run implements CommandRunner.
func (ExecRunner) Run(ctx context.Context, c Command) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, c.Env...)
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	return cmd.Run()
}

// DockerBuilder builds with `docker buildx build` (BuildKit). Status: the
// command line is unit-tested; it has never been run against a Docker
// daemon, and the flags were written from documentation, not verified on a
// real buildx.
type DockerBuilder struct {
	// Binary defaults to "docker".
	Binary string
	Runner CommandRunner
	// WorkDir is where temporary build contexts are created; empty selects
	// the OS temp directory.
	WorkDir string
}

var _ Builder = (*DockerBuilder)(nil)

// ErrBuild wraps failures of the build process itself.
var ErrBuild = errors.New("imagebuild: build failed")

// BuildArgs returns the argument list (without the binary) for a build in
// contextDir that writes its metadata to metadataFile. It contains no
// credentials: registry auth travels in a temporary docker config.
func BuildArgs(in BuildInput, contextDir, metadataFile string) ([]string, error) {
	if len(in.Platforms) == 0 {
		return nil, invalid("no platform given")
	}
	for _, p := range in.Platforms {
		if !platformRe.MatchString(p) {
			return nil, invalid("platform %q is not supported", p)
		}
	}
	if len(in.Platforms) > 1 && !in.Push {
		return nil, invalid("a multi-platform build can only be pushed, not loaded locally")
	}
	if in.Reference == "" || strings.ContainsAny(in.Reference, " \t\n") || strings.HasPrefix(in.Reference, "-") {
		return nil, invalid("malformed image reference")
	}
	args := []string{
		"buildx", "build",
		"--platform", strings.Join(in.Platforms, ","),
		"--file", filepath.Join(contextDir, "Dockerfile"),
		"--tag", in.Reference,
		"--metadata-file", metadataFile,
		"--progress", "plain",
	}
	if in.Push {
		args = append(args, "--push")
	} else {
		args = append(args, "--load")
	}
	return append(args, contextDir), nil
}

// WriteContext writes the Dockerfile and the context files below dir.
func WriteContext(dir string, r Rendered) error {
	write := func(name string, mode uint32, content string) error {
		clean := path.Clean(name)
		if clean != name || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("imagebuild: unsafe context path %q", name)
		}
		full := filepath.Join(dir, filepath.FromSlash(clean))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		return os.WriteFile(full, []byte(content), os.FileMode(mode))
	}
	if err := write("Dockerfile", 0o644, r.Dockerfile); err != nil {
		return err
	}
	for _, f := range r.Files {
		if err := write(f.Path, f.Mode, f.Content); err != nil {
			return err
		}
	}
	return nil
}

// Build implements Builder.
func (b *DockerBuilder) Build(ctx context.Context, in BuildInput) (BuildOutput, error) {
	if b.Runner == nil {
		return BuildOutput{}, errors.New("imagebuild: DockerBuilder needs a Runner")
	}
	root, err := os.MkdirTemp(b.WorkDir, "dbcm-build-")
	if err != nil {
		return BuildOutput{}, err
	}
	defer os.RemoveAll(root)
	ctxDir := filepath.Join(root, "context")
	if err := os.Mkdir(ctxDir, 0o700); err != nil {
		return BuildOutput{}, err
	}
	if err := WriteContext(ctxDir, in.Rendered); err != nil {
		return BuildOutput{}, err
	}
	meta := filepath.Join(root, "metadata.json")
	args, err := BuildArgs(in, ctxDir, meta)
	if err != nil {
		return BuildOutput{}, err
	}
	bin := b.Binary
	if bin == "" {
		bin = "docker"
	}
	cmd := Command{Name: bin, Args: args, Dir: ctxDir, Stdout: in.Log, Stderr: in.Log}
	if in.Auth != nil && in.Push {
		cfgDir := filepath.Join(root, "dockerconfig")
		if err := writeDockerConfig(cfgDir, *in.Auth); err != nil {
			return BuildOutput{}, err
		}
		cmd.Env = []string{"DOCKER_CONFIG=" + cfgDir}
	}
	if err := b.Runner.Run(ctx, cmd); err != nil {
		return BuildOutput{}, fmt.Errorf("%w: %v", ErrBuild, err)
	}
	digest, err := readMetadataDigest(meta)
	if err != nil {
		return BuildOutput{}, err
	}
	return BuildOutput{ImageDigest: digest, BaseDigests: baseDigests(in.Rendered), SBOM: SBOMNotGenerated}, nil
}

// writeDockerConfig writes a docker config.json holding the registry
// credential, readable by the owner only. The directory is removed with the
// build's temporary root.
func writeDockerConfig(dir string, a RegistryAuth) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	auth := base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password.Reveal()))
	cfg := map[string]any{"auths": map[string]any{a.Server: map[string]string{"auth": auth}}}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
}

func readMetadataDigest(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%w: no build metadata written", ErrBuild)
	}
	var m struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(b, &m); err != nil || !digestRe.MatchString(m.Digest) {
		return "", fmt.Errorf("%w: build metadata has no image digest", ErrBuild)
	}
	return m.Digest, nil
}
