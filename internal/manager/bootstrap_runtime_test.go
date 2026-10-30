// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/dockerclient"
	"github.com/MagicalWig34653/db-cluster-manager/internal/enroll"
	"github.com/MagicalWig34653/db-cluster-manager/internal/manager/swarm"
	"github.com/MagicalWig34653/db-cluster-manager/internal/metastore"
	"github.com/MagicalWig34653/db-cluster-manager/internal/opjournal"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

const secretKeyMaterial = "-----BEGIN OPENSSH PRIVATE KEY-----\nTOP-SECRET-BOOTSTRAP-ROOT\n"

func TestBootstrapDraftStoreSealsCredentialsAndDeletesThem(t *testing.T) {
	s := newCluster(t, 1, nil).leader()
	st := BootstrapDraftStore{Store: s.Store(), Keys: testKey}
	rec := provision.DraftRecord{Request: provision.BootstrapRequest{ID: "boot1", Host: provision.Host{ID: "n1", Address: "10.0.0.5", User: "root"}},
		Status: provision.StatusPlanned, HostFingerprint: "SHA256:abc", HostKnownHosts: "10.0.0.5 ssh-ed25519 AAAA\n"}
	if err := st.SaveDraft(rec); err != nil {
		t.Fatal(err)
	}
	sec := provision.SecretsRecord{Root: provision.Credential{PrivateKey: secretKeyMaterial, Password: "hunter2-root"}, JoinToken: "SWMTKN-1-joinsecret",
		Dedicated: provision.Credential{PrivateKey: "DEDICATED-PRIVATE"}, Expires: time.Now().Add(time.Hour)}
	if err := st.SaveSecrets("boot1", sec); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{CollBootstrapDrafts, CollBootstrapSecrets} {
		raw, ok, _ := s.Store().Raw(c, "boot1")
		if !ok {
			t.Fatalf("%s missing", c)
		}
		for _, leak := range []string{"TOP-SECRET", "hunter2", "SWMTKN", "DEDICATED-PRIVATE", "SHA256:abc"} {
			if bytes.Contains(raw.Value, []byte(leak)) {
				t.Fatalf("%s stores %q in clear", c, leak)
			}
		}
	}
	drafts, err := st.LoadDrafts()
	if err != nil || len(drafts) != 1 || drafts[0].HostKnownHosts != rec.HostKnownHosts || drafts[0].Request.Host.Address != "10.0.0.5" {
		t.Fatalf("%+v %v", drafts, err)
	}
	got, ok, err := st.LoadSecrets("boot1")
	if err != nil || !ok || got.Root.Password != "hunter2-root" || got.JoinToken != "SWMTKN-1-joinsecret" || got.Dedicated.PrivateKey != "DEDICATED-PRIVATE" {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	// The draft document never holds a credential: wiping is the deletion of
	// the secrets document, and deleting twice is not an error.
	if err := st.DeleteSecrets("boot1"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSecrets("boot1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.LoadSecrets("boot1"); ok {
		t.Fatal("secrets survived")
	}
	if _, ok, _ := s.Store().Raw(CollBootstrapSecrets, "boot1"); ok {
		t.Fatal("secrets document survived")
	}
	// A document moved to another key does not open (AAD binds the key).
	raw, _, _ := s.Store().Raw(CollBootstrapDrafts, "boot1")
	if _, err := s.Store().Txn(metastore.DocOp{Collection: CollBootstrapDrafts, Key: "other", Value: raw.Value}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LoadDrafts(); err == nil {
		t.Fatal("a copied sealed draft opened under another ID")
	}
}

func TestBootstrapServiceSurvivesLeaderChangeOnTheReplicatedStore(t *testing.T) {
	// Two services on the same replicated store model the old and the new
	// leader. The credentials exist only sealed in the store between them.
	s := newCluster(t, 1, nil).leader()
	st := BootstrapDraftStore{Store: s.Store(), Keys: testKey}
	mk := func(term uint64) *provision.BootstrapService {
		svc := provision.NewBootstrapService(provision.BootstrapDeps{Journal: opjournal.New()}, provision.BootstrapEnv{}, noProbe{}, nil)
		svc.Store, svc.Term = st, func() uint64 { return term }
		if err := svc.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		return svc
	}
	old := mk(1)
	if _, err := old.Draft(context.Background(), provision.DraftInput{
		Request: provision.BootstrapRequest{ID: "boot1", Host: provision.Host{ID: "n1", Address: "10.0.0.5", User: "root"},
			Profile: provision.BootstrapProfile{ServiceUser: "dbcm", Runtime: "none", Cluster: provision.ClusterChoice{Platform: "none"}}},
		Credential: provision.Credential{Password: "pw-leader-change"}}); err != nil {
		t.Fatal(err)
	}
	n := mk(2)
	if v := n.Status("boot1"); !v.CredentialsHeld {
		t.Fatalf("%+v", v)
	}
	raw, _, _ := s.Store().Raw(CollBootstrapSecrets, "boot1")
	if bytes.Contains(raw.Value, []byte("pw-leader-change")) {
		t.Fatal("password in clear")
	}
}

type noProbe struct{}

func (noProbe) Probe(context.Context, provision.Host) (provision.HostKey, error) {
	return provision.HostKey{Type: "ssh-ed25519", Fingerprint: "SHA256:x", KnownHosts: "10.0.0.5 ssh-ed25519 AAAA\n"}, nil
}

func TestPatchInstallSealerSealsAndLists(t *testing.T) {
	s := newCluster(t, 1, nil).leader()
	sl := PatchInstallSealer{Store: s.Store(), Keys: testKey}
	type doc struct{ Secret, Public string }
	if err := sl.Put("patch_install", "i1", doc{Secret: "ssh-private-material", Public: "p"}); err != nil {
		t.Fatal(err)
	}
	raw, _, _ := s.Store().Raw("patch_install", "i1")
	if bytes.Contains(raw.Value, []byte("ssh-private-material")) {
		t.Fatal("stored in clear")
	}
	var got doc
	if ok, err := sl.Get("patch_install", "i1", &got); err != nil || !ok || got.Secret != "ssh-private-material" {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	if ok, _ := sl.Get("patch_install", "missing", &got); ok {
		t.Fatal("phantom")
	}
	keys, err := sl.List("patch_install")
	if err != nil || len(keys) != 1 || keys[0] != "i1" {
		t.Fatalf("%v %v", keys, err)
	}
	if err := sl.Delete("patch_install", "i1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := sl.Get("patch_install", "i1", &got); ok {
		t.Fatal("not deleted")
	}
}

func TestHostEnrollmentTokensAreBoundToOneEnrolledHost(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	src := HostEnrollmentTokens{PKI: f.pki}
	ctx := context.Background()
	if _, err := src.EnrollToken(ctx, provision.Host{ID: "unknown"}, "native"); err == nil || !strings.Contains(err.Error(), "enrolled") {
		t.Fatalf("a host without an enrolled record got a token: %v", err)
	}
	if _, err := src.EnrollToken(ctx, provision.Host{ID: "n2"}, "native"); err == nil {
		t.Fatal("a merely discovered host got a token")
	}
	if _, err := src.EnrollToken(ctx, provision.Host{ID: "n1"}, "docker"); err == nil {
		t.Fatal("unknown runtime accepted")
	}
	plain, err := src.EnrollToken(ctx, provision.Host{ID: "n1"}, "native")
	if err != nil || plain == "" {
		t.Fatal(err)
	}
	if other, _ := src.EnrollToken(ctx, provision.Host{ID: "n1"}, "native"); other == plain {
		t.Fatal("tokens must be unique")
	}
	// Another host cannot use it, the right host can (once per runtime).
	if err := f.pki.ConsumeToken(ctx, plain, "n2", agentrt.Native, net.ParseIP("127.0.0.1")); !errors.Is(err, enroll.ErrHostNotAllowed) {
		t.Fatalf("%v", err)
	}
	if err := f.pki.ConsumeToken(ctx, plain, "n1", agentrt.Native, net.ParseIP("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	if err := f.pki.ConsumeToken(ctx, plain, "n1", agentrt.Native, net.ParseIP("127.0.0.1")); err == nil {
		t.Fatal("token usable twice for the same runtime")
	}
	// The plain token is never stored.
	recs, _ := List[EnrollTokenRecord](f.svc.Store(), CollEnrollTokens)
	for _, r := range recs {
		if strings.Contains(r.Value.Token.ID+r.Value.Token.Digest, plain) {
			t.Fatal("plain token stored")
		}
	}
}

func TestBootstrapManagerActionsDeployContainerAgentAndSealKey(t *testing.T) {
	c := newCluster(t, 1, nil)
	s := c.leader()
	ops := opsFor(t, s)
	if _, err := Put(s.Store(), CollSettings, SettingsKey, Settings{InstallationID: "inst-1", VolumeBase: "/srv/dbcm", AgentImage: "agent:1"}, nil); err != nil {
		t.Fatal(err)
	}
	conn := &memConn{labels: map[string]map[string]string{}, nodes: []dockerclient.Node{
		{ID: "n1", Hostname: "m1", Role: "manager", Availability: "active", State: "ready", EngineVersion: "29.9.1"},
	}}
	sw := &Swarms{Svc: s, Authorizer: ops.Authorizer, Matrix: swarm.DefaultMatrix(),
		Dial: func(string) (swarm.Connector, error) { return conn, nil }}
	rec, err := sw.Connect(adminSubject, "lab", "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Sync(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	act := BootstrapManagerActions{Swarms: sw, Store: s.Store(), Keys: testKey, Actor: adminSubject}
	ctx := context.Background()
	if err := act.DeployContainerAgent(ctx, provision.Host{ID: "nope"}); err == nil {
		t.Fatal("a host without a Swarm node record was accepted")
	}
	if err := act.DeployContainerAgent(ctx, provision.Host{ID: "n1"}); err != nil {
		t.Fatal(err)
	}
	if conn.labels["n1"][swarm.AgentLabel("inst-1")] != "true" {
		t.Fatalf("labels %+v", conn.labels)
	}
	h, _, _ := Get[HostRecord](s.Store(), CollHosts, HostKey(rec.ID, "n1"))
	if h.Value.Enrollment != EnrollEnrolled || h.Value.AgentServiceID != "agent-svc" {
		t.Fatalf("%+v", h.Value)
	}
	// Idempotent.
	if err := act.DeployContainerAgent(ctx, provision.Host{ID: "n1"}); err != nil {
		t.Fatal(err)
	}
	// A revoked host is never re-enrolled by a bootstrap.
	if _, err := sw.SetEnrollment(adminSubject, rec.ID, "n1", false, ""); err != nil {
		t.Fatal(err)
	}
	if err := act.DeployContainerAgent(ctx, provision.Host{ID: "n1"}); err == nil {
		t.Fatal("revoked host re-enrolled")
	}
	// The dedicated key is sealed.
	if err := act.StoreDedicatedKey(ctx, provision.Host{ID: "n1"}, provision.Credential{PrivateKey: secretKeyMaterial, KnownHosts: "k"}); err != nil {
		t.Fatal(err)
	}
	raw, ok, _ := s.Store().Raw(CollBootstrapHostKeys, "n1")
	if !ok || bytes.Contains(raw.Value, []byte("TOP-SECRET")) {
		t.Fatalf("dedicated key stored wrongly: %v", ok)
	}
	if got, ok, _ := GetSealed[provision.Credential](s.Store(), testKey, CollBootstrapHostKeys, "n1"); !ok || got.PrivateKey != secretKeyMaterial {
		t.Fatal("sealed key unreadable")
	}
}
