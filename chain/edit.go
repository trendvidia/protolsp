// AST-level edits on chain.pxf documents. Mirrors the
// workspaceconfig/edit.go pattern: the in-memory pxf.Document is
// mutated in place so re-emitting via pxf.FormatDocument preserves
// every non-target entry (comments, sibling environments,
// unrelated fields).
//
// The chain.pxf schema differs from .protolsp.pxf in shape — the
// environments list is `environments = [ { name = "…" … } ]`
// (an assignment carrying a ListVal of BlockVals), not a flat
// key-value tree — so this file's helpers are distinct from the
// workspaceconfig analogues but follow the same round-trip
// preservation rules.

package chain

import (
	"errors"
	"fmt"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// ErrEnvironmentNotFound is returned by SetEnvironmentKeyName when
// the named environment isn't declared in the spec's
// `environments = [...]` list. Callers surface this as a typed
// error so the UI can prompt the user to add the environment
// first rather than silently no-op.
var ErrEnvironmentNotFound = errors.New("chain: environment not found")

// SetEnvironmentKeyName mutates doc so the environment named env's
// `key_name` assignment equals value. Behavior:
//
//   - value != "" and the field exists → replace the StringVal.
//   - value != "" and the field is absent → append a new assignment
//     onto the matching BlockVal's Entries (preserves declaration
//     order of every other field).
//   - value == "" and the field exists → remove the assignment
//     (cleaner output than emitting `key_name = ""` — both forms
//     mean the same thing to the resolver, but the absence is the
//     canonical "inherit from parent" signal).
//   - value == "" and the field is absent → no-op.
//
// Returns ErrEnvironmentNotFound when no BlockVal in the
// environments list has `name = "<env>"`. Returns a wrapping error
// when the document's structure doesn't match the chain.pxf shape
// (no top-level `environments = [...]` assignment, or it's not a
// list of blocks).
func SetEnvironmentKeyName(doc *pxf.Document, env, value string) error {
	if doc == nil {
		return fmt.Errorf("chain: nil document")
	}
	if env == "" {
		return fmt.Errorf("chain: environment name is empty")
	}
	envsList, err := findEnvironmentsList(doc)
	if err != nil {
		return err
	}
	block, err := findEnvironmentBlock(envsList, env)
	if err != nil {
		return err
	}
	return setKeyNameOn(block, value)
}

// findEnvironmentsList walks doc's top-level entries for the
// `environments = [...]` assignment and returns its ListVal.
// Errors when the assignment is missing or carries a non-list
// value — both are spec-level malformations that the caller
// surfaces back to the user.
func findEnvironmentsList(doc *pxf.Document) (*pxf.ListVal, error) {
	for _, e := range doc.Entries {
		assign, ok := e.(*pxf.Assignment)
		if !ok {
			continue
		}
		if assign.Key != "environments" {
			continue
		}
		lv, ok := assign.Value.(*pxf.ListVal)
		if !ok {
			return nil, fmt.Errorf("chain: `environments` is not a list value")
		}
		return lv, nil
	}
	return nil, fmt.Errorf("chain: no `environments = [...]` assignment in document")
}

// findEnvironmentBlock returns the BlockVal whose `name = "<env>"`
// matches. Iterates in declaration order so a duplicate (rare)
// picks the first occurrence, matching parser behavior elsewhere
// in this package.
func findEnvironmentBlock(lv *pxf.ListVal, env string) (*pxf.BlockVal, error) {
	for _, elem := range lv.Elements {
		bv, ok := elem.(*pxf.BlockVal)
		if !ok {
			continue
		}
		for _, entry := range bv.Entries {
			assign, ok := entry.(*pxf.Assignment)
			if !ok || assign.Key != "name" {
				continue
			}
			s, ok := stringValue(assign.Value)
			if !ok {
				continue
			}
			if s == env {
				return bv, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrEnvironmentNotFound, env)
}

// setKeyNameOn updates / appends / removes the `key_name`
// assignment within an environment BlockVal per the
// SetEnvironmentKeyName behavior table above.
func setKeyNameOn(bv *pxf.BlockVal, value string) error {
	for i, entry := range bv.Entries {
		assign, ok := entry.(*pxf.Assignment)
		if !ok || assign.Key != "key_name" {
			continue
		}
		if value == "" {
			// Drop the entry entirely so the round-tripped
			// document doesn't emit `key_name = ""`.
			bv.Entries = append(bv.Entries[:i], bv.Entries[i+1:]...)
			return nil
		}
		assign.Value = &pxf.StringVal{Value: value}
		return nil
	}
	if value == "" {
		// Absent + clear → no-op.
		return nil
	}
	bv.Entries = append(bv.Entries, &pxf.Assignment{
		Key:   "key_name",
		Value: &pxf.StringVal{Value: value},
	})
	return nil
}
