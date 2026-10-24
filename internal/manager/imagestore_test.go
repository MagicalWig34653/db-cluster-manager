// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

func imageRequest() imagebuild.Request {
	return imagebuild.Request{
		Spec: imagebuild.WebServerSpec{
			Name:  "shop",
			Nginx: imagebuild.NginxSpec{Version: "1.27", BaseDigest: "sha256:" + strings.Repeat("a", 64)},
			PHP:   imagebuild.PHPSpec{Version: "8.3", Extensions: []string{"mysqli"}},
		},
		Target: imagebuild.Target{Registry: "main", Repository: "team/shop", Tag: "1"},
	}
}

// TestImageStateIsReplicatedSealedAndPersistent runs the registry and build
// services on a real single-node store: the registry password must not be
// in the stored documents in clear, nothing secret may reach a job or plan,
// and all state must survive a restart.
func TestImageStateIsReplicatedSealedAndPersistent(t *testing.T) {
	const pw = "pw-very-secret-1234"
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "bot" || p != pw {
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(200)
	}))
	defer reg.Close()
	dir, addr := t.TempDir(), freeAddr(t)
	start := func() *Service {
		s, err := New(Config{NodeID: "solo", DataDir: dir, BindAddr: addr, Tune: fastRaft, PollInterval: 10 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		s.Start()
		waitFor(t, "leader", s.IsLeader)
		return s
	}
	ctx := context.Background()
	fb := &imagebuild.FakeBuilder{}
	s := start()
	builds, regs := ImageServices(s.Store(), testKey, fb, false, 0)
	add := func(name string) error {
		_, err := regs.AddExternal(ctx, "admin", registry.ExternalInput{ID: "main", Name: name, Kind: registry.KindGeneric,
			Endpoint: reg.URL, Username: "bot", Password: registry.NewSecret(pw)})
		return err
	}
	if err := add("Main"); err != nil {
		t.Fatal(err)
	}
	if err := add("Again"); !errors.Is(err, registry.ErrExists) {
		t.Fatalf("duplicate registry: %v", err)
	}
	plan, err := builds.Plan(ctx, imageRequest())
	if err != nil {
		t.Fatal(err)
	}
	req := imageRequest()
	req.ApprovalDigest = plan.Digest
	job, err := builds.Start(ctx, "op", req)
	if err != nil || job.Status != imagebuild.JobSucceeded {
		t.Fatalf("start: %+v %v", job, err)
	}
	// An unfinished job stays "running" in the store until recovered.
	if err := (ImageJobStore{Store: s.Store()}).Put(ctx, imagebuild.Job{ID: "ib-stale", Status: imagebuild.JobRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// The password is sealed; no stored document holds it in clear.
	for _, coll := range []string{collRegistries, collRegistrySecrets, collImageJobs, collImagePlans} {
		docs, err := s.Store().RawList(coll)
		if err != nil || len(docs) == 0 {
			t.Fatalf("collection %s: %d docs, %v", coll, len(docs), err)
		}
		for _, d := range docs {
			if bytes.Contains(d.Value, []byte(pw)) {
				t.Fatalf("%s/%s contains the registry password", coll, d.Key)
			}
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = start()
	defer s.Close()
	builds, regs = ImageServices(s.Store(), testKey, fb, false, 0)
	cs, err := regs.List(ctx)
	if err != nil || len(cs) != 1 || cs[0].CredentialRef == "" {
		t.Fatalf("registries after restart: %+v %v", cs, err)
	}
	if _, sec, err := regs.Credential(ctx, "main"); err != nil || sec.Reveal() != pw {
		t.Fatalf("credential after restart: %v", err)
	}
	if got, err := builds.Get(ctx, job.ID); err != nil || got.ImageDigest == "" {
		t.Fatalf("job after restart: %+v %v", got, err)
	}
	if sp, err := builds.GetPlan(ctx, plan.Digest); err != nil || sp.Plan.Dockerfile == "" {
		t.Fatalf("plan after restart: %v", err)
	}
	n, err := builds.RecoverInterrupted(ctx)
	if err != nil || n != 1 {
		t.Fatalf("recover: %d %v", n, err)
	}
	if got, _ := builds.Get(ctx, "ib-stale"); got.Status != imagebuild.JobFailed || got.Error == "" {
		t.Fatalf("stale job: %+v", got)
	}
	list, _ := builds.List(ctx)
	if len(list) != 2 || list[0].Log != "" {
		t.Fatalf("list: %d jobs, log omitted=%v", len(list), list[0].Log == "")
	}
	if _, err := builds.Get(ctx, "nope"); !errors.Is(err, imagebuild.ErrJobNotFound) {
		t.Fatalf("missing job: %v", err)
	}
}

// TestStoredJobLogIsRedacted checks that a secret echoed by the builder
// never reaches the replicated job record.
func TestStoredJobLogIsRedacted(t *testing.T) {
	const pw = "pw-log-leak-9876"
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer reg.Close()
	s := newSolo(t)
	fb := &imagebuild.FakeBuilder{LogLines: []string{"login with " + pw, "Authorization: Bearer abcdef123456"}, Err: errors.New("push failed for " + pw)}
	builds, regs := ImageServices(s.Store(), testKey, fb, false, 0)
	ctx := context.Background()
	if _, err := regs.AddExternal(ctx, "admin", registry.ExternalInput{ID: "main", Name: "Main", Kind: registry.KindGeneric,
		Endpoint: reg.URL, Username: "bot", Password: registry.NewSecret(pw)}); err != nil {
		t.Fatal(err)
	}
	plan, err := builds.Plan(ctx, imageRequest())
	if err != nil {
		t.Fatal(err)
	}
	req := imageRequest()
	req.ApprovalDigest = plan.Digest
	job, err := builds.Start(ctx, "op", req)
	if err != nil || job.Status != imagebuild.JobFailed {
		t.Fatalf("start: %+v %v", job, err)
	}
	docs, _ := s.Store().RawList(collImageJobs)
	if len(docs) != 1 {
		t.Fatalf("%d job documents", len(docs))
	}
	for _, leak := range []string{pw, "abcdef123456"} {
		if bytes.Contains(docs[0].Value, []byte(leak)) {
			t.Fatalf("stored job contains %q", leak)
		}
	}
	if !bytes.Contains(docs[0].Value, []byte("REDACTED")) {
		t.Fatal("stored job log carries no redaction marker")
	}
}

func TestImagePlanStorePrunesOldest(t *testing.T) {
	s := newSolo(t)
	ps := ImagePlanStore{Store: s.Store()}
	base := time.Now()
	for i := 0; i < maxStoredPlans+3; i++ {
		p := imagebuild.StoredPlan{Plan: imagebuild.Plan{Digest: fmt.Sprintf("d%04d", i)}, At: base.Add(time.Duration(i) * time.Second)}
		if err := ps.Put(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	docs, _ := s.Store().RawList(collImagePlans)
	if len(docs) != maxStoredPlans {
		t.Fatalf("%d plans kept", len(docs))
	}
	if _, err := ps.Get(context.Background(), "d0000"); !errors.Is(err, imagebuild.ErrPlanNotFound) {
		t.Fatalf("oldest plan not pruned: %v", err)
	}
}

// newSolo starts a single-node service for a test.
func newSolo(t *testing.T) *Service {
	t.Helper()
	s, err := New(Config{NodeID: "solo", DataDir: t.TempDir(), BindAddr: freeAddr(t), Tune: fastRaft, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s.Start()
	t.Cleanup(func() { _ = s.Close() })
	waitFor(t, "leader", s.IsLeader)
	return s
}
