// SPDX-License-Identifier: AGPL-3.0-only

// Command pitrlab is the host-side helper of lab experiment E15
// (tests/p0/e15-pitr.sh, point-in-time recovery, ADR 0031). It exposes the
// library functions the lab script must not re-implement in shell, so the
// experiment checks the product code and not a copy of it:
//
//	pitrlab keygen FILE                    new recovery key (0600), prints the public key
//	pitrlab init-sql INSPECT_PW REPLAY_PW  the init file of a point-in-time inspection instance
//	pitrlab replay-opts REPLAY_PW PORT     the replay job's client option file
//	pitrlab stop-datetime RFC3339          the --stop-datetime for a target time
//	pitrlab propose DIR CLUSTER BASE AT    coverage and chain for AT over a dir target
//	                                       (BASE is the full backup's object name)
//
// It touches no infrastructure and no secret except the key file it creates.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/backup"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dbbackup"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pitrlab:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: see the command documentation")
	}
	a := args[1:]
	need := func(n int) error {
		if len(a) != n {
			return fmt.Errorf("%s needs %d argument(s)", args[0], n)
		}
		return nil
	}
	switch args[0] {
	case "keygen":
		if err := need(1); err != nil {
			return err
		}
		k, err := backup.GenerateRecoveryKey()
		if err != nil {
			return err
		}
		pub, err := k.StreamPublicKey()
		if err != nil {
			return err
		}
		f, err := os.OpenFile(a[0], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(f, k.String()); err != nil {
			_ = f.Close()
			return err
		}
		fmt.Println(pub.String())
		return f.Close()
	case "init-sql":
		if err := need(2); err != nil {
			return err
		}
		sql, err := dbbackup.RenderInspectionInitReplay(a[0], a[1])
		fmt.Print(sql)
		return err
	case "replay-opts":
		if err := need(2); err != nil {
			return err
		}
		var port int
		if _, err := fmt.Sscan(a[1], &port); err != nil {
			return err
		}
		opts, err := dbbackup.RenderReplayOptions(a[0], port)
		fmt.Print(opts)
		return err
	case "stop-datetime":
		if err := need(1); err != nil {
			return err
		}
		at, err := time.Parse(time.RFC3339, a[0])
		if err != nil {
			return err
		}
		fmt.Println(dbbackup.ReplayStopDatetime(at))
		return nil
	case "propose":
		if err := need(4); err != nil {
			return err
		}
		return propose(a[0], a[1], a[2], a[3])
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func propose(dir, cluster, baseObject, atStr string) error {
	at, err := time.Parse(time.RFC3339, atStr)
	if err != nil {
		return err
	}
	ctx := context.Background()
	be, err := backup.NewDirBackend(dir)
	if err != nil {
		return err
	}
	rc, err := be.Get(ctx, dbbackup.ManifestName(baseObject))
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(rc, 16<<20))
	_ = rc.Close()
	if err != nil {
		return err
	}
	base, err := dbbackup.DecodeManifest(raw)
	if err != nil {
		return err
	}
	archives, err := dbbackup.ListBinlogArchives(ctx, be, cluster, base.BackupID)
	if err != nil {
		return err
	}
	cov, err := dbbackup.CoverageOf(base, archives)
	if err != nil {
		return err
	}
	out := map[string]any{"backup": base.BackupID, "binlogBase": base.BinlogBase, "finished": base.Finished, "coverage": cov}
	if segs, err := dbbackup.ResolvePITR(base, archives, at); err != nil {
		out["covered"], out["reason"] = false, err.Error()
	} else {
		out["covered"], out["segments"] = true, len(segs)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
