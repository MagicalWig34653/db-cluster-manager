// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager"
)

// runMetastoreImport implements `manager metastore-import`: it restores a
// backup of the manager's own metastore (POST /settings/metastore-backups)
// into the EMPTY metastore of this replica, offline. Run it with the
// manager stopped, as the same user that runs the manager, on the host that
// will become the first replica of the restored installation. The recovery
// key is read from a file and never from the command line or the manager.
//
// Steps: the sealed export is opened with the recovery key; a single-voter
// metastore node starts on the configured bind address; the documents are
// written (keyring first, with the data keys re-wrapped for this replica's
// identity); the node stops. Sessions, operations, approvals and events
// are not part of a backup, so nothing resumes by itself. Further replicas
// join through the normal membership procedure.
func runMetastoreImport(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("metastore-import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "manager JSON configuration (required)")
	in := fs.String("in", "", "sealed metastore backup object (metastore-*.dbcms, required)")
	keyFile := fs.String("recovery-key-file", "", "file with the backup recovery key (required; never pass the key itself)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || *in == "" || *keyFile == "" {
		return errors.New("metastore-import needs -config, -in and -recovery-key-file")
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	mc := cfg.Manager
	if mc == nil {
		return errors.New("metastore-import needs a config with a manager section")
	}
	if err := mc.validate(); err != nil {
		return err
	}
	metaDir := filepath.Join(mc.DataDir, "metastore")
	if entries, err := os.ReadDir(metaDir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty: an import only writes into a fresh metastore (move the old data directory away first)", metaDir)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	keyText, err := os.ReadFile(*keyFile)
	if err != nil {
		return fmt.Errorf("recovery key file: %w", err)
	}
	recovery, err := backup.ParseRecoveryKey(strings.TrimSpace(string(keyText)))
	if err != nil {
		return fmt.Errorf("recovery key file: %w", err)
	}
	data, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	exp, err := manager.OpenMetaExport(data, recovery)
	if err != nil {
		return fmt.Errorf("cannot open %s with this recovery key: %w", *in, err)
	}
	nodeID := mc.NodeID
	if nodeID == "" {
		nodeID = "manager-1"
	}
	ident, err := manager.LoadReplicaIdentity(filepath.Join(mc.DataDir, "replica.key"))
	if err != nil {
		return err
	}
	pub, err := ident.StreamPublicKey()
	if err != nil {
		return err
	}
	svc, err := manager.New(manager.Config{NodeID: nodeID, DataDir: metaDir, BindAddr: mc.Bind, AdvertiseAddr: mc.Advertise,
		Bootstrap: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		return err
	}
	defer svc.Close()
	deadline := time.Now().Add(30 * time.Second)
	for !svc.Node().IsLeader() {
		if time.Now().After(deadline) {
			return errors.New("the single-voter metastore did not become leader within 30 s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := svc.Node().Barrier(10 * time.Second); err != nil {
		return err
	}
	res, err := manager.ImportMetastore(svc.Store(), exp, recovery, nodeID, pub, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported %d documents of %d collections from a backup made %s (installation %q, revision %d)\n",
		res.Documents, res.Collections, exp.Created.Format(time.RFC3339), exp.Installation, exp.Revision)
	fmt.Fprintf(stdout, "data keys re-wrapped for replica %s: %s\n", nodeID, strings.Join(res.DataKeys, ", "))
	for _, w := range res.Warnings {
		fmt.Fprintf(stdout, "warning: %s\n", w)
	}
	fmt.Fprintln(stdout, "next: start the manager, log in, check clusters and hosts, then add further replicas (docs/guides/en/21-disaster-recovery.md)")
	return nil
}
