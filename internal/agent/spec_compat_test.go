// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"encoding/json"
	"testing"
)

// TestServiceSpecEncodingStable pins the JSON encoding and the args digest
// of specs that do not use the optional fields. Added fields must be
// omitempty so that step records written by earlier versions still match
// (a changed digest would turn a replay into ErrStepConflict).
func TestServiceSpecEncodingStable(t *testing.T) {
	cases := []struct {
		name   string
		spec   ServiceSpec
		json   string
		digest string
	}{
		{
			name:   "minimal",
			spec:   ServiceSpec{Name: "galera-1", Image: "mariadb:11.4"},
			json:   `{"name":"galera-1","image":"mariadb:11.4","replicas":0}`,
			digest: "5d40f0a7545248887c632fe063ffd6c2b09eba21f50ef96054b81f9ae43ea9e6",
		},
		{
			name: "pre-orchestrate fields",
			spec: ServiceSpec{
				Name: "galera-1", Image: "mariadb:11.4",
				Labels:        map[string]string{LabelCluster: "c1"},
				Env:           []string{"A=1"},
				Command:       []string{"mariadbd"},
				Args:          []string{"--wsrep_on=ON"},
				Mounts:        []Mount{{Type: "volume", Source: "v1", Target: "/dbcm"}},
				Constraints:   []string{"node.id==abc"},
				Replicas:      1,
				RestartPolicy: &RestartPolicy{Condition: "any"},
				Resources:     &Resources{Limits: &ResourceLimits{NanoCPUs: 1_000_000_000}},
			},
			json: `{"name":"galera-1","image":"mariadb:11.4","labels":{"dbcm.cluster":"c1"},"env":["A=1"],` +
				`"command":["mariadbd"],"args":["--wsrep_on=ON"],"mounts":[{"type":"volume","source":"v1","target":"/dbcm"}],` +
				`"constraints":["node.id==abc"],"replicas":1,"restart_policy":{"condition":"any"},"resources":{"limits":{"nano_cpus":1000000000}}}`,
			digest: "f22db4a137aaa9d51e41528144a4c8db17280c9aba938a15763ecd3b79d824bb",
		},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.spec)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.json {
			t.Errorf("%s: encoding\n got %s\nwant %s", c.name, b, c.json)
		}
		d, err := argsDigest(KindCreateService, b)
		if err != nil {
			t.Fatal(err)
		}
		if d != c.digest {
			t.Errorf("%s: digest %s, want %s", c.name, d, c.digest)
		}
	}
}
