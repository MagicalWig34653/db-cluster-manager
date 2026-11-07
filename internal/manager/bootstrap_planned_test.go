// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"context"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agentrt"
	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

var plannedHost = provision.Host{ID: "fresh1", Address: "127.0.0.1", User: "root"}

func plannedRecords(t *testing.T, f *pkiFixture) (tokens []EnrollTokenRecord, hosts []Versioned[HostRecord]) {
	t.Helper()
	recs, err := List[EnrollTokenRecord](f.svc.Store(), CollEnrollTokens, Consistent())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Value.Swarm == PlannedSwarm {
			tokens = append(tokens, r.Value)
		}
	}
	all, err := List[HostRecord](f.svc.Store(), CollHosts, Consistent())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range all {
		if h.Value.Swarm == PlannedSwarm {
			hosts = append(hosts, h)
		}
	}
	return tokens, hosts
}

func TestPreissueBindsASingleUseTokenToThePlannedHost(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	pe := PlannedEnrollment{PKI: f.pki}
	plain, err := pe.Preissue(context.Background(), plannedHost)
	if err != nil || plain == "" {
		t.Fatalf("%q %v", plain, err)
	}
	tokens, hosts := plannedRecords(t, f)
	if len(tokens) != 1 || len(tokens[0].Token.AllowedHosts) != 1 || tokens[0].Token.AllowedHosts[0] != "fresh1" {
		t.Fatalf("%+v", tokens)
	}
	if ttl := tokens[0].Token.ExpiresAt.Sub(tokens[0].Token.CreatedAt); ttl != HostTokenTTL {
		t.Fatalf("ttl %v", ttl)
	}
	if raw, _, _ := f.svc.Store().Raw(CollEnrollTokens, tokens[0].Token.ID); strings.Contains(string(raw.Value), plain) {
		t.Fatal("plain token stored")
	}
	if len(hosts) != 1 || hosts[0].Value.NodeID != "fresh1" || hosts[0].Value.Addr != "127.0.0.1" || hosts[0].Value.Enrollment != EnrollEnrolled {
		t.Fatalf("%+v", hosts)
	}

	// It cannot enroll another host, not even a registered one.
	for _, other := range []string{"n1", "fresh2"} {
		if _, err := f.enrollInfo(t, plain, other, agentrt.NativeInfo(false)); err == nil {
			t.Fatalf("token enrolled %s", other)
		}
	}
	// It enrolls the planned host once.
	if _, err := f.enrollInfo(t, plain, "fresh1", agentrt.NativeInfo(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enrollInfo(t, plain, "fresh1", agentrt.NativeInfo(false)); err == nil {
		t.Fatal("token usable twice")
	}
}

func TestPreissueForARegisteredHostIssuesNothing(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	plain, err := PlannedEnrollment{PKI: f.pki}.Preissue(context.Background(), provision.Host{ID: "n1", Address: "127.0.0.1"})
	if err != nil || plain != "" {
		t.Fatalf("%q %v", plain, err)
	}
	if tokens, hosts := plannedRecords(t, f); len(tokens) != 0 || len(hosts) != 0 {
		t.Fatalf("%+v %+v", tokens, hosts)
	}
}

func TestPreissueRefusesAKnownButNotEnrolledNode(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	if _, err := (PlannedEnrollment{PKI: f.pki}).Preissue(context.Background(), provision.Host{ID: "n2", Address: "127.0.0.1"}); err == nil {
		t.Fatal("a discovered Swarm node got a planned record")
	}
	if tokens, hosts := plannedRecords(t, f); len(tokens) != 0 || len(hosts) != 0 {
		t.Fatalf("%+v %+v", tokens, hosts)
	}
}

func TestPreissueAgainWithdrawsTheEarlierToken(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	pe := PlannedEnrollment{PKI: f.pki}
	first, err := pe.Preissue(context.Background(), plannedHost)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pe.Preissue(context.Background(), plannedHost)
	if err != nil || second == first {
		t.Fatalf("%v", err)
	}
	if _, err := f.enrollInfo(t, first, "fresh1", agentrt.NativeInfo(false)); err == nil {
		t.Fatal("withdrawn token enrolled")
	}
	if _, err := f.enrollInfo(t, second, "fresh1", agentrt.NativeInfo(false)); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseWithdrawsAnUnusedPreissueAndKeepsAnEnrolledAgent(t *testing.T) {
	f := newPKIFixture(t, "127.0.0.1")
	pe := PlannedEnrollment{PKI: f.pki}
	ctx := context.Background()

	plain, _ := pe.Preissue(ctx, plannedHost)
	if err := pe.Release(ctx, plannedHost); err != nil {
		t.Fatal(err)
	}
	if _, hosts := plannedRecords(t, f); len(hosts) != 0 {
		t.Fatalf("placeholder survived: %+v", hosts)
	}
	if _, err := f.enrollInfo(t, plain, "fresh1", agentrt.NativeInfo(false)); err == nil {
		t.Fatal("released token enrolled")
	}
	if err := pe.Release(ctx, plannedHost); err != nil {
		t.Fatalf("release is not idempotent: %v", err)
	}

	// After a successful enrollment the record is the agent's identity and stays.
	plain, _ = pe.Preissue(ctx, plannedHost)
	if _, err := f.enrollInfo(t, plain, "fresh1", agentrt.NativeInfo(false)); err != nil {
		t.Fatal(err)
	}
	if err := pe.Release(ctx, plannedHost); err != nil {
		t.Fatal(err)
	}
	if _, hosts := plannedRecords(t, f); len(hosts) != 1 || hosts[0].Value.identity(agentrt.Native).Serial == "" {
		t.Fatalf("%+v", hosts)
	}

	// A registered host is never touched.
	if err := pe.Release(ctx, provision.Host{ID: "n1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Get[HostRecord](f.svc.Store(), CollHosts, HostKey("sw1", "n1"), Consistent()); !ok {
		t.Fatal("registered host record removed")
	}
}
