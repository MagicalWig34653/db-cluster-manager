// SPDX-License-Identifier: AGPL-3.0-only

package patch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/provision"
)

// memSealer keeps serialized documents only, like the sealed metastore.
type memSealer struct{ m map[string][]byte }

func (s *memSealer) Put(c, k string, v any) error {
	b, err := json.Marshal(v)
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[c+"/"+k] = b
	return err
}
func (s *memSealer) Get(c, k string, v any) (bool, error) {
	b, ok := s.m[c+"/"+k]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(b, v)
}
func (s *memSealer) Delete(c, k string) error { delete(s.m, c+"/"+k); return nil }
func (s *memSealer) List(c string) ([]string, error) {
	var out []string
	for k := range s.m {
		if strings.HasPrefix(k, c+"/") {
			out = append(out, strings.TrimPrefix(k, c+"/"))
		}
	}
	return out, nil
}

func installCred() provision.Credential {
	return provision.Credential{PrivateKey: "-----PRIVATE-----", KnownHosts: "10.0.0.5 ssh-ed25519 AAAA\n"}
}

func TestInstallDraftExpiresAndIsWiped(t *testing.T) {
	st := &memSealer{}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	inst := &Installer{Runner: &fakeSSH{out: goodPre}, Store: st, Now: func() time.Time { return now },
		Token: func(context.Context, string) (string, error) { return "tok", nil }}
	plan, err := inst.Plan(context.Background(), installReq(InstallReport), installCred())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.m) != 1 {
		t.Fatal("draft not stored")
	}
	now = now.Add(2 * time.Hour)
	err = inst.Execute(context.Background(), "inst1", plan.Digest)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired draft executed or wrong error: %v", err)
	}
	if len(st.m) != 0 {
		t.Fatal("expired credential still stored")
	}
}

func TestInstallSweepDeletesOnlyExpiredDrafts(t *testing.T) {
	st := &memSealer{}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	inst := &Installer{Runner: &fakeSSH{out: goodPre}, Store: st, Now: func() time.Time { return now }}
	if _, err := inst.Plan(context.Background(), installReq(InstallReport), installCred()); err != nil {
		t.Fatal(err)
	}
	r2 := installReq(InstallReport)
	r2.ID = "inst2"
	now = now.Add(50 * time.Minute)
	if _, err := inst.Plan(context.Background(), r2, installCred()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Minute) // i1 is 70 min old, i2 20 min
	n, err := inst.Sweep(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, ok := st.m["patch_install/inst1"]; ok {
		t.Fatal("expired draft kept")
	}
	if _, ok := st.m["patch_install/inst2"]; !ok {
		t.Fatal("fresh draft removed")
	}
}

func TestInstallCredentialIsWipedAfterTheRunEvenOnFailure(t *testing.T) {
	st := &memSealer{}
	ssh := &fakeSSH{out: goodPre}
	inst := &Installer{Runner: ssh, Store: st, Token: func(context.Context, string) (string, error) { return "tok", nil }}
	plan, err := inst.Plan(context.Background(), installReq(InstallReport), installCred())
	if err != nil {
		t.Fatal(err)
	}
	ssh.failRun = true
	if err := inst.Execute(context.Background(), "inst1", plan.Digest); err == nil {
		t.Fatal("failure expected")
	}
	if len(st.m) != 0 {
		t.Fatal("the SSH credential survived a failed run")
	}
}

func TestInstallTokenFailureKeepsTheDraftForRetry(t *testing.T) {
	st := &memSealer{}
	inst := &Installer{Runner: &fakeSSH{out: goodPre}, Store: st, Token: func(context.Context, string) (string, error) { return "", errors.New("no enrolled host") }}
	plan, err := inst.Plan(context.Background(), installReq(InstallReport), installCred())
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Execute(context.Background(), "inst1", plan.Digest); err == nil {
		t.Fatal("failure expected")
	}
	if len(st.m) != 1 {
		t.Fatal("a failure before the host was touched must keep the draft")
	}
}
