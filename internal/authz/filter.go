// SPDX-License-Identifier: AGPL-3.0-only

package authz

// Filter returns the items the subject may perform action on, preserving
// order. resourceOf maps an item to the resource it exposes. Use it for
// listings and for data shown inside shared objects: access to a container
// (for example a shared dashboard) never implies access to the resources its
// widgets display, so every widget's underlying resource is checked against
// the viewer's own grants.
func Filter[T any](c Checker, s Subject, action Action, items []T, resourceOf func(T) Resource) []T {
	out := make([]T, 0, len(items))
	for _, it := range items {
		if c.Check(s, action, resourceOf(it)).Allowed {
			out = append(out, it)
		}
	}
	return out
}
