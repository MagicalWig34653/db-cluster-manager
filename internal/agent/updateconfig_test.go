// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"math"
	"testing"
)

func TestCheckUpdateConfig(t *testing.T) {
	ok := []struct {
		u        *UpdateConfig
		rollback bool
	}{
		{nil, false},
		{&UpdateConfig{Parallelism: 1}, false},
		{&UpdateConfig{Parallelism: 2, DelayNS: 1, MonitorNS: 1, MaxFailureRatio: 0.5, FailureAction: FailureActionRollback, Order: UpdateStartFirst}, false},
		{&UpdateConfig{Parallelism: 1, FailureAction: FailureActionPause, Order: UpdateStopFirst}, true},
	}
	for _, c := range ok {
		if err := CheckUpdateConfig(c.u, c.rollback); err != nil {
			t.Errorf("%+v: %v", c.u, err)
		}
	}
	bad := []struct {
		u        *UpdateConfig
		rollback bool
	}{
		{&UpdateConfig{}, false},
		{&UpdateConfig{Parallelism: 1, DelayNS: -1}, false},
		{&UpdateConfig{Parallelism: 1, MaxFailureRatio: math.NaN()}, false},
		{&UpdateConfig{Parallelism: 1, FailureAction: "explode"}, false},
		{&UpdateConfig{Parallelism: 1, FailureAction: FailureActionRollback}, true},
		{&UpdateConfig{Parallelism: 1, Order: "first-first"}, false},
	}
	for _, c := range bad {
		if err := CheckUpdateConfig(c.u, c.rollback); err == nil {
			t.Errorf("%+v (rollback %v) accepted", c.u, c.rollback)
		}
	}
}

// The update-service command refuses an invalid update strategy before it
// reaches Docker.
func TestUpdateServiceArgsRefuseBadUpdateConfig(t *testing.T) {
	args := UpdateServiceArgs{ServiceName: "s", Spec: ServiceSpec{Name: "s", Image: "x:1",
		UpdateConfig: &UpdateConfig{Parallelism: 0}}}
	cmd := func() Command {
		raw, _ := json.Marshal(args)
		return Command{Term: 1, OpID: "op1", StepID: "s1", Kind: KindUpdateService, Args: raw}
	}
	if _, err := decodeCommand(cmd(), "/srv/dbcm"); err == nil {
		t.Fatal("parallelism 0 accepted")
	}
	args.Spec.UpdateConfig.Parallelism = 1
	if _, err := decodeCommand(cmd(), "/srv/dbcm"); err != nil {
		t.Fatal(err)
	}
}
