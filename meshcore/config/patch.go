// Package config is meshcore's DOMAIN-FREE configuration store: byte-atomic writes, YAML/JSON
// format detection + conversion, pure map-level patch application, and a "validate before
// write / load" mechanism where the SCHEMA VALIDATOR IS INJECTED by the caller. It knows
// nothing of any app's typed schema (profiles, lanes, roles, explorers, collators) — an app
// supplies a ValidateBytes closure that enforces its own schema. This is the §4 ConfigAccess
// contract: meshcore applies validated patches, never learns the roster schema.
package config

import (
	"fmt"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
)

// Patch is a pure, minimal description of config changes: an ordered list of "set this dotted
// path to this value" (and "delete this dotted path") operations. It carries no I/O — a caller
// plans a Patch from already-validated choices, and Apply mutates a decoded config map before
// the store writes the result. It has no array ops (only set/delete of scalar/subtree leaves).
type Patch struct {
	Ops []SetOp
}

// SetOp sets the value at Path (a sequence of nested map keys; the last element is the leaf
// key). Intermediate maps are created as needed. When Delete is true it instead removes the leaf
// key at Path (a full-subtree delete for a nested key); deleting an absent path is a no-op and
// intermediate maps are never created for a delete.
type SetOp struct {
	Path   []string
	Value  any
	Delete bool
}

// IsEmpty reports whether the patch makes no changes (e.g. a no-op action).
func (p Patch) IsEmpty() bool { return len(p.Ops) == 0 }

// Apply mutates root (a decoded YAML/JSON object) in place, applying each SetOp: it walks
// (creating missing intermediate maps) and sets the leaf key. It preserves every key not named
// by an op. It returns an error if an intermediate path element exists but is not a mapping (so a
// malformed shape is never silently overwritten) — never panics.
func (p Patch) Apply(root map[string]any) error {
	if root == nil {
		return fault.New(fault.Internal, "config patch: nil root")
	}
	for _, op := range p.Ops {
		if len(op.Path) == 0 {
			return fault.New(fault.Internal, "config patch: empty path")
		}
		if op.Delete {
			if err := applyDelete(root, op.Path); err != nil {
				return err
			}
			continue
		}
		m := root
		for i := 0; i < len(op.Path)-1; i++ {
			k := op.Path[i]
			existing, ok := m[k]
			if !ok || existing == nil {
				next := map[string]any{}
				m[k] = next
				m = next
				continue
			}
			next, isMap := existing.(map[string]any)
			if !isMap {
				return fault.New(fault.Config,
					fmt.Sprintf("config: %q is not a mapping", strings.Join(op.Path[:i+1], ".")))
			}
			if next == nil {
				// a typed-nil map[string]any passes the assertion but cannot be written to;
				// replace it with a fresh map so we never panic on a leaf/child assignment.
				next = map[string]any{}
				m[k] = next
			}
			m = next
		}
		m[op.Path[len(op.Path)-1]] = op.Value
	}
	return nil
}

// applyDelete removes the leaf key at path. It walks only existing maps: a missing intermediate
// makes the delete a no-op (nothing to remove), and it never creates maps. A non-map intermediate
// is an error, so a malformed shape is surfaced rather than ignored.
func applyDelete(root map[string]any, path []string) error {
	m := root
	for i := 0; i < len(path)-1; i++ {
		existing, ok := m[path[i]]
		if !ok || existing == nil {
			return nil // parent path absent → nothing to delete
		}
		next, isMap := existing.(map[string]any)
		if !isMap {
			return fault.New(fault.Config,
				fmt.Sprintf("config: %q is not a mapping", strings.Join(path[:i+1], ".")))
		}
		m = next
	}
	delete(m, path[len(path)-1])
	return nil
}
