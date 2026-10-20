// SPDX-License-Identifier: AGPL-3.0-only

package galera_test

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/orchestrate"
)

// The galera package does not import the agent. These checks keep the
// mirrored constants in step with internal/agent and make sure that the
// conversion in internal/orchestrate carries every galera spec value into
// the agent spec. A field added on either side fails here until it is
// mapped (or, on the agent side, declared unused by galera).

// specFieldMap maps each galera.ServiceSpec leaf to the agent.ServiceSpec
// leaf it converts to, with the expected agent value for a galera value.
var specFieldMap = []struct {
	galera, agent string
	conv          func(any) any // nil: values are equal
}{
	{"Name", "Name", nil},
	{"Image", "Image", nil},
	{"Labels", "Labels", nil},
	{"Env", "Env", nil},
	{"Args", "Args", nil},
	{"Mounts[].Type", "Mounts[].Type", nil},
	{"Mounts[].Source", "Mounts[].Source", nil},
	{"Mounts[].Target", "Mounts[].Target", nil},
	{"Mounts[].ReadOnly", "Mounts[].ReadOnly", nil},
	{"Constraints", "Constraints", nil},
	{"Replicas", "Replicas", nil},
	{"Hostname", "Hostname", nil},
	{"Networks", "Networks", nil},
	{"EndpointMode", "EndpointMode", nil},
	{"Secrets", "Secrets", nil},
	{"RestartCondition", "RestartPolicy.Condition", nil},
	{"StopGracePeriod", "StopGracePeriodNS", func(v any) any { return int64(v.(time.Duration)) }},
	{"Resources.Limits.MilliCPU", "Resources.Limits.NanoCPUs", milliToNano},
	{"Resources.Limits.MemoryBytes", "Resources.Limits.MemoryBytes", nil},
	{"Resources.Reservations.MilliCPU", "Resources.Reservations.NanoCPUs", milliToNano},
	{"Resources.Reservations.MemoryBytes", "Resources.Reservations.MemoryBytes", nil},
}

// agentUnused are agent.ServiceSpec leaves galera does not set.
var agentUnused = map[string]bool{
	"Command":                   true,
	"RestartPolicy.DelayNS":     true,
	"RestartPolicy.MaxAttempts": true,
	"RestartPolicy.WindowNS":    true,
	"Resources.Limits.Pids":     true,
}

func milliToNano(v any) any { return v.(int64) * 1_000_000 }

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
	if !reflect.DeepEqual(fieldNames(reflect.TypeFor[galera.Mount]()), fieldNames(reflect.TypeFor[agent.Mount]())) {
		t.Error("galera.Mount differs from agent.Mount")
	}
}

// TestAgentSpecJSONContract pins the agent json names of the fields the
// conversion fills, and requires that fields with the same name and type on
// both sides also share their json tag.
func TestAgentSpecJSONContract(t *testing.T) {
	want := map[string]string{
		"Hostname":          "hostname,omitempty",
		"Networks":          "networks,omitempty",
		"EndpointMode":      "endpoint_mode,omitempty",
		"Secrets":           "secrets,omitempty",
		"StopGracePeriodNS": "stop_grace_period_ns,omitempty",
		"RestartPolicy":     "restart_policy,omitempty",
		"Resources":         "resources,omitempty",
	}
	at := reflect.TypeFor[agent.ServiceSpec]()
	for name, tag := range want {
		f, ok := at.FieldByName(name)
		if !ok {
			t.Errorf("agent.ServiceSpec lacks %s", name)
			continue
		}
		if got := f.Tag.Get("json"); got != tag {
			t.Errorf("agent.ServiceSpec.%s json tag %q, want %q", name, got, tag)
		}
	}
	gt := reflect.TypeFor[galera.ServiceSpec]()
	for i := range gt.NumField() {
		gf := gt.Field(i)
		af, ok := at.FieldByName(gf.Name)
		if !ok || af.Type != gf.Type {
			continue
		}
		if gf.Tag.Get("json") != af.Tag.Get("json") {
			t.Errorf("field %s json tag %q, agent %q", gf.Name, gf.Tag.Get("json"), af.Tag.Get("json"))
		}
	}
}

