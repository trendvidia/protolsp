// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"fmt"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/redact"
	"github.com/trendvidia/protolsp/schema"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// HoverFor computes the Hover response for a (uri, position), or
// returns nil to signal "no hover info." nil is the LSP-correct way to
// say "I have nothing to add here" — clients render nothing in that
// case.
func (s *Workspace) HoverFor(docURI protocol.DocumentURI, pos protocol.Position) *protocol.Hover {
	// Phase 2 of proto authoring: .proto documents have their own
	// AST-based hover path. The .pxf flow below assumes a pxf.Parse
	// result on doc.Parsed and a bound MessageDescriptor; neither
	// applies to .proto documents. Routing them through the
	// dedicated handler keeps the two languages cleanly separated
	// without entangling pxf-specific helpers (effectiveTypeURL,
	// chameleon chain resolution, secret redaction) with
	// proto-specific ones.
	// Registry:// .proto previews (goed#26) route here too: read-only
	// shadow documents (see OpenShadowProto) carry a parsed AST but no
	// disk path, so the proto hover handler applies while the analyze
	// pipeline stays file-only.
	if IsProtoURI(uri.URI(docURI)) || IsRegistryProtoURI(uri.URI(docURI)) {
		return s.protoHoverFor(docURI, pos)
	}
	// Note: no s.lookup gate. Internal descriptors (protolsp.v1,
	// chameleon.v1) resolve via findType regardless of user
	// registry config, so hovering on a `.protolsp.pxf` field
	// works even without a registry. For user-domain types
	// (acme.demo.v1.*), findType returns nil → the rest of the
	// function exits cleanly via the existing nil checks below.
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	// RLock the doc for the duration: every branch below reads
	// doc.Parsed / doc.DatasetRegions / doc.LayerInfo / doc.secret
	// fields. Holding RLock across these is safe — handlers don't
	// re-enter and findTypeForDoc / chain-aware helpers go through
	// stateMu / lookupMu, not doc.mu.
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	// R3: region-aware hover. When the cursor lands on a
	// @dataset directive's start line, render type info from the
	// region's TypeFQN. Region awareness comes before the
	// standard entry-walk path because the document might have no
	// @type at all (per the v1 spec a @dataset doc MUST NOT have
	// @type or body entries) — without this branch hover would
	// always return nil for dataset documents.
	if region := s.RegionAtPosition(doc, pos); region != nil {
		if md := s.renderDatasetHover(doc, region, pos); md != "" {
			return &protocol.Hover{
				Contents: protocol.MarkupContent{
					Kind:  protocol.Markdown,
					Value: md,
				},
			}
		}
	}
	if doc.Parsed == nil {
		return nil
	}
	// effectiveTypeURL bridges the chameleon.schema_fqn fallback —
	// a chain layer file without its own @type still binds against
	// the workspace-configured default. doc.Parsed.TypeURL by
	// itself would skip every such layer.
	typeURL := s.effectiveTypeURL(doc)
	if typeURL == "" {
		return nil
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(typeURL))
	if err != nil {
		return nil
	}
	target, targetPath := findHoverTargetWithPath(doc.Parsed.Entries, mt.Descriptor(), pos.Line)
	if target == nil {
		return nil
	}
	markdown := renderHoverMarkdown(target, s.lookupMgr.Snapshot())
	// C6: merged-value hover. When the doc is a PER_ENV chain
	// layer, append a "Chain resolution" section listing the
	// field's value at every layer in the env's resolve order.
	// Returns "" for any standalone / ONCE-layer doc so the
	// existing hover passes through unchanged.
	if extra := s.renderChainResolution(doc, targetPath, target.descriptor); extra != "" {
		markdown += extra
	}
	// C7 part 2: env-source hover hint. When the chain includes
	// an env-kind source, surface the env var name the runtime
	// would consult for this field path. Skipped when no env
	// source is configured — purely additive UX.
	if extra := s.renderEnvSourceHint(doc, targetPath); extra != "" {
		markdown += extra
	}
	// S1: redact any pxf.Secret values that happen to appear in
	// the rendered markdown. Current renderHoverMarkdown doesn't
	// quote values, but defense in depth — future hover additions
	// (merged-value rows from chameleon, "value sourced from"
	// hints) might.
	if secrets := doc.secretValuesLocked(); len(secrets) > 0 {
		markdown = redact.String(markdown, secrets)
	}
	return &protocol.Hover{
		Contents: protocol.MarkupContent{
			Kind:  protocol.Markdown,
			Value: markdown,
		},
	}
}

