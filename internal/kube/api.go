// Package kube provides an explicitly enabled Kubernetes API experiment.
// No controller or production manager wiring exists in W10.
package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	manifests "github.com/MagicalWig34653/db-cluster-manager/internal/service/k8s"
)

var (
	ErrDisabled = errors.New("kube: groundwork disabled")
	ErrNotOwned = errors.New("kube: object is outside ownership inventory")
	ErrNotFound = errors.New("kube: object not found")
	ErrConflict = errors.New("kube: conflict")
)

// Resource is an allow-listed Kubernetes resource, not an arbitrary URL.
type Resource string

const (
	Deployments   Resource = "deployments"
	StatefulSets  Resource = "statefulsets"
	Services      Resource = "services"
	Claims        Resource = "persistentvolumeclaims"
	Volumes       Resource = "persistentvolumes"
	ConfigMaps    Resource = "configmaps"
	IngressRoutes Resource = "ingressroutes"
)

type resourceInfo struct {
	version, kind string
	global        bool
}

var resources = map[Resource]resourceInfo{
	Deployments: {"apps/v1", "Deployment", false}, StatefulSets: {"apps/v1", "StatefulSet", false}, Services: {"v1", "Service", false}, Claims: {"v1", "PersistentVolumeClaim", false}, Volumes: {"v1", "PersistentVolume", true}, ConfigMaps: {"v1", "ConfigMap", false}, IngressRoutes: {"traefik.io/v1alpha1", "IngressRoute", false},
}

type Ref struct {
	Resource  Resource
	Namespace string
	Name      string
}

func (r Ref) key() string { return string(r.Resource) + "/" + r.Namespace + "/" + r.Name }
func objectRef(o manifests.Object) (Ref, error) {
	v, k, m := o.Header()
	for resource, info := range resources {
		if info.version == v && info.kind == k {
			return Ref{resource, m.Namespace, m.Name}, nil
		}
	}
	return Ref{}, errors.New("kube: unsupported object kind")
}

// KubeAPI is the only mutation boundary. Implementations never execute a
// service plan and do not decide whether any blocker may be overridden.
type KubeAPI interface {
	Apply(context.Context, manifests.Object) error
	List(context.Context, Resource) ([]manifests.RawObject, error)
	Delete(context.Context, Ref) error
}

type ownership struct {
	installation, namespace string
	inventory               map[string]string
}

func (g ownership) valid(r Ref) bool {
	info, ok := resources[r.Resource]
	return ok && validName(r.Name) && ((info.global && r.Namespace == "") || (!info.global && r.Namespace == g.namespace))
}
func validName(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' && i > 0 && i < len(s)-1) {
			return false
		}
	}
	return true
}
func (g ownership) allowed(r Ref) bool { _, ok := g.inventory[r.key()]; return g.valid(r) && ok }
func (g ownership) check(r Ref, m manifests.Metadata) error {
	uid, ok := g.inventory[r.key()]
	if !ok || !g.valid(r) || uid == "" || uid != m.UID || m.Labels[manifests.LabelInstallation] != g.installation || m.Name != r.Name || m.Namespace != r.Namespace {
		return ErrNotOwned
	}
	return nil
}
func newOwnership(installation, namespace string, inventory map[Ref]string) (ownership, error) {
	if installation == "" || !validName(namespace) {
		return ownership{}, errors.New("kube: installation and namespace required")
	}
	g := ownership{installation, namespace, map[string]string{}}
	for r, uid := range inventory {
		if !g.valid(r) {
			return ownership{}, ErrNotOwned
		}
		g.inventory[r.key()] = uid
	}
	return g, nil
}

// Fake is a concurrency-safe in-memory KubeAPI with the same ownership rules.
type Fake struct {
	mu      sync.Mutex
	guard   ownership
	objects map[string]manifests.RawObject
	next    int
}

func NewFake(enabled bool, installation, namespace string, inventory map[Ref]string) (*Fake, error) {
	if !enabled {
		return nil, ErrDisabled
	}
	g, e := newOwnership(installation, namespace, inventory)
	if e != nil {
		return nil, e
	}
	return &Fake{guard: g, objects: map[string]manifests.RawObject{}}, nil
}
func (f *Fake) Apply(_ context.Context, o manifests.Object) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, e := objectRef(o)
	if e != nil {
		return e
	}
	_, _, m := o.Header()
	if !f.guard.allowed(r) || m.Labels[manifests.LabelInstallation] != f.guard.installation {
		return ErrNotOwned
	}
	if old, ok := f.objects[r.key()]; ok {
		if e := f.guard.check(r, old.Metadata); e != nil {
			return e
		}
	} else if f.guard.inventory[r.key()] != "" {
		return ErrNotOwned
	}
	b, e := json.Marshal(o)
	if e != nil {
		return e
	}
	var raw manifests.RawObject
	if e := json.Unmarshal(b, &raw); e != nil {
		return e
	}
	raw.Metadata.UID = f.guard.inventory[r.key()]
	if raw.Metadata.UID == "" {
		f.next++
		raw.Metadata.UID = fmt.Sprintf("fake-%d", f.next)
	}
	raw.Metadata.ResourceVersion = fmt.Sprintf("%d", f.next+1)
	f.objects[r.key()] = raw
	f.guard.inventory[r.key()] = raw.Metadata.UID
	return nil
}
func (f *Fake) List(_ context.Context, resource Resource) ([]manifests.RawObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := resources[resource]; !ok {
		return nil, ErrNotOwned
	}
	out := []manifests.RawObject{}
	for _, o := range f.objects {
		r, _ := objectRef(o)
		if r.Resource == resource && f.guard.check(r, o.Metadata) == nil {
			b, _ := json.Marshal(o)
			var cp manifests.RawObject
			_ = json.Unmarshal(b, &cp)
			out = append(out, cp)
		}
	}
	slices.SortFunc(out, func(a, b manifests.RawObject) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
	return out, nil
}
func (f *Fake) Delete(_ context.Context, r Ref) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.guard.allowed(r) {
		return ErrNotOwned
	}
	o, ok := f.objects[r.key()]
	if !ok {
		return ErrNotFound
	}
	if e := f.guard.check(r, o.Metadata); e != nil {
		return e
	}
	delete(f.objects, r.key())
	delete(f.guard.inventory, r.key())
	return nil
}
