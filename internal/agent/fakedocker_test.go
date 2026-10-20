// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"sync"
)

// fakeDocker is an in-memory DockerAPI that records every call.
type fakeDocker struct {
	mu      sync.Mutex
	objects map[string]Object
	calls   []string
	nextID  int
	// failNext, if set, is returned by the next mutating call.
	failNext error
	// onCreate runs inside CreateService before the object is stored.
	onCreate func()
	// specs holds the spec of every created service by ID.
	specs map[string]ServiceSpec
}

func newFakeDocker(objs ...Object) *fakeDocker {
	f := &fakeDocker{objects: map[string]Object{}, specs: map[string]ServiceSpec{}}
	for _, o := range objs {
		f.objects[o.ID] = o
	}
	return f
}

func (f *fakeDocker) record(format string, a ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
}

func (f *fakeDocker) takeFail() error {
	err := f.failNext
	f.failNext = nil
	return err
}

func (f *fakeDocker) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Mutations returns only the effectful calls.
func (f *fakeDocker) Mutations() []string {
	var out []string
	for _, c := range f.Calls() {
		switch {
		case len(c) >= 7 && c[:7] == "inspect", len(c) >= 4 && c[:4] == "list":
		default:
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeDocker) CreateService(_ context.Context, spec ServiceSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create-service %s", spec.Name)
	if err := f.takeFail(); err != nil {
		return "", err
	}
	if f.onCreate != nil {
		f.onCreate()
	}
	f.nextID++
	id := fmt.Sprintf("svc-new-%d", f.nextID)
	f.objects[id] = Object{Kind: KindService, ID: id, Name: spec.Name, Labels: maps.Clone(spec.Labels), Mounts: spec.Mounts,
		Constraints: spec.Constraints, Replicas: spec.Replicas}
	f.specs[id] = spec
	return id, nil
}

func (f *fakeDocker) RemoveService(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove-service %s", id)
	if err := f.takeFail(); err != nil {
		return err
	}
	if o, ok := f.objects[id]; !ok || o.Kind != KindService {
		return ErrObjectNotFound
	}
	delete(f.objects, id)
	return nil
}

func (f *fakeDocker) ScaleService(_ context.Context, id string, replicas uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("scale-service %s %d", id, replicas)
	if err := f.takeFail(); err != nil {
		return err
	}
	if o, ok := f.objects[id]; !ok || o.Kind != KindService {
		return ErrObjectNotFound
	}
	return nil
}

func (f *fakeDocker) RemoveVolume(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove-volume %s", id)
	if err := f.takeFail(); err != nil {
		return err
	}
	if o, ok := f.objects[id]; !ok || o.Kind != KindVolume {
		return ErrObjectNotFound
	}
	delete(f.objects, id)
	return nil
}

func (f *fakeDocker) Inspect(_ context.Context, kind ObjectKind, id string) (Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("inspect %s %s", kind, id)
	o, ok := f.objects[id]
	if !ok {
		// Like Docker, networks and secrets can also be inspected by name.
		for _, c := range f.objects {
			if (kind == KindNetwork || kind == KindSecret) && c.Kind == kind && c.Name == id {
				o, ok = c, true
				break
			}
		}
	}
	if !ok {
		return Object{}, ErrObjectNotFound
	}
	o.Labels = maps.Clone(o.Labels)
	return o, nil
}

func (f *fakeDocker) ListByLabel(_ context.Context, kind ObjectKind, key, value string) ([]Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("list %s %s=%s", kind, key, value)
	var out []Object
	for _, o := range f.objects {
		if o.Kind == kind && o.Labels[key] == value {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *fakeDocker) CreateVolume(_ context.Context, spec VolumeSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create-volume %s", spec.Name)
	if err := f.takeFail(); err != nil {
		return err
	}
	f.objects[spec.Name] = Object{Kind: KindVolume, ID: spec.Name, Name: spec.Name, Labels: maps.Clone(spec.Labels), Mountpoint: spec.Device}
	return nil
}

func (f *fakeDocker) UpdateService(_ context.Context, id string, spec ServiceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("update-service %s", id)
	if err := f.takeFail(); err != nil {
		return err
	}
	o, ok := f.objects[id]
	if !ok || o.Kind != KindService {
		return ErrObjectNotFound
	}
	o.Labels, o.Mounts, o.Constraints, o.Replicas = maps.Clone(spec.Labels), spec.Mounts, spec.Constraints, spec.Replicas
	f.objects[id] = o
	f.specs[id] = spec
	return nil
}
