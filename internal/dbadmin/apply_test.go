// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestWriterOnly(t *testing.T) {
	a, f, _, w := newAdmin()
	ctx := context.Background()
	w.node = "n2"
	if _, err := a.PlanCreateDatabase(ctx, CreateDatabaseRequest{Name: "x"}); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("plan on non-writer: %v", err)
	}
	w.node = "n1"
	p, err := a.PlanCreateDatabase(ctx, CreateDatabaseRequest{Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	w.node = "n3" // failover between plan and apply
	if _, err := a.Apply(ctx, p); !errors.Is(err, ErrNotWriter) {
		t.Fatalf("apply on non-writer: %v", err)
	}
	// connected node became writer again but a different one than planned
	a.NodeID, w.node = "n3", "n3"
	if _, err := a.Apply(ctx, p); !errors.Is(err, ErrWriterMoved) {
		t.Fatalf("apply on other writer: %v", err)
	}
	if len(f.execs) != 0 {
		t.Fatal("statements executed despite writer check")
	}
}

func TestApplyPartialFailureAndRetry(t *testing.T) {
	a, f, sink, _ := newAdmin()
	ctx := context.Background()
	p, err := a.PlanProvision(ctx, ProvisionRequest{
		Database: CreateDatabaseRequest{Name: "shop"}, User: CreateUserRequest{User: "shop_app", Host: "%"},
		Set: PrivilegeSet{Preset: PresetReadOnly},
	})
	if err != nil {
		t.Fatal(err)
	}
	pw := p.Statements[1].secret.Reveal()
	f.failOn = "CREATE USER"
	f.failWith = errors.New("ERROR 1396 near IDENTIFIED BY '" + pw + "'")
	res, err := a.Apply(ctx, p)
	var pf *PartialFailureError
	if !errors.As(err, &pf) || pf.Index != 1 || pf.Completed != 1 || pf.Total != 3 {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), pw) || strings.Contains(res.Results[1].Error, pw) {
		t.Fatal("password leaked in error")
	}
	st := []Status{res.Results[0].Status, res.Results[1].Status, res.Results[2].Status}
	if st[0] != StatusDone || st[1] != StatusFailed || st[2] != StatusPending {
		t.Fatalf("%v", st)
	}
	if !p.Idempotent() {
		t.Fatal("provision plan must be safely re-appliable")
	}
	f.failOn = ""
	res, err = a.Apply(ctx, p)
	if err != nil || res.Completed() != 3 {
		t.Fatalf("retry: %v", err)
	}
	if sink.got[SecretRef{Kind: "db-user-password", User: "shop_app", Host: "%"}] != pw {
		t.Fatal("retry must deliver the same password the plan carries")
	}
}

func TestApplySinkFailureStopsBeforeCreate(t *testing.T) {
	a, f, sink, _ := newAdmin()
	sink.fail = errors.New("vault down")
	p, _ := a.PlanCreateUser(context.Background(), CreateUserRequest{User: "u", Host: "%"})
	if _, err := a.Apply(context.Background(), p); err == nil || len(f.execs) != 0 {
		t.Fatalf("user created although secret was not stored: %v", err)
	}
	a.Sink = nil
	if _, err := a.Apply(context.Background(), p); !errors.Is(err, ErrNoSink) {
		t.Fatalf("%v", err)
	}
}

func TestApplyContextCancel(t *testing.T) {
	a, f, _, _ := newAdmin()
	p, _ := a.PlanCreateDatabase(context.Background(), CreateDatabaseRequest{Name: "x"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := a.Apply(ctx, p)
	if !errors.Is(err, context.Canceled) || len(f.execs) != 0 || res.Results[0].Status != StatusFailed {
		t.Fatalf("%v", err)
	}
}

func TestPlanDoesNotExecute(t *testing.T) {
	a, f, _, _ := newAdmin()
	ctx := context.Background()
	_, _ = a.PlanDropDatabase(ctx, "x", ConfirmToken("database", "x"))
	_, _ = a.PlanGrant(ctx, GrantRequest{User: "u", Host: "%", Database: "d", Set: PrivilegeSet{Preset: PresetReadOnly}})
	if len(f.execs) != 0 {
		t.Fatal("planning executed statements")
	}
}
