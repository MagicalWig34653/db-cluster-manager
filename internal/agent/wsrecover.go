// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// KindWsrepRecover recovers the last committed Galera position of a stopped
// node from its InnoDB data (`mariadbd --wsrep-recover`) in a one-off
// container on the volume's host (args: WsrepRecoverArgs). It is the
// position source the full-outage diagnosis needs after a crash, when
// grastate.dat says seqno -1.
//
// It is not read-only: mariadbd performs InnoDB crash recovery on the data,
// as every start of the node would. The agent verifies the identity marker
// and the owned volume first, refuses while any running container on the
// host mounts the volume, and runs the container without network, as the
// database user, with a cluster address that cannot reach anything (so even
// an ignored --wsrep-recover could not join or bootstrap a cluster). The
// command line is built here; the manager only names the image, the user
// and the volume.
const KindWsrepRecover Kind = "wsrep-recover"

// DefaultGaleraProvider is the provider library of the supported images.
const DefaultGaleraProvider = "/usr/lib/galera/libgalera_smm.so"

// wsrepRecoverTarget is where the volume is mounted in the one-off
// container (galera.VolumeTarget, the database services' mount point).
const wsrepRecoverTarget = "/dbcm"

// ContainerRunner runs one-off local containers. internal/dockerclient
// implements it; an executor whose DockerAPI does not refuses
// KindWsrepRecover as not configured.
type ContainerRunner interface {
	// RunContainer creates, starts and waits for the container, returns
	// its exit code and output and removes it (never its volumes).
	RunContainer(ctx context.Context, r ContainerRun) (ContainerResult, error)
	// RunningContainersWithVolume lists running containers on this host
	// that mount the named volume.
	RunningContainersWithVolume(ctx context.Context, volume string) ([]string, error)
}

// ContainerRun describes a one-off container without network.
type ContainerRun struct {
	Image       string
	Entrypoint  []string
	Cmd         []string
	User        string // "uid:gid"
	Labels      map[string]string
	Mounts      []Mount
	MemoryBytes int64
}

// ContainerResult is the outcome of a one-off container.
type ContainerResult struct {
	ExitCode int64
	Output   []byte // stdout and stderr, bounded
}

// WsrepRecoverArgs are the arguments of KindWsrepRecover.
type WsrepRecoverArgs struct {
	VolumeRoot string   `json:"volume_root"`
	Volume     string   `json:"volume"` // Docker volume name; VolumeRoot is VolumeBase/Volume
	Expected   Identity `json:"expected"`
	Image      string   `json:"image"`
	UID        int      `json:"uid"`
	GID        int      `json:"gid"`
	DataDir    string   `json:"data_dir,omitempty"` // relative to the volume root; default data
	Provider   string   `json:"provider,omitempty"` // default DefaultGaleraProvider
}

// WsrepRecoverOutput is the output of KindWsrepRecover.
type WsrepRecoverOutput struct {
	Position *galera.Position `json:"position,omitempty"`
	ExitCode int64            `json:"exit_code"`
	// LogTail is the end of the mariadbd output (no secrets: the command
	// line carries none).
	LogTail string `json:"log_tail,omitempty"`
}

func checkWsrepRecover(a *WsrepRecoverArgs, volumeBase string) error {
	if !volumeName.MatchString(a.Volume) {
		return invalid("volume name %q", a.Volume)
	}
	if err := checkVolumeRoot(a.VolumeRoot, volumeBase); err != nil {
		return err
	}
	if want := filepath.Join(volumeBase, a.Volume); a.VolumeRoot != want {
		return invalid("volume_root %q must be %q", a.VolumeRoot, want)
	}
	if a.Image == "" || strings.ContainsAny(a.Image, " \t\n") {
		return invalid("image %q", a.Image)
	}
	if a.UID <= 0 || a.GID <= 0 {
		return invalid("uid %d / gid %d must be positive (never root)", a.UID, a.GID)
	}
	if a.DataDir != "" && !filepath.IsLocal(a.DataDir) {
		return invalid("data_dir %q must be a local relative path", a.DataDir)
	}
	if a.Provider != "" && (!path.IsAbs(a.Provider) || path.Clean(a.Provider) != a.Provider || strings.ContainsAny(a.Provider, " \t\n")) {
		return invalid("provider %q must be a clean absolute path", a.Provider)
	}
	return nil
}

