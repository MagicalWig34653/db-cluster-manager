// SPDX-License-Identifier: AGPL-3.0-only

package galeraprobe

import (
	"context"
	"fmt"
	"strconv"

	"github.com/MagicalWig34653/db-cluster-manager/internal/galera"
	"github.com/MagicalWig34653/db-cluster-manager/internal/proxy"
)

// ProxyStatus adapts a Prober to proxy.GaleraStatusProber. It lives here (and
// not in internal/proxy) so the proxy package stays free of a MySQL client.
type ProxyStatus struct {
	Prober *Prober
}

var _ proxy.GaleraStatusProber = ProxyStatus{}

// GaleraStatus queries addr and converts the wsrep variables. Incomplete or
// unparsable answers are errors, so the proxy keeps the node sync-unknown.
func (a ProxyStatus) GaleraStatus(ctx context.Context, addr string) (proxy.GaleraStatus, error) {
	vars, err := a.Prober.Status(ctx, addr)
	if err != nil {
		return proxy.GaleraStatus{}, err
	}
	return StatusFromVars(vars)
}

// StatusFromVars converts SHOW GLOBAL STATUS variables to the proxy's view.
// Ready is true only when wsrep_ready and wsrep_connected are ON and the
// cluster size is positive.
func StatusFromVars(vars map[string]string) (proxy.GaleraStatus, error) {
	h := galera.HealthFromStatus(vars)
	if h.State == galera.HealthUnknown {
		return proxy.GaleraStatus{}, fmt.Errorf("galeraprobe: unusable wsrep status: %v", h.Reasons)
	}
	ls, err := strconv.Atoi(vars["wsrep_local_state"])
	if err != nil {
		return proxy.GaleraStatus{}, fmt.Errorf("galeraprobe: unparsable wsrep_local_state %q", vars["wsrep_local_state"])
	}
	return proxy.GaleraStatus{
		LocalState:        ls,
		LocalStateComment: h.LocalState,
		ClusterStatus:     h.ClusterStatus,
		Ready:             h.Ready && h.Connected && h.ClusterSize > 0,
	}, nil
}