// hoverTarget bundles the AST key under the cursor with the resolved
// field descriptor it refers to. fieldName is the as-written identifier
// from the source (Assignment.Key or Block.Name); descriptor is the
// resolved FieldDescriptor or nil if the key didn't match any field on
// the enclosing message.
type hoverTarget struct {
	fieldName  string
	descriptor protoreflect.FieldDescriptor
	// keyedElem, when non-nil, marks this target as a keyed repeated
	// field's entry (draft -01 §3.13): fieldName is the entry's key and
	// keyedElem is the element message the entry binds to. keyedField
	// names the enclosing keyed field. In this shape descriptor is nil
	// (a key is not a field), so renderHoverMarkdown branches on
	// keyedElem to describe the entry as an element rather than emitting
	// "unknown field".
	keyedElem  protoreflect.MessageDescriptor
	keyedField string
}

// findHoverTarget walks entries recursively, descending into Blocks
// when the cursor line is not their own line and the block name maps
// to a message-typed field on the enclosing descriptor. Returns nil
// when no entry matches the cursor line — the LSP returns "no hover".
//
// LSP positions are 0-indexed; pxf.Position.Line is 1-indexed. We
// compare pos.Line+1 against entry.Pos.Line to stay in 1-indexed
// space (avoids underflow when uint32 0 lands here).
func findHoverTarget(entries []pxf.Entry, md protoreflect.MessageDescriptor, cursorLine uint32) *hoverTarget {
	t, _ := findHoverTargetWithPath(entries, md, cursorLine)
	return t
}

// findHoverTargetWithPath is findHoverTarget plus the dotted path
// leading TO the target. Used by C6's chain-resolution hover, which
// needs to look up the field's value across layer files by full
// path. Returns ("", "") when no target matches the cursor line.
//
// Path semantics: the cursor on a top-level Assignment `api_url`
// yields path "api_url". Cursor on a nested `host` inside
// `db { host = ... }` yields path "db.host". Block-name hover —
// cursor on the `db` token of `db { ... }` — yields path "db".
func findHoverTargetWithPath(
	entries []pxf.Entry,
	md protoreflect.MessageDescriptor,
	cursorLine uint32,
) (*hoverTarget, string) {
	target := cursorLine + 1 // 1-indexed for pxf comparison
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			if uint32(n.Pos.Line) == target { // #nosec G115 -- parser lines are non-negative
				return &hoverTarget{
					fieldName:  n.Key,
					descriptor: md.Fields().ByName(protoreflect.Name(n.Key)),
				}, n.Key
			}
			// Descend into the value when it's a structured literal
			// (list of inline blocks, or a single block value). The
			// fields inside are typed by the parent field's message
			// descriptor — Field.Message() for both repeated and
			// singular cases.
			//
			// Concrete case: `environments = [ { name = "prod" } ]`.
			// Without this branch, hover on `name` returns nil
			// because the walker only matched Assignment lines at
			// the top level and never visited the array entries.
			fd := md.Fields().ByName(protoreflect.Name(n.Key))
			if fd != nil && fd.Message() != nil {
				if inner, innerPath := findInValue(n.Value, fd.Message(), cursorLine); inner != nil {
					return inner, n.Key + "." + innerPath
				}
			}
		case *pxf.Block:
			if uint32(n.Pos.Line) == target { // #nosec G115 -- parser lines are non-negative
				return &hoverTarget{
					fieldName:  n.Name,
					descriptor: md.Fields().ByName(protoreflect.Name(n.Name)),
				}, n.Name
			}
			fd := md.Fields().ByName(protoreflect.Name(n.Name))
			if pxf.KeyField(fd) != nil {
				// Keyed repeated field: its children are entries. Hovering
				// an entry name describes it as an element of the element
				// message; hovering inside an entry resolves against that
				// element (so nested fields hover correctly).
				elem := fd.Message()
				for _, ce := range n.Entries {
					ke, ok := asKeyedEntry(ce)
					if !ok {
						continue
					}
					if uint32(ke.pos.Line) == target { // #nosec G115 -- parser lines are non-negative
						return &hoverTarget{fieldName: ke.key, keyedElem: elem, keyedField: n.Name}, n.Name + "." + ke.key
					}
					if inner, innerPath := findHoverTargetWithPath(ke.body, elem, cursorLine); inner != nil {
						return inner, n.Name + "." + ke.key + "." + innerPath
					}
				}
				continue
			}
			if fd != nil && fd.Message() != nil {
				if inner, innerPath := findHoverTargetWithPath(n.Entries, fd.Message(), cursorLine); inner != nil {
					return inner, n.Name + "." + innerPath
				}
			}
		}
	}
	return nil, ""
}

