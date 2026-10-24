// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

// CreateConfig sends Data as base64, as required by the Engine API.
func (c *Client) CreateConfig(ctx context.Context, s agent.ConfigSpec) (string, error) {
	if !objectID.MatchString(s.Name) || len(s.Data) == 0 || len(s.Data) > 500*1024 || s.Secret {
		return "", fmt.Errorf("%w: invalid config", ErrInvalidArgument)
	}
	body := struct {
		Name   string
		Labels map[string]string
		Data   []byte
	}{s.Name, s.Labels, s.Data}
	var out struct{ ID string }
	if err := c.do(ctx, http.MethodPost, "/configs/create", nil, body, &out, true); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("%w: config create response has no ID", agent.ErrOutcomeUnknown)
	}
	return out.ID, nil
}

func (c *Client) RemoveConfig(ctx context.Context, id string) error {
	p, err := objectPath("/configs/", id)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodDelete, p, nil, nil, nil, true)
}
