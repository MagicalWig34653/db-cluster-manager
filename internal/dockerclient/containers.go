// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// One-off local containers (not Swarm services): the agent runs short
// diagnostic programs on its own host with them, for example
// `mariadbd --wsrep-recover` on a stopped node's volume. Workers accept
// these calls (they are not Swarm manager operations).

var _ agent.ContainerRunner = (*Client)(nil)

// maxContainerLog bounds the captured output of a one-off container.
const maxContainerLog = 1 << 20

// RunningContainersWithVolume lists the IDs of running containers on this
// host that mount the named volume (GET /containers/json with the volume
// and status filters).
func (c *Client) RunningContainersWithVolume(ctx context.Context, volume string) ([]string, error) {
	if !objectID.MatchString(volume) {
		return nil, fmt.Errorf("%w: volume %q", ErrInvalidArgument, volume)
	}
	f, err := json.Marshal(map[string][]string{"volume": {volume}, "status": {"running"}})
	if err != nil {
		return nil, err
	}
	var list []struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodGet, "/containers/json", url.Values{"filters": {string(f)}}, nil, &list, false); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, x := range list {
		ids = append(ids, x.ID)
	}
	return ids, nil
}

// RunContainer creates, starts and waits for a one-off container without
// network, then reads its output and removes it (never its volumes). The
// container is removed even when waiting fails.
func (c *Client) RunContainer(ctx context.Context, r agent.ContainerRun) (agent.ContainerResult, error) {
	if r.Image == "" || len(r.Entrypoint) == 0 {
		return agent.ContainerResult{}, fmt.Errorf("%w: image and entrypoint required", ErrInvalidArgument)
	}
	type mount struct {
		Type     string `json:"Type"`
		Source   string `json:"Source"`
		Target   string `json:"Target"`
		ReadOnly bool   `json:"ReadOnly,omitempty"`
	}
	var mounts []mount
	for _, m := range r.Mounts {
		mounts = append(mounts, mount{Type: m.Type, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	body := map[string]any{
		"Image":           r.Image,
		"Entrypoint":      r.Entrypoint,
		"Cmd":             r.Cmd,
		"User":            r.User,
		"Labels":          r.Labels,
		"NetworkDisabled": true,
		"HostConfig": map[string]any{
			"NetworkMode": "none",
			"Mounts":      mounts,
			"AutoRemove":  false,
			"Memory":      r.MemoryBytes,
		},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/create", nil, body, &created, true); err != nil {
		return agent.ContainerResult{}, err
	}
	p, err := objectPath("/containers/", created.ID)
	if err != nil {
		return agent.ContainerResult{}, err
	}
	defer func() {
		// Remove with a fresh context: the caller's may have ended.
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.do(rctx, http.MethodDelete, p, url.Values{"force": {"1"}}, nil, nil, true)
	}()
	if err := c.do(ctx, http.MethodPost, p+"/start", nil, nil, nil, true); err != nil {
		return agent.ContainerResult{}, err
	}
	var waited struct {
		StatusCode int64 `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := c.do(ctx, http.MethodPost, p+"/wait", url.Values{"condition": {"not-running"}}, nil, &waited, true); err != nil {
		return agent.ContainerResult{}, err
	}
	res := agent.ContainerResult{ExitCode: waited.StatusCode}
	ver, err := c.NegotiateAPIVersion(ctx)
	if err != nil {
		return res, err
	}
	resp, err := c.send(ctx, http.MethodGet, "/v"+ver+p+"/logs?"+url.Values{"stdout": {"1"}, "stderr": {"1"}}.Encode(), nil, nil, false)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	out, err := demuxLogs(io.LimitReader(resp.Body, maxContainerLog))
	res.Output = out
	return res, err
}

// demuxLogs decodes the multiplexed log stream of a container without a
// TTY (8-byte frame headers: stream, 3 zero bytes, big-endian length) into
// one byte slice, stdout and stderr interleaved in order.
func demuxLogs(r io.Reader) ([]byte, error) {
	var out bytes.Buffer
	var h [8]byte
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return out.Bytes(), nil
			}
			return out.Bytes(), err
		}
		n := int64(binary.BigEndian.Uint32(h[4:]))
		if _, err := io.CopyN(&out, r, n); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return out.Bytes(), nil
			}
			return out.Bytes(), err
		}
	}
}
