// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

// addRefObjects adds networks and secrets to the fixture:
//
//	net-1        owned network "dbcm-net", allow-listed
//	sec-1        owned secret "root-pw", allow-listed
//	sec-2        owned secret "tls-key", allow-listed
//	net-foreign  network "other-net" of another installation, allow-listed
//	sec-nolabel  secret "nolabel-pw" without labels, allow-listed
//	sec-unlisted owned secret "unlisted-pw", not allow-listed
func addRefObjects(f *fixture) {
	f.t.Helper()
	own := map[string]string{LabelInstallation: testInstallation}
	for _, o := range []Object{
		{Kind: KindNetwork, ID: "net-1", Name: "dbcm-net", Labels: own},
		{Kind: KindSecret, ID: "sec-1", Name: "root-pw", Labels: own},
		{Kind: KindSecret, ID: "sec-2", Name: "tls-key", Labels: own},
		{Kind: KindNetwork, ID: "net-foreign", Name: "other-net", Labels: map[string]string{LabelInstallation: "other"}},
		{Kind: KindSecret, ID: "sec-nolabel", Name: "nolabel-pw"},
		{Kind: KindSecret, ID: "sec-unlisted", Name: "unlisted-pw", Labels: own},
	} {
		f.docker.objects[o.ID] = o
	}
	wantStatus(f.t, f.mustDo(1, "setup", "allow", KindAllowObjects,
		AllowObjectsArgs{IDs: []string{"net-1", "sec-1", "sec-2", "net-foreign", "sec-nolabel"}}), StatusSucceeded, "")
}

func TestCreateServiceReferencesPinnedToCheckedIDs(t *testing.T) {
	f := newFixture(t)
	addRefObjects(f)
	s := spec("galera-1")
	s.Hostname = "galera-1"
	s.Networks = []string{"dbcm-net"}        // by name
	s.Secrets = []string{"root-pw", "sec-2"} // by name and by ID
	s.EndpointMode = EndpointModeDNSRR
	s.StopGracePeriodNS = 60_000_000_000
	r := f.mustDo(1, "op", "create", KindCreateService, s)
	wantStatus(t, r, StatusSucceeded, "")
	var out struct{ ID string }
	if err := json.Unmarshal(r.Output, &out); err != nil {
		t.Fatal(err)
	}
	got := f.docker.specs[out.ID]
	if !reflect.DeepEqual(got.Networks, []string{"net-1"}) || !reflect.DeepEqual(got.Secrets, []string{"sec-1", "sec-2"}) {
		t.Fatalf("references not pinned to IDs: networks %v secrets %v", got.Networks, got.Secrets)
	}
	if got.Hostname != "galera-1" || got.EndpointMode != EndpointModeDNSRR || got.StopGracePeriodNS != 60_000_000_000 {
		t.Fatalf("fields lost on the way to Docker: %+v", got)
	}
}

func TestCreateServiceRefusesUnownedReferences(t *testing.T) {
	cases := []struct {
		name     string
		networks []string
		secrets  []string
		status   Status
		code     Code
	}{
		{"foreign network", []string{"other-net"}, nil, StatusRefused, CodeNotOwned},
		{"foreign network by ID", []string{"net-foreign"}, nil, StatusRefused, CodeNotOwned},
		{"unlabelled secret", nil, []string{"nolabel-pw"}, StatusRefused, CodeNotOwned},
		{"secret not in allow-list", nil, []string{"unlisted-pw"}, StatusRefused, CodeNotOwned},
		{"volume referenced as network", []string{"vol-1"}, nil, StatusRefused, CodeNotOwned},
		{"owned network, one bad secret", []string{"dbcm-net"}, []string{"root-pw", "sec-unlisted"}, StatusRefused, CodeNotOwned},
		{"missing network", []string{"nope"}, nil, StatusFailed, CodeNotFound},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			addRefObjects(f)
			s := spec("galera-1")
			s.Networks, s.Secrets = c.networks, c.secrets
			wantStatus(t, f.mustDo(1, "op", "create-"+string(rune('a'+i)), KindCreateService, s), c.status, c.code)
			if n := countPrefix(f.docker.Calls(), "create-service"); n != 0 {
				t.Fatalf("service created despite %s", c.name)
			}
		})
	}
}

func TestCreateServiceRejectsInvalidNewFields(t *testing.T) {
	f := newFixture(t)
	bad := []ServiceSpec{spec("a"), spec("b"), spec("c"), spec("d")}
	bad[0].EndpointMode = "round-robin"
	bad[1].StopGracePeriodNS = -1
	bad[2].Networks = []string{""}
	bad[3].Secrets = []string{""}
	for i, s := range bad {
		if _, err := f.do(1, "op", "bad-"+s.Name, KindCreateService, s); err == nil {
			t.Errorf("spec %d accepted", i)
		}
	}
	if len(f.docker.Calls()) != 0 {
		t.Fatalf("docker called for invalid specs: %v", f.docker.Calls())
	}
}