// TestConversionDropsNothing converts a galera spec in which every field
// holds a distinct non-zero value and checks each value arrives in the
// agent spec, and that every agent field is either filled or declared
// unused.
func TestConversionDropsNothing(t *testing.T) {
	var g galera.ServiceSpec
	seed := 0
	fill(reflect.ValueOf(&g).Elem(), &seed)
	a := orchestrate.ToAgentSpec(g)

	gl := map[string]reflect.Value{}
	leaves(reflect.ValueOf(g), "", gl)
	al := map[string]reflect.Value{}
	leaves(reflect.ValueOf(a), "", al)

	mapped := map[string]bool{}
	targets := map[string]bool{}
	for _, m := range specFieldMap {
		mapped[m.galera] = true
		targets[m.agent] = true
		gv, ok := gl[m.galera]
		if !ok {
			t.Errorf("mapping names unknown galera field %s", m.galera)
			continue
		}
		av, ok := al[m.agent]
		if !ok {
			t.Errorf("mapping names unknown agent field %s", m.agent)
			continue
		}
		if gv.IsZero() {
			t.Fatalf("fill left galera field %s zero", m.galera)
		}
		want := gv.Interface()
		if m.conv != nil {
			want = m.conv(want)
		}
		if !reflect.DeepEqual(av.Interface(), want) {
			t.Errorf("galera %s = %v converts to agent %s = %v, want %v", m.galera, gv.Interface(), m.agent, av.Interface(), want)
		}
	}
	for _, p := range sortedKeys(gl) {
		if !mapped[p] {
			t.Errorf("galera field %s is not converted (add it to orchestrate.ToAgentSpec and specFieldMap)", p)
		}
	}
	for _, p := range sortedKeys(al) {
		switch {
		case targets[p]:
		case agentUnused[p]:
			if !al[p].IsZero() {
				t.Errorf("agent field %s declared unused but set to %v", p, al[p].Interface())
			}
		default:
			t.Errorf("agent field %s is neither filled from galera nor declared unused", p)
		}
	}
}

// fill sets every field reachable from v to a distinct non-zero value.
func fill(v reflect.Value, seed *int) {
	*seed++
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			fill(v.Field(i), seed)
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem(), seed)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0), seed)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		e := reflect.New(v.Type().Elem()).Elem()
		fill(k, seed)
		fill(e, seed)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.String:
		v.SetString(fmt.Sprintf("v%d", *seed))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(*seed))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(*seed))
	case reflect.Bool:
		v.SetBool(true)
	default:
		panic("fill: unsupported kind " + v.Kind().String())
	}
}

// leaves records the scalar, map and scalar-slice values below v by path.
// Struct slices are descended through their first element ("Mounts[]"),
// nil pointers through a zero value so their paths still appear.
func leaves(v reflect.Value, prefix string, out map[string]reflect.Value) {
	join := func(name string) string {
		if prefix == "" {
			return name
		}
		return prefix + "." + name
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			leaves(reflect.Zero(v.Type().Elem()), prefix, out)
		} else {
			leaves(v.Elem(), prefix, out)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			leaves(v.Field(i), join(v.Type().Field(i).Name), out)
		}
	case reflect.Slice:
		if k := v.Type().Elem().Kind(); k == reflect.Struct || k == reflect.Pointer {
			e := reflect.Zero(v.Type().Elem())
			if v.Len() > 0 {
				e = v.Index(0)
			}
			leaves(e, prefix+"[]", out)
			return
		}
		out[prefix] = v
	default:
		out[prefix] = v
	}
}

func sortedKeys(m map[string]reflect.Value) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fieldNames(t reflect.Type) []string {
	var out []string
	for i := range t.NumField() {
		out = append(out, t.Field(i).Name+" "+t.Field(i).Type.String()+" "+string(t.Field(i).Tag))
	}
	return out
}