// wsrepRecoverCmd is the mariadbd command line (after the entrypoint).
func wsrepRecoverCmd(dataDir, provider string) []string {
	return []string{
		"--wsrep-recover",
		"--datadir=" + path.Join(wsrepRecoverTarget, dataDir),
		"--socket=/tmp/dbcm-wsrep-recover.sock",
		"--skip-networking",
		"--wsrep_on=ON",
		"--wsrep_provider=" + provider,
		// mariadbd refuses to start Galera without a cluster address
		// (lab E6); this one is unreachable from a container without
		// network, and an empty "gcomm://" (bootstrap) is never used.
		"--wsrep_cluster_address=gcomm://127.0.0.1",
		"--wsrep_sst_method=mariabackup",
		"--binlog_format=ROW",
	}
}

func (e *Executor) wsrepRecover(ctx context.Context, a WsrepRecoverArgs) Result {
	cr, ok := e.docker.(ContainerRunner)
	if !ok {
		return Result{Status: StatusRefused, Code: CodeNotConfigured, Message: "agent cannot run one-off containers"}
	}
	root, res, ok := e.resolveVolumeRoot(a.VolumeRoot)
	if !ok {
		return res
	}
	if err := Verify(root, a.Expected); err != nil {
		return outcome(err)
	}
	vol, res, ok := e.owned(ctx, KindVolume, a.Volume)
	if !ok {
		return res
	}
	if vol.Mountpoint == "" || filepath.Clean(vol.Mountpoint) != a.VolumeRoot {
		return Result{Status: StatusRefused, Code: CodeVolumeMismatch,
			Message: fmt.Sprintf("volume %q mountpoint %q is not %s", a.Volume, vol.Mountpoint, a.VolumeRoot)}
	}
	running, err := cr.RunningContainersWithVolume(ctx, a.Volume)
	if err != nil {
		return outcome(err)
	}
	if len(running) > 0 {
		return Result{Status: StatusRefused, Code: CodeVolumeInUse,
			Message: fmt.Sprintf("%v: %d running container(s) mount %s", ErrVolumeInUse, len(running), a.Volume)}
	}
	dataDir := a.DataDir
	if dataDir == "" {
		dataDir = DefaultDataDir
	}
	provider := a.Provider
	if provider == "" {
		provider = DefaultGaleraProvider
	}
	r, err := cr.RunContainer(ctx, ContainerRun{
		Image:      a.Image,
		Entrypoint: []string{"mariadbd"},
		Cmd:        wsrepRecoverCmd(dataDir, provider),
		User:       strconv.Itoa(a.UID) + ":" + strconv.Itoa(a.GID),
		Labels: map[string]string{LabelInstallation: e.cfg.InstallationID, LabelNode: a.Expected.NodeUUID,
			LabelCluster: a.Expected.ClusterID, LabelJob: "wsrep-recover"},
		Mounts:      []Mount{{Type: "volume", Source: a.Volume, Target: wsrepRecoverTarget}},
		MemoryBytes: 1 << 30,
	})
	if err != nil {
		return outcome(err)
	}
	out := WsrepRecoverOutput{ExitCode: r.ExitCode, LogTail: tailLines(string(r.Output), 12)}
	if p, err := galera.ParseWsrepRecover(string(r.Output)); err == nil {
		out.Position = &p
	}
	if r.ExitCode != 0 || out.Position == nil {
		res := succeeded(out)
		res.Status, res.Code = StatusFailed, CodeIO
		res.Message = fmt.Sprintf("mariadbd --wsrep-recover exited %d without a recovered position", r.ExitCode)
		return res
	}
	return succeeded(out)
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
