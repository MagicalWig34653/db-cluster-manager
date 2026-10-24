package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
	manifests "github.com/MagicalWig34653/db-cluster-manager/internal/service/k8s"
)

// REST implements a deliberately small, client-go-free API subset.
// Tokens are wrapped; kubeconfigs are stored opaquely, never executed or parsed
// for exec/auth-provider plugins. A caller must supply a validated TLS client.
type REST struct {
	mu     sync.Mutex
	base   string
	client *http.Client
	token  registry.Secret
	guard  ownership
}

func NewREST(enabled bool, endpoint, installation, namespace string, token registry.Secret, client *http.Client, inventory map[Ref]string) (*REST, error) {
	if !enabled {
		return nil, ErrDisabled
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, errors.New("kube: HTTPS origin required")
	}
	g, e := newOwnership(installation, namespace, inventory)
	if e != nil {
		return nil, e
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	cp := *client
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if cp.Timeout == 0 {
		cp.Timeout = 10 * time.Second
	}
	return &REST{base: endpoint, client: &cp, token: token, guard: g}, nil
}
func (c *REST) collection(resource Resource) (string, error) {
	info, ok := resources[resource]
	if !ok {
		return "", ErrNotOwned
	}
	prefix := "/apis/" + info.version
	if info.version == "v1" {
		prefix = "/api/v1"
	}
	if !info.global {
		prefix += "/namespaces/" + url.PathEscape(c.guard.namespace)
	}
	return prefix + "/" + string(resource), nil
}
func (c *REST) call(ctx context.Context, method, path string, body []byte, contentType string, out any) error {
	req, e := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if e != nil {
		return errors.New("kube: invalid request")
	}
	if !c.token.IsZero() {
		req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, e := c.client.Do(req)
	if e != nil {
		return errors.New("kube: transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return ErrNotFound
	}
	if resp.StatusCode == 409 {
		return ErrConflict
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errors.New("kube: API request rejected")
	}
	if out != nil {
		b, e := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
		if e != nil || len(b) > 4<<20 {
			return errors.New("kube: response too large")
		}
		if json.Unmarshal(b, out) != nil {
			return errors.New("kube: malformed API response")
		}
	}
	return nil
}
func (c *REST) Apply(ctx context.Context, o manifests.Object) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, e := objectRef(o)
	if e != nil {
		return e
	}
	_, _, m := o.Header()
	if !c.guard.allowed(r) || m.Labels[manifests.LabelInstallation] != c.guard.installation {
		return ErrNotOwned
	}
	base, _ := c.collection(r.Resource)
	target := base + "/" + url.PathEscape(r.Name)
	var old manifests.RawObject
	e = c.call(ctx, "GET", target, nil, "", &old)
	if e == nil {
		if e = c.guard.check(r, old.Metadata); e != nil {
			return e
		}
		if old.Metadata.ResourceVersion == "" {
			return ErrNotOwned
		}
	} else if !errors.Is(e, ErrNotFound) {
		return e
	} else if c.guard.inventory[r.key()] != "" {
		return ErrNotOwned
	}
	b, e := json.Marshal(o)
	if e != nil {
		return e
	}
	if old.Metadata.ResourceVersion == "" {
		// Atomic create refuses a racing foreign object; SSA alone cannot safely
		// adopt a name after a GET returned 404.
		var creation map[string]any
		if err := json.Unmarshal(b, &creation); err != nil {
			return err
		}
		creationMeta := creation["metadata"].(map[string]any)
		delete(creationMeta, "uid")
		delete(creationMeta, "resourceVersion")
		createBody, err := json.Marshal(creation)
		if err != nil {
			return err
		}
		if e = c.call(ctx, "POST", base+"?fieldManager=dbcm-groundwork", createBody, "application/json", &old); e != nil {
			return e
		}
		if old.Metadata.Name != r.Name || old.Metadata.Namespace != r.Namespace || old.Metadata.UID == "" || old.Metadata.ResourceVersion == "" || old.Metadata.Labels[manifests.LabelInstallation] != c.guard.installation {
			return ErrNotOwned
		}
		c.guard.inventory[r.key()] = old.Metadata.UID
	}
	var raw map[string]any
	if e = json.Unmarshal(b, &raw); e != nil {
		return e
	}
	md := raw["metadata"].(map[string]any)
	// CAS prevents adoption of an object created/replaced between GET and PATCH.
	md["resourceVersion"] = old.Metadata.ResourceVersion
	md["uid"] = old.Metadata.UID
	b, e = json.Marshal(raw)
	if e != nil {
		return e
	}
	var applied manifests.RawObject
	if e = c.call(ctx, "PATCH", target+"?fieldManager=dbcm-groundwork&force=false", b, "application/apply-patch+yaml", &applied); e != nil {
		return e
	}
	if applied.Metadata.UID == "" || applied.Metadata.Labels[manifests.LabelInstallation] != c.guard.installation || applied.Metadata.Name != r.Name || applied.Metadata.Namespace != r.Namespace {
		return ErrNotOwned
	}
	c.guard.inventory[r.key()] = applied.Metadata.UID
	return nil
}
func (c *REST) List(ctx context.Context, resource Resource) ([]manifests.RawObject, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	base, e := c.collection(resource)
	if e != nil {
		return nil, e
	}
	q := url.Values{"labelSelector": {manifests.LabelInstallation + "=" + c.guard.installation}}
	var result struct {
		Items    []manifests.RawObject `json:"items"`
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if e = c.call(ctx, "GET", base+"?"+q.Encode(), nil, "", &result); e != nil {
		return nil, e
	}
	if result.Metadata.Continue != "" {
		return nil, errors.New("kube: paginated inventory unsupported")
	}
	out := []manifests.RawObject{}
	for _, o := range result.Items {
		r, e := objectRef(o)
		if e == nil && r.Resource == resource && c.guard.check(r, o.Metadata) == nil {
			out = append(out, o)
		}
	}
	sortObjects(out)
	return out, nil
}
func sortObjects(out []manifests.RawObject) {
	slices.SortFunc(out, func(a, b manifests.RawObject) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
}
func (c *REST) Delete(ctx context.Context, r Ref) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.guard.allowed(r) {
		return ErrNotOwned
	}
	base, _ := c.collection(r.Resource)
	target := base + "/" + url.PathEscape(r.Name)
	var old manifests.RawObject
	if e := c.call(ctx, "GET", target, nil, "", &old); e != nil {
		return e
	}
	if e := c.guard.check(r, old.Metadata); e != nil {
		return e
	}
	if old.Metadata.ResourceVersion == "" {
		return ErrNotOwned
	}
	b, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]string{"uid": old.Metadata.UID, "resourceVersion": old.Metadata.ResourceVersion}})
	if e := c.call(ctx, "DELETE", target, b, "application/json", nil); e != nil {
		return e
	}
	delete(c.guard.inventory, r.key())
	return nil
}
