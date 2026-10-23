// SPDX-License-Identifier: AGPL-3.0-only

package imagebuild

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/MagicalWig34653/db-cluster-manager/internal/registry"
)

// FakeBuilder is a Builder that builds nothing. It is a test double: it
// records inputs, writes configurable log lines and derives a deterministic
// image digest from the inputs. It must never be wired into a real manager.
type FakeBuilder struct {
	mu    sync.Mutex
	Calls []BuildInput
	// Err makes Build fail after logging.
	Err error
	// LogLines are written to the build log (tests put secrets here to
	// check redaction).
	LogLines []string
}

var _ Builder = (*FakeBuilder)(nil)

// Build implements Builder.
func (f *FakeBuilder) Build(_ context.Context, in BuildInput) (BuildOutput, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, in)
	f.mu.Unlock()
	if in.Log != nil {
		for _, l := range f.LogLines {
			fmt.Fprintln(in.Log, l)
		}
		fmt.Fprintln(in.Log, "fake build of "+in.Reference)
	}
	if f.Err != nil {
		return BuildOutput{}, f.Err
	}
	digest := registry.DigestOf([]byte(in.Rendered.ContextHash + "|" + strings.Join(in.Platforms, ",") + "|" + in.Reference))
	return BuildOutput{ImageDigest: digest, BaseDigests: baseDigests(in.Rendered), SBOM: SBOMNotGenerated}, nil
}

// baseDigests extracts the digest of the pinned base reference.
func baseDigests(r Rendered) []string {
	if _, d, ok := strings.Cut(r.BaseImage, "@"); ok {
		return []string{d}
	}
	return nil
}
