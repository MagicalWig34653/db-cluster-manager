// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/passkey"
)

// Paths inside the manager container that deploy/stack.yaml and
// deploy/compose.yaml mount; scripts/check_deploy.py keeps them in step.
const (
	containerConfigPath  = "/etc/dbcm/manager.json"
	containerDataDir     = "/var/lib/dbcm"
	containerBackendFile = "/run/secrets/dbcm-backend.json"
	containerPeerCA      = "/run/secrets/dbcm-peer-ca.pem"
	containerPeerCert    = "/run/secrets/dbcm-peer.pem"
	containerPeerKey     = "/run/secrets/dbcm-peer-key.pem"
)

// Default ports of a generated configuration. The metastore does not use
// 7946: with host networking that port belongs to the Swarm gossip of dockerd.
const (
	defaultEnrollPort    = 7444
	defaultMetastorePort = 7445
	defaultForwardPort   = 7447
	defaultAgentPort     = 7443
)

var installationPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// initOptions are the inputs of `manager init`.
type initOptions struct {
	RPID          string
	Origin        string
	DisplayName   string
	Installation  string
	ManagerAddr   string
	NodeID        string
	Peers         string
	DataDir       string
	VolumeBase    string
	AgentImage    string
	JobImage      string
	Backup        bool
	ImageBuild    bool
	RecipientKey  string
	RecoveryOut   string
	ConfigOut     string
	MetastorePort int
	EnrollPort    int
}

// secretNames are the Swarm secret and network names the configuration
// refers to; the operator creates them (see the printed instructions).
type secretNames struct {
	Root, Monitor, Backup, BackupCred, Backend, RecoveryKey, InspectNet, ClusterNet string
}

func namesFor(inst string) secretNames {
	p := "dbcm-" + inst + "-"
	return secretNames{Root: p + "root-pw", Monitor: p + "monitor-pw", Backup: p + "backup-pw", BackupCred: p + "backup-cred",
		Backend: p + "backend", RecoveryKey: p + "recovery-key", InspectNet: p + "inspect", ClusterNet: p + "net"}
}

// runInit implements `manager init`: it writes a configuration skeleton
// for managed mode in a Swarm (deploy/stack.yaml) and prints first-run
// instructions to stderr. It never prints secret values; the only secret it
// can create is a recovery key, written to -recovery-key-out (0600) with
// only its public half embedded in the configuration.
func runInit(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("manager init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o initOptions
	fs.StringVar(&o.RPID, "rp-id", "", "WebAuthn relying party ID: the host name users open the UI on (required)")
	fs.StringVar(&o.Origin, "origin", "", "UI origin (default https://<rp-id>)")
	fs.StringVar(&o.DisplayName, "display-name", "cluster-manager", "relying party display name")
	fs.StringVar(&o.Installation, "installation", "", "installation ID, [a-z0-9-], at most 31 characters (default: random)")
	fs.StringVar(&o.ManagerAddr, "manager-addr", "", "address (IP or DNS name) of this manager's Swarm node that agents reach (required)")
	fs.StringVar(&o.NodeID, "node-id", "manager-1", "metastore replica ID of this manager")
	fs.StringVar(&o.Peers, "peers", "", "HA only: all replicas as id=host:port,... (enables keyring and peer TLS)")
	fs.StringVar(&o.DataDir, "data-dir", containerDataDir, "manager data directory (the volume inside the container)")
	fs.StringVar(&o.VolumeBase, "volume-base", "/var/lib/dbcm/volumes", "host directory below which database volumes live (every host)")
	fs.StringVar(&o.AgentImage, "agent-image", "db-cluster-manager:local", "image of the agent service (the manager image; pin by digest)")
	fs.StringVar(&o.JobImage, "job-image", "db-cluster-manager-jobs:local", "backup/restore job image (deploy/Dockerfile.jobs; pin by digest)")
	fs.BoolVar(&o.Backup, "backup", false, "configure backups (backend secret, job image, recovery key)")
	fs.BoolVar(&o.ImageBuild, "image-build", false, "enable the image build service (needs a docker CLI with buildx and a daemon the manager can use; the shipped manager image has neither)")
	fs.StringVar(&o.RecipientKey, "recipient-key", "", "with -backup: public half of an existing recovery key")
	fs.StringVar(&o.RecoveryOut, "recovery-key-out", "", "with -backup: generate a recovery key into this new file (0600) and use its public half")
	fs.StringVar(&o.ConfigOut, "out", "", "write the configuration to this new file instead of stdout")
	fs.IntVar(&o.MetastorePort, "metastore-port", defaultMetastorePort, "metastore port")
	fs.IntVar(&o.EnrollPort, "enroll-port", defaultEnrollPort, "agent enrollment port")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("manager init: unexpected argument %q", fs.Arg(0))
	}
	if o.Installation == "" {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		o.Installation = "i" + hex.EncodeToString(b)
	}
	if o.Backup && o.RecoveryOut != "" {
		if o.RecipientKey != "" {
			return errors.New("manager init: -recipient-key and -recovery-key-out exclude each other")
		}
		pub, err := writeRecoveryKey(o.RecoveryOut)
		if err != nil {
			return err
		}
		o.RecipientKey = pub
	}
	cfg, err := buildInitConfig(o)
	if err != nil {
		return err
	}
	b, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	if o.ConfigOut == "" {
		if _, err := stdout.Write(b); err != nil {
			return err
		}
	} else if err := writeNewFile(o.ConfigOut, b, 0o600); err != nil {
		return err
	}
	return printInstructions(stderr, o, cfg)
}