// findInValue descends into a pxf.Value to find a hover target.
// Handles the two structured-literal shapes that carry a nested
// message:
//
//   - ListVal of BlockVal elements — typed by the parent field's
//     element message descriptor (the same `fd.Message()` regardless
//     of repeated cardinality).
//   - A bare BlockVal — used for map-entry rhs and (rarely) for
//     inline assignment of a message field.
//
// Scalar values (StringVal, IntVal, …) carry no sub-structure so
// hover descent stops at them; the parent Assignment's own line
// match is the only hover target in that path.
//
// Path component: array elements don't contribute a name to the
// dotted path — the user wrote `environments = [{name=…}]`, and
// the hover path for `name` is "environments.name" (one logical
// step into the parent field's element schema). Adding an index
// would clutter the path without helping any consumer.
func findInValue(
	v pxf.Value,
	elemMd protoreflect.MessageDescriptor,
	cursorLine uint32,
) (*hoverTarget, string) {
	switch val := v.(type) {
	case *pxf.ListVal:
		for _, el := range val.Elements {
			bv, ok := el.(*pxf.BlockVal)
			if !ok {
				continue
			}
			if inner, innerPath := findHoverTargetWithPath(bv.Entries, elemMd, cursorLine); inner != nil {
				return inner, innerPath
			}
		}
	case *pxf.BlockVal:
		if inner, innerPath := findHoverTargetWithPath(val.Entries, elemMd, cursorLine); inner != nil {
			return inner, innerPath
		}
	}
	return nil, ""
}

// renderHoverMarkdown formats a hoverTarget for the LSP client. Falls
// back gracefully when the field name didn't resolve against the
// schema — we still show what the user wrote (with an "unknown field"
// note) so they aren't left wondering whether the hover failed.
//
// Provenance: when a Lookup is available and reports a non-empty
// namespace origin for the field's defining file, the hover includes
// "from namespace <id>" next to the file path. Empty origin (ad-hoc
// parent registries) suppresses the label rather than rendering a
// confusing "from namespace “".
func renderHoverMarkdown(t *hoverTarget, lookup schema.Lookup) string {
	var b strings.Builder
	if t.keyedElem != nil {
		// A keyed repeated field's entry: the name is a free-form key,
		// the block binds to the element message. Show the element type
		// and which keyed field this entry belongs to.
		fmt.Fprintf(&b, "**%s** `%s`\n\n", t.fieldName, t.keyedElem.FullName())
		fmt.Fprintf(&b, "Entry of keyed field `%s`\n", t.keyedField)
		return b.String()
	}
	if t.descriptor == nil {
		fmt.Fprintf(&b, "**%s**\n\nUnknown field on this message.\n", t.fieldName)
		return b.String()
	}
	fd := t.descriptor
	fmt.Fprintf(&b, "**%s** `%s`\n\n", fd.Name(), fieldTypeLabel(fd))
	file := fd.ParentFile()
	origin := ""
	if lookup != nil {
		if _, ns, err := lookup.FindFileByPathWithOrigin(file.Path()); err == nil {
			origin = ns
		}
	}
	if origin != "" {
		fmt.Fprintf(&b, "Defined in `%s` (namespace `%s`)\n", file.Path(), origin)
	} else {
		fmt.Fprintf(&b, "Defined in `%s`\n", file.Path())
	}

	// Leading doc comment from the proto source, if the file descriptor
	// carries source-info metadata (set by protocompile and preserved
	// in the registry's stored descriptors).
	loc := file.SourceLocations().ByDescriptor(fd)
	if c := strings.TrimSpace(loc.LeadingComments); c != "" {
		fmt.Fprintf(&b, "\n%s\n", c)
	}
	// C7: surface pxf.default. The annotation is a PXF literal —
	// render it verbatim so users see exactly what the schema
	// would supply for an absent field.
	if defLit, has := pxf.Default(fd); has {
		fmt.Fprintf(&b, "\nDefault: `%s`\n", defLit)
	}
	return b.String()
}

// fieldTypeLabel returns a human-friendly type string for a field
// descriptor — the protobuf kind for scalars and the fully-qualified
// name for message and enum fields. Cardinality (repeated, map) is
// appended so the hover surfaces "repeated string" vs "string" at a
// glance.
func fieldTypeLabel(fd protoreflect.FieldDescriptor) string {
	var base string
	switch {
	case fd.Kind() == protoreflect.MessageKind:
		base = string(fd.Message().FullName())
	case fd.Kind() == protoreflect.EnumKind:
		base = string(fd.Enum().FullName())
	default:
		base = fd.Kind().String()
	}
	switch {
	case fd.IsMap():
		return fmt.Sprintf("map<%s, %s>", fd.MapKey().Kind(), fieldTypeLabel(fd.MapValue()))
	case fd.IsList():
		return "repeated " + base
	}
	return base
}
