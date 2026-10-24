// SPDX-License-Identifier: AGPL-3.0-only

package manager

import (
	"slices"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/service"
)

func TestServiceRefsExcludeConfigsCreatedByPlan(t *testing.T) {
	p := service.Plan{Steps: []service.Step{
		{Kind: service.StepCreateConfig, Config: "managed"},
		{Kind: service.StepCreateService, Spec: &service.ServiceSpec{Networks: []string{"net"}, Secrets: []string{"pw"}, Configs: []service.ConfigRef{{Config: "managed"}, {Config: "external"}}}},
	}}
	r := serviceRefs(p)
	if !slices.Equal(r.Configs, []string{"external"}) || !slices.Equal(r.Networks, []string{"net"}) || !slices.Equal(r.Secrets, []string{"pw"}) {
		t.Fatal(r)
	}
}
