// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// C7 of the chameleon-integration design (first half): redundancy
// hint for `pxf.default` annotations. When a field has a
// `[(pxf.default) = X]` annotation in its .proto and the document
// assigns the same value to it, the runtime would have produced
// that value anyway — the assignment is no-op. We emit a Hint-
// severity diagnostic ("matches default; consider removing") to
// nudge users to clean up.
//
// Hint vs Information vs Warning: Hint is the right severity for
// this kind of stylistic feedback. Editors typically render Hints
// as a subtle underline (not the loud red squiggle) and surface
// the message only on hover. The user can ignore them indefinitely
// without visual debt.
//
// Comparison is value-repr-based: we render the assignment's
// pxf.Value via formatValueRepr (same helper the chain-resolution
// hover uses) and compare strings to pxf.Default(fd)'s returned
// literal. Both produce stable canonical forms for simple types
// (`"abc"` for strings, `42` for ints, `true` for bools); the
// comparison is exact-string-match, no normalization. Edge cases
// — e.g. `default = 42` vs an assignment that wrote `0x2A` — would
// fall out as "doesn't match" and silently skip the hint, which
// errs on the side of not bothering the user about a value they
// chose to express differently.
//
// Env-source hints (the design's other C7 piece) are deferred to
// a follow-up: they require the chain spec parser to read the
// `sources` map + chameleon's UPPER_SNAKE naming heuristic.
// Splitting them keeps this slice tight.

package analysis

import (
	"fmt"

	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// defaultRedundancyDiagnostics emits one Hint per Assignment whose
// value matches the field's `pxf.default` annotation. Walks
// recursively into Block-typed fields so `db { host = "..." }`
// gets the same treatment as `top_level = "..."`.
//
// Silent when there's no schema bound (no descriptor to read
// annotations from) and when the document didn't parse.
func (s *Workspace) defaultRedundancyDiagnostics(doc *Document) []protocol.Diagnostic {
	if s == nil || doc == nil || doc.Parsed == nil {
		return nil
	}
	// Layer files commonly omit @type and rely on chameleon's
	// schema_fqn default. effectiveTypeURL bridges the fallback so
	// the "matches default — consider removing" hints fire on
	// layer files too.
	typeURL := s.effectiveTypeURL(doc)
	if typeURL == "" {
		return nil
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(typeURL))
	if err != nil || mt == nil {
		return nil
	}
	var out []protocol.Diagnostic
	walkAssignmentsAgainstDescriptor(doc.Parsed.Entries, mt.Descriptor(),
		func(name string, value pxf.Value, pos pxf.Position, fd protoreflect.FieldDescriptor) {
			if fd == nil || value == nil {
				return
			}
			defaultLiteral, has := pxf.Default(fd)
			if !has {
				return
			}
			if formatValueRepr(value) != defaultLiteral {
				return
			}
			out = append(out, protocol.Diagnostic{
				Range:    pxfPosToRange(pos),
				Severity: protocol.DiagnosticSeverityHint,
				Source:   "protolsp/schema",
				Message: fmt.Sprintf(
					"field %q matches default %s; consider removing",
					name, defaultLiteral),
			})
		})
	return out
}

// walkAssignmentsAgainstDescriptor iterates entries, resolving
// each Assignment / MapEntry / Block to its FieldDescriptor on
// the enclosing MessageDescriptor and invoking fn. Descends into
// Block-typed fields whose descriptor names a sub-message. Mirrors
// walkFieldAssignments but passes the value along — the latter is
// shape-only and was built for unknown-field detection.
//
// MapEntry is not walked: pxf.default on map values isn't part of
// the runtime semantics today, and walking map entries would
// require a different signature (map value isn't a Value the way
// Assignment.Value is).
func walkAssignmentsAgainstDescriptor(
	entries []pxf.Entry,
	md protoreflect.MessageDescriptor,
	fn func(name string, value pxf.Value, pos pxf.Position, fd protoreflect.FieldDescriptor),
) {
	if md == nil {
		return
	}
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			fd := md.Fields().ByName(protoreflect.Name(n.Key))
			fn(n.Key, n.Value, n.Pos, fd)
		case *pxf.Block:
			fd := md.Fields().ByName(protoreflect.Name(n.Name))
			if fd != nil && fd.Message() != nil {
				walkAssignmentsAgainstDescriptor(n.Entries, fd.Message(), fn)
			}
		}
	}
}
