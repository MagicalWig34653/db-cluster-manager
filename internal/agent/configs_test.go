// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func testConfig() ConfigSpec {
	return ConfigSpec{Name: "app-config", Data: []byte("file contents"), Labels: map[string]string{LabelCluster: "cluster-1", LabelNode: "config"}}
}

func TestConfigLifecycle(t *testing.T) {
	f := newFixture(t)
	a := testConfig()
	r := f.mustDo(1, "o", "create", KindCreateConfig, a)
	wantStatus(t, r, StatusSucceeded, "")
	var out map[string]string
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	id := out["id"]
	o := f.docker.objects[id]
	if o.Labels[LabelInstallation] != testInstallation || o.Labels[LabelOpID] != "o" || o.Labels[LabelStepID] != "create" || !f.exec.owner.Allowed(id) {
		t.Fatal(o)
	}
	f.restart()
	r = f.mustDo(2, "o", "create", KindCreateConfig, a)
	if !r.Replayed {
		t.Fatal(r)
	}
	r = f.mustDo(2, "o2", "create", KindCreateConfig, a)
	wantStatus(t, r, StatusSucceeded, "")
	spec := ServiceSpec{Name: "app", Image: "app:1", Replicas: 1, Configs: []ConfigRef{{Config: a.Name, Target: "/etc/app.conf"}}}
	r = f.mustDo(2, "o2", "service", KindCreateService, spec)
	wantStatus(t, r, StatusSucceeded, "")
	for _, s := range f.docker.specs {
		if s.Configs[0].Config != id {
			t.Fatal(s)
		}
	}
	a.Data = []byte("changed")
	r = f.mustDo(2, "o3", "create", KindCreateConfig, a)
	wantStatus(t, r, StatusRefused, CodeImmutable)
	rm := RemoveConfigArgs{Config: a.Name, ClusterID: "wrong"}
	r = f.mustDo(2, "d", "wrong", KindRemoveConfig, rm)
	wantStatus(t, r, StatusRefused, CodeNotOwned)
	rm.ClusterID = "cluster-1"
	f.docker.failNext = ErrConflict
	r = f.mustDo(2, "d", "busy", KindRemoveConfig, rm)
	wantStatus(t, r, StatusFailed, CodeConfigInUse)
	r = f.mustDo(2, "d", "remove", KindRemoveConfig, rm)
	wantStatus(t, r, StatusSucceeded, "")
	if f.exec.owner.Allowed(id) {
		t.Fatal("deleted config remains allowed")
	}
	r = f.mustDo(2, "d", "absent", KindRemoveConfig, rm)
	wantStatus(t, r, StatusSucceeded, "")
}

func TestConfigGuardsAndRedaction(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlisted", true: "foreign"}[allowed], func(t *testing.T) {
			f := newFixture(t)
			a := testConfig()
			labels := map[string]string{LabelInstallation: testInstallation}
			if allowed {
				labels[LabelInstallation] = "foreign"
				f.exec.allow("config-id")
			}
			f.docker.objects["config-id"] = Object{Kind: KindConfig, ID: "config-id", Name: a.Name, Labels: labels}
			r := f.mustDo(1, "o", "c", KindCreateConfig, a)
			wantStatus(t, r, StatusRefused, CodeNotOwned)
			r = f.mustDo(1, "o", "d", KindRemoveConfig, RemoveConfigArgs{Config: a.Name, ClusterID: "cluster-1"})
			wantStatus(t, r, StatusRefused, CodeNotOwned)
			if len(f.docker.Mutations()) != 0 {
				t.Fatal(f.docker.Mutations())
			}
		})
	}
	f := newFixture(t)
	a := testConfig()
	a.Secret = true
	a.Data = []byte("unique-sensitive-content")
	if _, err := f.do(1, "o", "secret", KindCreateConfig, a); !errors.Is(err, ErrInvalidCommand) || strings.Contains(err.Error(), string(a.Data)) {
		t.Fatal("secret config was accepted or leaked")
	}
	a.Secret = false
	f.docker.failNext = errors.New(string(a.Data))
	r := f.mustDo(1, "o", "error", KindCreateConfig, a)
	state, err := os.ReadFile(f.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Message, string(a.Data)) || strings.Contains(string(state), string(a.Data)) {
		t.Fatal("config content leaked")
	}
	f.docker.failNext = ErrOutcomeUnknown
	r = f.mustDo(1, "o", "unknown", KindCreateConfig, a)
	wantStatus(t, r, StatusUnknown, CodeInDoubt)
}

func TestConfigDecodeErrorDoesNotEchoContent(t *testing.T) {
	f := newFixture(t)
	_, err := f.exec.Handle(context.Background(), Command{Term: 1, OpID: "o", StepID: "s", Kind: KindCreateConfig, Args: json.RawMessage(`{"name":"cfg","data":123456789,"secret":true}`)})
	if !errors.Is(err, ErrInvalidCommand) || strings.Contains(err.Error(), "123456789") {
		t.Fatal("invalid sensitive config was accepted or echoed")
	}
}