// buildInitConfig derives the configuration from o and checks it with the
// same validation the manager applies on start.
func buildInitConfig(o initOptions) (fileConfig, error) {
	if o.RPID == "" || o.ManagerAddr == "" {
		return fileConfig{}, errors.New("manager init needs -rp-id and -manager-addr")
	}
	if !installationPattern.MatchString(o.Installation) {
		return fileConfig{}, fmt.Errorf("manager init: installation ID %q must match %s", o.Installation, installationPattern)
	}
	if strings.ContainsAny(o.ManagerAddr, ":/ ") && net.ParseIP(o.ManagerAddr) == nil {
		return fileConfig{}, fmt.Errorf("manager init: -manager-addr %q must be a host name or IP without port", o.ManagerAddr)
	}
	if o.Backup && o.RecipientKey == "" {
		return fileConfig{}, errors.New("manager init: -backup needs -recipient-key or -recovery-key-out")
	}
	if !o.Backup && (o.RecipientKey != "" || o.RecoveryOut != "") {
		return fileConfig{}, errors.New("manager init: -recipient-key and -recovery-key-out need -backup")
	}
	if o.RecipientKey != "" {
		if _, err := backup.ParseStreamPublicKey(o.RecipientKey); err != nil {
			return fileConfig{}, fmt.Errorf("manager init: -recipient-key: %w", err)
		}
	}
	origin := o.Origin
	if origin == "" {
		origin = "https://" + o.RPID
	}
	n := namesFor(o.Installation)
	host := o.ManagerAddr
	enroll := net.JoinHostPort(host, fmt.Sprint(o.EnrollPort))
	mc := &managerConfig{
		NodeID:    o.NodeID,
		DataDir:   o.DataDir,
		Bind:      net.JoinHostPort("127.0.0.1", fmt.Sprint(o.MetastorePort)),
		Peers:     []peerConfig{},
		Bootstrap: true,
		AgentTransport: &agentTransportConfig{
			EnrollListen: enroll, ManagerURL: "https://" + enroll, ServerDNS: []string{}, ServerIPs: []string{},
			AgentPort: defaultAgentPort, MonitorUser: galera.DefaultMonitorUser, MonitorSecret: n.Monitor,
			DBSecrets: []string{n.Root, n.Backup},
		},
		Settings: &settingsConfig{
			InstallationID: o.Installation, VolumeBase: o.VolumeBase, AgentImage: o.AgentImage,
			Accounts: &accountsConfig{MonitorUser: galera.DefaultMonitorUser, MonitorSecret: n.Monitor,
				BackupUser: galera.DefaultBackupUser, BackupSecret: n.Backup},
		},
		// OS patch management ships disabled and explicit; see patch.go.
		Patch: &patchConfig{Enabled: false, AgentPort: defaultPatchPort, PollIntervalSeconds: defaultPatchInterval},
		// Image updates ship disabled and explicit; see imageupdate.go.
		ImageUpdate: &imageUpdateConfig{Enabled: false, PollIntervalSeconds: defaultImageUpdateInterval},
		// The image composer and the fresh-server bootstrap ship disabled;
		// see bootstrap.go. Enabling the bootstrap also needs agentRelease.
		ImageComposer:   &imageComposerConfig{Enabled: false},
		ServerBootstrap: &bootstrapConfig{Enabled: false, SweepSeconds: defaultBootstrapSweep},
	}
	if net.ParseIP(host) == nil {
		mc.AgentTransport.ServerDNS = []string{host}
	}
	if o.Peers != "" {
		peers, err := parsePeers(o.Peers)
		if err != nil {
			return fileConfig{}, err
		}
		self := ""
		for _, p := range peers {
			if p.ID == o.NodeID {
				self = p.Address
			}
		}
		if self == "" {
			return fileConfig{}, fmt.Errorf("manager init: -peers does not contain -node-id %q", o.NodeID)
		}
		mc.Peers, mc.Bind, mc.Advertise, mc.Keyring = peers, self, self, true
		for i := range mc.Peers {
			host, _, _ := net.SplitHostPort(mc.Peers[i].Address)
			mc.Peers[i].APIAddress = net.JoinHostPort(host, fmt.Sprint(defaultForwardPort))
		}
		selfHost, _, _ := net.SplitHostPort(self)
		mc.APIForwardBind = net.JoinHostPort(selfHost, fmt.Sprint(defaultForwardPort))
		mc.PeerTLS = &peerTLSConfig{Installation: o.Installation, CAFile: containerPeerCA,
			CertFile: filepath.Join(o.DataDir, "peer", "peer-bundle.pem"), KeyFile: filepath.Join(o.DataDir, "peer", "peer-bundle.pem"),
			Renewal: &peerRenewalConfig{CAFile: "/run/secrets/dbcm-peer-ca-sealed", PassphraseFile: "/run/secrets/dbcm-peer-ca-passphrase"}}
	}
	if o.Backup {
		mc.Backup = &backupConfig{BackendConfigFile: containerBackendFile, ScheduleIntervalSeconds: 60}
		mc.Settings.Backup = &backupSettings{JobImage: o.JobImage, BackendSecret: n.Backend, DBCredentialSecret: n.BackupCred,
			RecipientKey: o.RecipientKey, KeySecret: n.RecoveryKey, InspectionNetwork: n.InspectNet}
	}
	if o.ImageBuild {
		mc.ImageBuild = &imageBuildConfig{DockerBinary: "docker", MaxLogBytes: imagebuild.DefaultMaxLogBytes}
	}
	cfg := fileConfig{Auth: authConfig{Enabled: true, RPID: o.RPID, RPDisplayName: o.DisplayName, Origins: []string{origin}}, Manager: mc}
	if err := validateConfigStatic(cfg); err != nil {
		return fileConfig{}, fmt.Errorf("manager init: %w", err)
	}
	return cfg, nil
}

