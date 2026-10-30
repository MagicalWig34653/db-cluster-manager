// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
)

func iuRuntime(t *testing.T, section any) (*managedRuntime, error) {
	t.Helper()
	mgr := map[string]any{"dataDir": t.TempDir(), "bind": freeLoopback(t)}
	if section != nil {
		mgr["imageUpdate"] = section
	}
	cfg, err := loadConfig(writeConfig(t, map[string]any{
		"auth":    map[string]any{"enabled": true, "rpId": "db.example.test", "origins": []string{"https://db.example.test"}},
		"manager": mgr,
	}))
	if err != nil {
		return nil, err
	}
	rt, err := newManagedRuntime(cfg, quietLog(), &bytes.Buffer{})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = rt.Close() })
	waitUntil(t, "leadership", rt.svc.IsLeader)
	return rt, nil
}

func TestImageUpdateIsOffByDefault(t *testing.T) {
	for name, section := range map[string]any{"absent": nil, "disabled": map[string]any{"enabled": false}} {
		t.Run(name, func(t *testing.T) {
			rt, err := iuRuntime(t, section)
			if err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "api", func() bool { return rt.stack.Load() != nil })
			if rt.on("image-update") {
				t.Fatal("image update service runs although it was not enabled")
			}
			if rec := serve(t, newMux(rt), http.MethodGet, "/api/v1/image-updates/services", ""); rec.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401", rec.Code)
			}
		})
	}
}

func TestImageUpdateEnabledWiresStoreAndScope(t *testing.T) {
	rt, err := iuRuntime(t, map[string]any{"enabled": true})
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "image update service", func() bool { return rt.imageUpdate.Load() != nil })
	svc := rt.imageUpdate.Load()
	if svc.Journal == nil || svc.Term == nil || svc.Driver == nil || svc.Source == nil || svc.Resolver == nil || svc.InstallationFunc == nil {
		t.Fatalf("incomplete wiring: %+v", svc)
	}
	if _, err := svc.Term(); err != nil {
		t.Fatal(err)
	}
	// Without installation settings nothing is in scope.
	if views, _ := svc.Services(context.Background()); len(views) != 0 {
		t.Fatalf("%+v", views)
	}
	if n, err := svc.RecoverInterrupted(context.Background()); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestImageUpdateConfigValidation(t *testing.T) {
	_, err := iuRuntime(t, map[string]any{"enabled": true, "pollIntervalSeconds": 5})
	if err == nil || !strings.Contains(err.Error(), "pollIntervalSeconds") {
		t.Fatalf("%v", err)
	}
}

func TestBuildLookup(t *testing.T) {
	jobs := &imagebuild.MemoryJobStore{}
	_ = jobs.Create(context.Background(), imagebuild.Job{ID: "ib-1", Status: imagebuild.JobSucceeded, Reference: "nginx:1.2.3", ImageDigest: "sha256:" + strings.Repeat("a", 64)})
	b := buildLookup{Jobs: jobs}
	got, err := b.Built(context.Background(), "ib-1", "f1")
	if err != nil || got.Digest == "" || got.FindingID != "f1" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := b.Built(context.Background(), "nope", ""); err == nil {
		t.Fatal("unknown job accepted")
	}
}
