// SPDX-License-Identifier: AGPL-3.0-only

package lab

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"
)

// LabDir is the working-directory-relative directory that holds lab state
// (the default manifest lives at LabDir/manifest.json). Resources of kind
// "directory" are bare names resolved below it.
const LabDir = ".lab"

// maxNameLen bounds lab resource names; Lima derives host names and socket
// paths from instance names, so they are kept short.
const maxNameLen = 48

// Action is one planned external command.
type Action struct {
	Describe string    // human readable, one line
	Command  []string  // exact argv, e.g. {"limactl","start","--tty=false","dbcm-lab-m1"}
	Resource *Resource // resource recorded in (or, with Remove, removed from) the manifest after success; nil for none
	Remove   bool      // true: Resource is removed from the manifest after success instead of added
}

// IsLabName reports whether name is a valid lab-owned resource name:
// NamePrefix followed by at least one character of [a-z0-9-], starting with
// [a-z0-9]. This excludes path separators, whitespace, shell metacharacters
// and option-like names.
func IsLabName(name string) bool {
	if len(name) > maxNameLen || len(name) <= len(NamePrefix) || name[:len(NamePrefix)] != NamePrefix {
		return false
	}
	rest := name[len(NamePrefix):]
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// gib formats a byte count as a GiB value accepted by limactl's float flags.
func gib(b uint64) string {
	return strconv.FormatFloat(float64(b)/float64(1<<30), 'f', -1, 64)
}

// PlanCreate validates the specs against the budget (including lab VMs
// already recorded in the manifest), requires valid lab names that are
// neither duplicated nor already recorded, and returns two actions per VM:
// `limactl create` (which records the VM) followed by `limactl start`.
func PlanCreate(b Budget, m *Manifest, specs []VMSpec) ([]Action, error) {
	if len(specs) == 0 {
		return nil, errors.New("lab: no VM specs given")
	}
	if m == nil {
		return nil, errors.New("lab: nil manifest")
	}
	seen := make(map[string]bool, len(specs))
	for _, s := range specs {
		if !IsLabName(s.Name) {
			return nil, fmt.Errorf("lab: invalid VM name %q: must be %q followed by [a-z0-9-]", s.Name, NamePrefix)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("lab: duplicate VM name %q", s.Name)
		}
		seen[s.Name] = true
		if _, ok := m.Find("lima-vm", s.Name); ok {
			return nil, fmt.Errorf("lab: VM %q is already recorded in the manifest", s.Name)
		}
	}
	all := make([]VMSpec, 0, len(specs)+len(m.Resources))
	for _, r := range m.Resources {
		if r.Kind == "lima-vm" && r.Spec != nil {
			all = append(all, *r.Spec)
		}
	}
	all = append(all, specs...)
	if err := b.Check(all); err != nil {
		return nil, err
	}
	actions := make([]Action, 0, 2*len(specs))
	for _, s := range specs {
		spec := s
		actions = append(actions,
			Action{
				Describe: fmt.Sprintf("create Lima VM %s (%d CPUs, %s GiB RAM, %s GiB disk, template docker)", s.Name, s.CPUs, gib(s.RAMBytes), gib(s.DiskBytes)),
				Command: []string{"limactl", "create",
					"--name=" + s.Name,
					"--cpus=" + strconv.Itoa(s.CPUs),
					"--memory=" + gib(s.RAMBytes),
					"--disk=" + gib(s.DiskBytes),
					"--tty=false",
					"template:docker"},
				Resource: &Resource{Kind: "lima-vm", Name: s.Name, Spec: &spec},
			},
			Action{
				Describe: "start Lima VM " + s.Name,
				Command:  []string{"limactl", "start", "--tty=false", s.Name},
			})
	}
	return actions, nil
}

// PlanCleanup returns, for each manifest resource of kind "lima-vm",
// `limactl stop <name>` and `limactl delete --force <name>`, followed by
// `rm -rf -- LabDir/<name>` for each "directory" resource. Only names
// accepted by IsLabName are considered; every other entry (including
// hand-edited foreign names) is ignored. Duplicates are planned once.
// Other kinds are not cleaned up by this planner.
func PlanCleanup(m *Manifest) []Action {
	if m == nil {
		return nil
	}
	var vms, dirs []Action
	seen := map[string]bool{}
	for _, r := range m.Resources {
		if !IsLabName(r.Name) || seen[r.Kind+"\x00"+r.Name] {
			continue
		}
		res := Resource{Kind: r.Kind, Name: r.Name}
		switch r.Kind {
		case "lima-vm":
			seen[r.Kind+"\x00"+r.Name] = true
			vms = append(vms,
				Action{Describe: "stop Lima VM " + r.Name, Command: []string{"limactl", "stop", r.Name}},
				Action{Describe: "delete Lima VM " + r.Name, Command: []string{"limactl", "delete", "--force", r.Name}, Resource: &res, Remove: true})
		case "directory":
			seen[r.Kind+"\x00"+r.Name] = true
			path := filepath.Join(LabDir, r.Name)
			dirs = append(dirs, Action{Describe: "remove lab directory " + path, Command: []string{"rm", "-rf", "--", path}, Resource: &res, Remove: true})
		}
	}
	return append(vms, dirs...)
}

// Execute runs actions in order and stops at the first error. After each
// successful action it updates the manifest (Add, or Remove when
// Action.Remove is set) and calls save, so the manifest always reflects what
// succeeded. Actions with an empty command or a non-lab resource name are
// rejected before anything runs.
func Execute(ctx context.Context, r Runner, m *Manifest, actions []Action, save func() error) error {
	if m == nil {
		return errors.New("lab: nil manifest")
	}
	for i, a := range actions {
		if len(a.Command) == 0 {
			return fmt.Errorf("lab: action %d (%s) has no command", i, a.Describe)
		}
		if a.Resource != nil && !IsLabName(a.Resource.Name) {
			return fmt.Errorf("lab: action %d (%s) refers to non-lab resource %q", i, a.Describe, a.Resource.Name)
		}
	}
	for _, a := range actions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := r.Run(ctx, a.Command[0], a.Command[1:]...); err != nil {
			return fmt.Errorf("lab: %s: %w", a.Describe, err)
		}
		if a.Resource == nil {
			if err := save(); err != nil {
				return fmt.Errorf("lab: save manifest after %s: %w", a.Describe, err)
			}
			continue
		}
		if a.Remove {
			m.Remove(a.Resource.Kind, a.Resource.Name)
		} else {
			res := *a.Resource
			if res.Spec != nil {
				spec := *res.Spec
				res.Spec = &spec
			}
			if res.Created.IsZero() {
				res.Created = time.Now().UTC()
			}
			if err := m.Add(res); err != nil {
				return err
			}
		}
		if err := save(); err != nil {
			return fmt.Errorf("lab: save manifest after %s: %w", a.Describe, err)
		}
	}
	return nil
}
