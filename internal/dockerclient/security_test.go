// SPDX-License-Identifier: AGPL-3.0-only

package dockerclient

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDaemonErrorCannotEchoSecretBody(t *testing.T) {
	secret := "synthetic-daemon-secret-7cc41"
	encoded := base64.StdEncoding.EncodeToString([]byte(secret))
	client := &Client{base: "http://docker", version: "1.47", hc: &http.Client{Transport: configTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1.47/secrets/create" {
			t.Fatalf("unexpected request %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !strings.Contains(string(body), encoded) {
			t.Fatal("fake did not see encoded secret")
		}
		return &http.Response{StatusCode: http.StatusBadRequest,
			Body: io.NopCloser(strings.NewReader(`{"message":"invalid data ` + encoded + ` ` + secret + `"}`)), Header: make(http.Header)}, nil
	})}}
	_, err := client.CreateSecret(context.Background(), "dbcm-synthetic", nil, []byte(secret))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("unexpected error %v", err)
	}
	for _, leaked := range []string{secret, encoded} {
		if strings.Contains(err.Error(), leaked) || strings.Contains(apiErr.Message, leaked) {
			t.Fatalf("daemon error leaked %q", leaked)
		}
	}
	if apiErr.Message != http.StatusText(http.StatusBadRequest) {
		t.Fatalf("unexpected message %q", apiErr.Message)
	}
}
