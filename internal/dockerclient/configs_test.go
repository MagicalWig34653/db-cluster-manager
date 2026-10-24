// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/MagicalWig34653/db-cluster-manager/internal/agent"
)

type configTransport func(*http.Request) (*http.Response, error)

func (f configTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConfigRequests(t *testing.T) {
	status, body := 201, `{"ID":"cfg1"}`
	calls := 0
	c := &Client{base: "http://docker", version: "1.47", hc: &http.Client{Transport: configTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == http.MethodPost {
			if r.URL.Path != "/v1.47/configs/create" {
				t.Fatal(r.URL)
			}
			var got struct {
				Name   string
				Labels map[string]string
				Data   []byte
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			if got.Name != "cfg" || string(got.Data) != "hello\n" || got.Labels["dbcm.cluster"] != "s" {
				t.Fatal(got)
			}
		} else if r.Method != http.MethodDelete || r.URL.Path != "/v1.47/configs/cfg1" || r.URL.RawQuery != "" {
			t.Fatal(r)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	spec := agent.ConfigSpec{Name: "cfg", Data: []byte("hello\n"), Labels: map[string]string{"dbcm.cluster": "s"}}
	if id, err := c.CreateConfig(context.Background(), spec); err != nil || id != "cfg1" {
		t.Fatal(id, err)
	}
	status, body = 204, ""
	if err := c.RemoveConfig(context.Background(), "cfg1"); err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{404, 409, 500} {
		status, body = code, `{"message":"failure"}`
		err := c.RemoveConfig(context.Background(), "cfg1")
		target := map[int]error{404: agent.ErrObjectNotFound, 409: agent.ErrConflict, 500: agent.ErrOutcomeUnknown}[code]
		if !errors.Is(err, target) {
			t.Fatal(err)
		}
	}
	status, body = 201, `{}`
	if _, err := c.CreateConfig(context.Background(), spec); !errors.Is(err, agent.ErrOutcomeUnknown) {
		t.Fatal(err)
	}
	before := calls
	spec.Secret = true
	if _, err := c.CreateConfig(context.Background(), spec); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
	if err := c.RemoveConfig(context.Background(), "../bad"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
	if calls != before {
		t.Fatal("invalid arguments sent a request")
	}
}