// parsePeers parses id=host:port,... with distinct IDs and addresses.
func parsePeers(s string) ([]peerConfig, error) {
	var out []peerConfig
	seen := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		id, addr, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok || !peerIDPattern.MatchString(id) {
			return nil, fmt.Errorf("manager init: -peers entry %q must be id=host:port", item)
		}
		if _, port, err := net.SplitHostPort(addr); err != nil || port == "" {
			return nil, fmt.Errorf("manager init: -peers entry %q: address must be host:port", item)
		}
		if seen[id] || seen[addr] {
			return nil, fmt.Errorf("manager init: -peers entry %q is a duplicate", item)
		}
		seen[id], seen[addr] = true, true
		out = append(out, peerConfig{ID: id, Address: addr})
	}
	if len(out) < 3 || len(out)%2 == 0 {
		return nil, errors.New("manager init: -peers needs an odd number of at least 3 replicas")
	}
	return out, nil
}

// validateConfigStatic runs the checks of the manager start that need no
// files, listeners or network: auth, manager section, agent transport and
// configured settings.
func validateConfigStatic(cfg fileConfig) error {
	if cfg.Auth.Enabled {
		pk := passkey.Config{RPID: cfg.Auth.RPID, RPDisplayName: cfg.Auth.RPDisplayName, Origins: cfg.Auth.Origins}
		if err := pk.Validate(); err != nil {
			return fmt.Errorf("auth config: %w", err)
		}
	}
	mc := cfg.Manager
	if mc == nil {
		return nil
	}
	if err := mc.validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(mc.DataDir) {
		return errors.New("manager.dataDir must be an absolute path")
	}
	if len(mc.Peers) > 0 && mc.Advertise == "" {
		return errors.New("manager.advertise is required with manager.peers")
	}
	if p := mc.PeerTLS; p != nil && (p.Installation == "" || p.CAFile == "" || p.CertFile == "" || p.KeyFile == "") {
		return errors.New("manager.peerTLS needs installation, caFile, certFile and keyFile")
	}
	if at := mc.AgentTransport; at != nil {
		if _, err := at.validate(); err != nil {
			return err
		}
	}
	if s := mc.Settings; s != nil {
		if s.VolumeBase != "" && (!filepath.IsAbs(s.VolumeBase) || filepath.Clean(s.VolumeBase) != s.VolumeBase) {
			return fmt.Errorf("manager.settings.volumeBase %q must be a clean absolute path", s.VolumeBase)
		}
		if s.MonitorSocket != "" && !filepath.IsLocal(s.MonitorSocket) {
			return fmt.Errorf("manager.settings.monitorSocket %q must be relative to the volume root", s.MonitorSocket)
		}
		if b := s.Backup; b != nil && b.RecipientKey != "" {
			if _, err := backup.ParseStreamPublicKey(b.RecipientKey); err != nil {
				return fmt.Errorf("manager.settings.backup.recipientKey: %w", err)
			}
		}
	}
	return nil
}

