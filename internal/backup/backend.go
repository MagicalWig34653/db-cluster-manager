// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotFound means the named object does not exist.
	ErrNotFound = errors.New("backup: object not found")
	// ErrInvalidName rejects names that could escape a backend namespace.
	ErrInvalidName = errors.New("backup: invalid object name")
)

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// Backend stores opaque (already encrypted) backup containers.
type Backend interface {
	// Put stores r under name, replacing any existing object atomically.
	Put(ctx context.Context, name string, r io.Reader) error
	// Get opens the named object; the caller must close it.
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	// List returns objects whose names start with prefix, sorted by name.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// Delete removes the object. Deleting a missing object returns ErrNotFound.
	Delete(ctx context.Context, name string) error
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || strings.HasPrefix(name, ".tmp-") {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	return nil
}

// DirBackend stores objects as files in one local directory. Writes go to a
// temporary file that is fsynced and renamed into place.
type DirBackend struct{ dir string }

// NewDirBackend creates the directory (mode 0700) if needed.
func NewDirBackend(dir string) (*DirBackend, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &DirBackend{dir: dir}, nil
}

// Put implements Backend.
func (b *DirBackend) Put(ctx context.Context, name string, r io.Reader) (err error) {
	if err := validName(name); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(b.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, &ctxReader{ctx, r}); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(b.dir, name)); err != nil {
		return err
	}
	if d, derr := os.Open(b.dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// Get implements Backend.
func (b *DirBackend) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(b.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// List implements Backend.
func (b *DirBackend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	des, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}
	var out []ObjectInfo
	for _, de := range des {
		n := de.Name()
		if de.IsDir() || strings.HasPrefix(n, ".tmp-") || !strings.HasPrefix(n, prefix) {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, ObjectInfo{Name: n, Size: fi.Size(), ModTime: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete implements Backend.
func (b *DirBackend) Delete(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(b.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// MemBackend is an in-memory Backend for tests.
type MemBackend struct {
	mu   sync.Mutex
	objs map[string]memObj
	now  func() time.Time
}

type memObj struct {
	data []byte
	mod  time.Time
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{objs: map[string]memObj{}, now: time.Now}
}

// Put implements Backend.
func (m *MemBackend) Put(ctx context.Context, name string, r io.Reader) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := io.ReadAll(&ctxReader{ctx, r})
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[name] = memObj{data: data, mod: m.now()}
	return nil
}

// Get implements Backend.
func (m *MemBackend) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[name]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), o.data...))), nil
}

// List implements Backend.
func (m *MemBackend) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ObjectInfo
	for n, o := range m.objs {
		if strings.HasPrefix(n, prefix) {
			out = append(out, ObjectInfo{Name: n, Size: int64(len(o.data)), ModTime: o.mod})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete implements Backend.
func (m *MemBackend) Delete(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[name]; !ok {
		return ErrNotFound
	}
	delete(m.objs, name)
	return nil
}

var (
	_ Backend = (*DirBackend)(nil)
	_ Backend = (*MemBackend)(nil)
)
