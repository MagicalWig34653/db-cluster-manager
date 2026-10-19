// SPDX-License-Identifier: AGPL-3.0-only

package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

// Snapshot lists Lima instances (`limactl list --json`) and Colima profiles
// (`colima list --json`) as a sorted, de-duplicated []string of "lima:NAME"
// and "colima:NAME" entries. Status is deliberately ignored. A missing colima
// binary (exec.ErrNotFound) means no Colima profiles; any other error fails.
func Snapshot(ctx context.Context, r Runner) ([]string, error) {
	set := map[string]bool{}
	out, err := r.Run(ctx, "limactl", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("lab: list Lima instances: %w", err)
	}
	names, err := parseNames(out)
	if err != nil {
		return nil, fmt.Errorf("lab: parse limactl list output: %w", err)
	}
	for _, n := range names {
		set["lima:"+n] = true
	}
	out, err = r.Run(ctx, "colima", "list", "--json")
	switch {
	case errors.Is(err, exec.ErrNotFound):
		out = ""
	case err != nil:
		return nil, fmt.Errorf("lab: list Colima profiles: %w", err)
	}
	names, err = parseNames(out)
	if err != nil {
		return nil, fmt.Errorf("lab: parse colima list output: %w", err)
	}
	for _, n := range names {
		set["colima:"+n] = true
	}
	return sortedKeys(set), nil
}

type namedEntry struct {
	Name *string `json:"name"`
}

// parseNames accepts newline-delimited JSON objects or a single JSON array
// of objects; each object must carry a non-empty "name".
func parseNames(out string) ([]string, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	var entries []namedEntry
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
			return nil, err
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
		for {
			var e namedEntry
			err := dec.Decode(&e)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			entries = append(entries, e)
		}
	}
	names := make([]string, 0, len(entries))
	for i, e := range entries {
		if e.Name == nil || *e.Name == "" {
			return nil, fmt.Errorf("entry %d has no name", i)
		}
		names = append(names, *e.Name)
	}
	return names, nil
}

// Unrelated returns snapshot entries that are not owned by the manifest. Only
// manifest entries with a valid lab name count as owned, so a hand-edited
// foreign name can never hide an unrelated resource.
func Unrelated(snapshot []string, m *Manifest) []string {
	owned := map[string]bool{}
	if m != nil {
		for _, r := range m.Resources {
			if !IsLabName(r.Name) {
				continue
			}
			switch r.Kind {
			case "lima-vm":
				owned["lima:"+r.Name] = true
			case "colima-profile":
				owned["colima:"+r.Name] = true
			}
		}
	}
	out := []string{}
	for _, e := range snapshot {
		if !owned[e] {
			out = append(out, e)
		}
	}
	return out
}

// Diff reports sorted entries added or removed between two snapshots.
func Diff(before, after []string) (added, removed []string) {
	b, a := toSet(before), toSet(after)
	addedSet, removedSet := map[string]bool{}, map[string]bool{}
	for e := range a {
		if !b[e] {
			addedSet[e] = true
		}
	}
	for e := range b {
		if !a[e] {
			removedSet[e] = true
		}
	}
	return sortedKeys(addedSet), sortedKeys(removedSet)
}

func toSet(s []string) map[string]bool {
	set := make(map[string]bool, len(s))
	for _, e := range s {
		set[e] = true
	}
	return set
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
