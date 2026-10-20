// SPDX-License-Identifier: AGPL-3.0-only

package galera_test

import (
	"reflect"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
)

// The galera package does not import the agent; these checks keep the
// mirrored constants and struct fields in step with internal/agent.
func TestAgentCompatibility(t *testing.T) {
	pairs := [][2]string{
		{galera.LabelInstallation, agent.LabelInstallation},
		{galera.LabelCluster, agent.LabelCluster},
		{galera.LabelNode, agent.LabelNode},
		{galera.MarkerFileName, agent.MarkerFileName},
	}
	for _, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("constant %q differs from agent %q", p[0], p[1])
		}
	}
	mine := reflect.TypeFor[galera.ServiceSpec]()
	theirs := reflect.TypeFor[agent.ServiceSpec]()
	for i := range theirs.NumField() {
		af := theirs.Field(i)
		gf, ok := mine.FieldByName(af.Name)
		if !ok {
			t.Errorf("galera.ServiceSpec lacks agent field %s", af.Name)
			continue
		}
		if gf.Tag.Get("json") != af.Tag.Get("json") {
			t.Errorf("field %s json tag %q, agent %q", af.Name, gf.Tag.Get("json"), af.Tag.Get("json"))
		}
	}
	if !reflect.DeepEqual(fieldNames(reflect.TypeFor[galera.Mount]()), fieldNames(reflect.TypeFor[agent.Mount]())) {
		t.Error("galera.Mount differs from agent.Mount")
	}
}

func fieldNames(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		out = append(out, t.Field(i).Name+" "+t.Field(i).Type.String()+" "+string(t.Field(i).Tag))
	}
	return out
}