func marshalConfig(cfg fileConfig) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeNewFile creates path exclusively (never overwrites).
func writeNewFile(path string, b []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("manager init: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeRecoveryKey generates a backup recovery key into the new file path
// and returns its public half. The private key is never printed.
func writeRecoveryKey(path string) (string, error) {
	k, err := backup.GenerateRecoveryKey()
	if err != nil {
		return "", err
	}
	pub, err := k.StreamPublicKey()
	if err != nil {
		return "", err
	}
	if err := writeNewFile(path, []byte(k.String()+"\n"), 0o600); err != nil {
		return "", err
	}
	return pub.String(), nil
}

var instructions = template.Must(template.New("init").Parse(`
First-run instructions for installation {{.Inst}} (deploy/stack.yaml; see docs/INSTALL.md).
No secret values are printed here. Create each secret from a file or stdin you control.

1. On EVERY Swarm node: create the agent state directory and the volume base:
     install -d -m 0755 /var/lib/dbcm-agent {{.VolumeBase}}
2. On a Swarm manager node, create the database secrets and the cluster network
   (labelled with the installation):
     for s in {{.N.Root}} {{.N.Monitor}}{{if not .Backup}} {{.N.Backup}}{{end}}; do
       head -c 24 /dev/urandom | base64 | tr -d '\n' | docker secret create --label dbcm.installation={{.Inst}} "$s" -
     done
     docker network create --driver overlay --attachable --label dbcm.installation={{.Inst}} {{.N.ClusterNet}}
{{- if .Backup}}
   Backups additionally need (contents: see docs/INSTALL.md, "Backups: targets, secrets and stack changes").
   The backup account password secret {{.N.Backup}} and the option file {{.N.BackupCred}} must hold the SAME
   password, so create both from one value instead of the loop's random one:
     BPW=$(head -c 24 /dev/urandom | base64 | tr -d '\n')
     printf '%s' "$BPW" | docker secret create --label dbcm.installation={{.Inst}} {{.N.Backup}} -
     printf '[client]\nuser=dbcm_backup\npassword=%s\n' "$BPW" | docker secret create --label dbcm.installation={{.Inst}} {{.N.BackupCred}} -
     docker secret create --label dbcm.installation={{.Inst}} {{.N.Backend}} <backend.json>
     docker secret create --label dbcm.installation={{.Inst}} {{.N.RecoveryKey}} <recovery key file{{if .RecoveryOut}} {{.RecoveryOut}}{{end}}>
     docker network create --driver overlay --attachable --label dbcm.installation={{.Inst}} {{.N.InspectNet}}
   Keep a copy of the recovery key outside the Swarm; it is the only way to read backups.
   The manager reads {{.N.Backend}} too: mount it at /run/secrets/dbcm-backend.json in every
   manager service (the single-replica stack has an optional commented block).
{{- end}}
3. Store this configuration as a Swarm config (configs are immutable; use a new name per change):
{{- if .HA}}
     docker config create dbcm-manager-config-{{.NodeID}} <this file>
{{- else}}
     docker config create dbcm-manager-config-1 <this file>
{{- end}}
{{- if .ImageBuild}}
   Image builds (manager.imageBuild) run "docker buildx build" as the manager process. The shipped manager image
   contains no docker CLI and the stack gives it no BuildKit; provide both before approving a build (untested).
{{- end}}
{{- if .HA}}
   HA (not run as Swarm services): issue peer certificates once with manager -issue-peer-certs.
   Copy only {{.NodeID}}/peer-bundle.pem into this node's local /var/lib/dbcm/peer/ (uid 65532,
   mode 0600). Create three external Swarm secrets from ca.pem, ca.sealed and a separate
   passphrase file; keep offline recovery copies. These secrets give every manager node
   online peer-CA signing authority. Repeat init with the same -peers and installation
   for each -node-id. See docs/INSTALL.md ("Three-replica HA stack").
{{- end}}
{{- if .HA}}
4. Set DBCM_MANAGER_NODE_1..3, DBCM_CONFIG_NAME_1..3, DBCM_PEER_CA_NAME,
   DBCM_PEER_CA_SEALED_NAME, DBCM_PEER_CA_PASSPHRASE_NAME, DBCM_DOCKER_GID
   and DBCM_IMAGE, then deploy: docker stack deploy -c deploy/stack-ha.yaml dbcm
   Each service needs a different manager node, config and local volume.
5. Read the first leader's one-time setup token from its manager service log,
   then open {{.Origin}} through your TLS proxy and create the administrator.
   Register the other replicas' data-key recipients before relying on failover.
{{- else}}
4. Deploy on a Swarm manager node, pinned to the node at {{.Addr}}:
     DBCM_MANAGER_NODE=<hostname of that node> DBCM_DOCKER_GID=$(stat -c %g /var/run/docker.sock) \
       DBCM_CONFIG_NAME=dbcm-manager-config-1 docker stack deploy -c deploy/stack.yaml dbcm
5. Read the one-time setup token from the service log (printed only while no administrator exists):
     docker service logs dbcm_manager 2>&1 | grep 'setup token'
   Then open {{.Origin}} through your TLS reverse proxy (to 127.0.0.1:8080 on that node) and create the first administrator.
{{- end}}
6. Register the Swarm in the UI with Docker endpoint unix:///var/run/docker.sock; agents enroll at {{.ManagerURL}}.
`))

func printInstructions(w io.Writer, o initOptions, cfg fileConfig) error {
	mc := cfg.Manager
	return instructions.Execute(w, map[string]any{
		"Inst": o.Installation, "VolumeBase": mc.Settings.VolumeBase, "N": namesFor(o.Installation), "Backup": o.Backup, "ImageBuild": o.ImageBuild,
		"RecoveryOut": o.RecoveryOut, "HA": len(mc.Peers) > 0, "NodeID": mc.NodeID, "Addr": o.ManagerAddr,
		"Origin": cfg.Auth.Origins[0], "ManagerURL": mc.AgentTransport.ManagerURL,
	})
}
