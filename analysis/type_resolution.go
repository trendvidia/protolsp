// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Type-resolution helpers that combine the user's registry-backed
// lookup with protolsp's internal descriptors. Every site in the
// server that needs to resolve `@type` to a MessageType — validate,
// hover, completion, definition, chain-aware features — should go
// through these helpers rather than touching s.lookup directly.
//
// Behavior:
//   - User's lookup wins when it can resolve the name. A workspace
//     that publishes its own `protolsp.v1.WorkspaceConfig` to its
//     registry would shadow ours — unusual, but not blocked.
//   - When the user has no lookup at all, or their lookup returns
//     NotFound, the internal lookup (protolsp.v1.* + chameleon.v1.*)
//     gets the shot.
//   - Returns nil + error when neither lookup has the type. The
//     error is the underlying lookup's (typically protoregistry.NotFound).
//
// Why a helper rather than a composite Lookup wrapping s.lookup:
// the `s.lookup == nil` checks scattered across the server are
// load-bearing for tests that exercise the "no registry" path.
// Keeping s.lookup nil-able preserves those tests; the helper
// surfaces a non-nil resolution when the internal lookup can
// answer, without changing the field's semantics.

package analysis

import (
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// findType resolves a message FQN through the user's lookup first,
// then the internal lookup. Returns (nil, error) when neither has
// the name. The error matches what the underlying lookup returned
// — typically protoregistry.NotFound — so existing callsites that
// switch on `errors.Is(err, protoregistry.NotFound)` keep working.
// findTypeForDoc resolves a message FQN through the doc's
// workspace lookup first, then the server-wide root lookup,
// then the internal lookup. doc may be nil — that path treats
// "no doc" as "use root lookup" (suitable for handlers without
// document context like @type completion before didOpen).
//
// Sub-workspaces that declare their own registry get their
// own lookup; passing the doc lets the resolver pick THAT
// lookup over the root for documents under that sub-workspace.
// Sub-workspaces without their own registry naturally fall
// through to the root via lookupForDoc.
func (s *Workspace) findTypeForDoc(doc *Document, name protoreflect.FullName) (protoreflect.MessageType, error) {
	if doc != nil {
		if mt := doc.inline.findMessage(name); mt != nil {
			return mt, nil
		}
	}
	if lk := s.LookupForDoc(doc); lk != nil {
		if mt, err := lk.FindMessageByName(name); err == nil {
			return mt, nil
		}
	}
	if s != nil && s.internalLookup != nil {
		return s.internalLookup.FindMessageByName(name)
	}
	return nil, protoregistry.NotFound
}

// hasUserLookup reports whether the user configured a registry-
// backed lookup (i.e. s.lookup is non-nil). Distinct from "any
// lookup is available" — the internal lookup is always available
// but doesn't count as a user-configured one.
//
// Used by features that should ONLY fire when the user has opted
// into schema validation by configuring a registry — e.g., the
// "unknown type" diagnostic. Without this gate, every random
// `@type` directive would fire a spurious error on workspaces
// that haven't set up a registry.
func (s *Workspace) hasUserLookup() bool {
	return s.lookupMgr.Snapshot() != nil
}

// effectiveTypeURL returns the message FQN protolsp should treat
// as bound to the document, considering both the file's own
// `@type` directive AND chameleon's per-workspace default for
// layer files.
//
// Precedence:
//  1. The doc's own `@type` directive (`doc.Parsed.TypeURL`).
//  2. `chameleon.schema_fqn` from `.protolsp.pxf` — but only when
//     the doc has been classified as a chain layer
//     (`doc.LayerInfo.IsLayer`). The setting is *documented* as
//     "Default @type for chain layer files that don't carry one";
//     gating on IsLayer keeps the default from leaking into
//     unrelated `.pxf` files in the same workspace.
//
// Returns "" when neither source supplies a name — read-only
// handlers (hover, definition, completion, …) treat that as
// "no schema bound" and return null/empty, matching their
// pre-existing behavior for unbound docs.
//
// Callers should pass doc.mu.RLock() held already (or rely on
// the LayerInfo / Parsed snapshots being stable through the
// handler). The helper itself does no locking.
func (s *Workspace) effectiveTypeURL(doc *Document) string {
	if doc == nil {
		return ""
	}
	if doc.Parsed != nil && doc.Parsed.TypeURL != "" {
		return doc.Parsed.TypeURL
	}
	// Mid-edit: when the current parse fails, callers fall back
	// to LastGoodParse for AST traversal — the same fallback has
	// to apply to the type lookup or completion/hover/etc. would
	// regress during every transient invalid edit.
	if doc.LastGoodParse != nil && doc.LastGoodParse.TypeURL != "" {
		return doc.LastGoodParse.TypeURL
	}
	if doc.LayerInfo == nil || !doc.LayerInfo.IsLayer {
		return ""
	}
	fc := s.SnapshotFileConfig()
	if fc == nil {
		return ""
	}
	return fc.Chameleon.SchemaFQN
}

// docTypeLookup resolves names as findTypeForDoc does for doc, for the
// passes that take a narrow lookup.
type docTypeLookup struct {
	s   *Workspace
	doc *Document
}

func (l docTypeLookup) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	return l.s.findTypeForDoc(l.doc, name)
}
