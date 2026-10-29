// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// C3 of the chameleon-integration design: chain-aware required-
// field weighting. First user-visible behavior change on top of
// C1/C2's visibility-only slices.
//
// Standalone files keep today's per-file required check (it comes
// from pxf.UnmarshalDescriptor in validate(); that path is
// unchanged). Chain layer files take a different path:
//
//   - PER_ENV layer at env=X: walk env X's resolve order, read
//     each layer file, collect the union of top-level field names
//     set anywhere in the chain. Emit one Error per pxf.required
//     field NOT in that union. The error sits on the leaf file
//     (the one being analyzed), citing the chain context.
//
//   - ONCE chain layer (base.pxf and similar): suppress required
//     diagnostics. The check moves to the per-env leaves, which
//     have full visibility into the env's chain.
//
// Note on pxf's per-file required check: protowire-go's
// UnmarshalDescriptor (the path validate() uses) doesn't run
// post-decode validation today, so the standard SchemaErr never
// includes "missing required" errors. The chain-aware path here
// is the FIRST and ONLY required-field check protolsp emits.
// Standalone files have no required diagnostic — preserving
// today's behavior. The choice to add per-file required checks
// for standalone is out of scope for C3; the design only governs
// chain-aware behavior.
//
// Encrypted ancestors are opaque to the LSP (per the design's hard
// "no edit-time decryption" line). They're skipped on read; a
// field set only in an encrypted ancestor will trip a false
// "missing" diagnostic. Acceptable for C3 — refinement lives in
// the design's sops content-sniff note and is C3+ territory.

package analysis

import (
	"fmt"
	"path/filepath"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/safeio"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// chainRequiredDiagnostics computes the chain-aware required-field
// diagnostics for the document. Returns nil to signal "fall back to
// the standard per-file check"; returns an empty (non-nil) slice
// when chain-aware mode is on but no diagnostics fire.
//
// The dispatcher in diagnosticsFor() also checks
// doc.LayerInfo.IsLayer to decide whether to emit the SchemaErr
// diagnostic at all — chain layers run with SkipPostDecode=true and
// don't carry a SchemaErr for the required pass.
func (s *Workspace) chainRequiredDiagnostics(doc *Document) []protocol.Diagnostic {
	if s == nil || doc == nil || doc.Parsed == nil {
		return nil
	}
	if doc.LayerInfo == nil || !doc.LayerInfo.IsLayer {
		return nil // standalone — caller falls back to existing per-file pass.
	}
	// ONCE layer: suppress required-field diagnostics. The leaf-
	// file check at per-env layers covers them.
	if doc.LayerInfo.Strategy != "PER_ENV" {
		return []protocol.Diagnostic{}
	}
	docChain := s.ChainForDoc(doc)
	if docChain == nil || doc.LayerInfo.Environment == "" {
		return []protocol.Diagnostic{}
	}
	// C7 part 2: env-kind sources cover every field on the bound
	// message at runtime (the runtime reads each field's
	// UPPER_SNAKE env var if set). At edit time we can't observe
	// which env vars are set, but the conservative — and correct —
	// stance is to assume an env source MIGHT supply any required
	// field. Suppress diagnostics rather than false-positive on
	// fields the runtime will populate.
	if s.envSourceCoversEnvForDoc(doc, doc.LayerInfo.Environment) {
		return []protocol.Diagnostic{}
	}
	// Layer files commonly omit @type and rely on chameleon's
	// schema_fqn default. effectiveTypeURL honors that fallback so
	// the chain-aware required-field check actually runs.
	typeURL := s.effectiveTypeURL(doc)
	if typeURL == "" {
		return nil // no resolvable type — standard "unknown type" path handles it.
	}
	mt, err := s.findTypeForDoc(doc, protoreflect.FullName(typeURL))
	if err != nil || mt == nil {
		return nil // unresolved type — let the standard "unknown type" diagnostic fire.
	}
	desc := mt.Descriptor()
	requiredFields := collectRequiredFieldNames(desc)
	if len(requiredFields) == 0 {
		return []protocol.Diagnostic{}
	}
	setInChain, chainPaths := s.chainSetFieldUnionForDoc(doc, doc.LayerInfo.Environment)
	if setInChain == nil {
		// Chain expansion produced no layers (defensive — implies an
		// empty / broken chain spec). Skip rather than flood
		// diagnostics for every required field.
		return []protocol.Diagnostic{}
	}
	// Build one diagnostic per required field absent from the union.
	// Stable order: the descriptor's field iteration order.
	var out []protocol.Diagnostic
	for _, name := range requiredFields {
		if setInChain[name] {
			continue
		}
		out = append(out, protocol.Diagnostic{
			Range:    documentStartRange(),
			Severity: protocol.DiagnosticSeverityError,
			Source:   "protolsp/schema",
			Message: fmt.Sprintf(
				"required field %q is not set in any layer of the %q chain (%s)",
				name, doc.LayerInfo.Environment, strings.Join(chainPaths, ", ")),
		})
	}
	return out
}

// chainSetFieldUnionForDoc reads every readable layer file in
// the env's resolve order, parses each, and returns:
//
//   - the union of top-level field names assigned anywhere in those
//     files (the "set" set, used to gate the required-field
//     diagnostic)
//   - the human-readable list of relative chain paths for the
//     diagnostic message
//
// Layer file paths resolve against the doc's WORKSPACE dir (the
// nearest-ancestor `.protolsp.pxf` directory per W4), not the
// LSP-reported root — sub-workspaces' chain paths are relative to
// their own dir.
//
// Encrypted layers and unreadable files are SKIPPED on the set-
// gathering side (we can't observe their contents) but still appear
// in the diagnostic message — the user benefits from knowing those
// files exist in the chain even if we can't validate against them.
//
// Returns (nil, nil) when the chain has no layers for this env.
func (s *Workspace) chainSetFieldUnionForDoc(doc *Document, env string) (map[string]bool, []string) {
	docChain := s.ChainForDoc(doc)
	if docChain == nil {
		return nil, nil
	}
	order := docChain.ResolveOrder(env)
	if len(order) == 0 {
		return nil, nil
	}
	rootPath := s.workspaceDirForDoc(doc)
	if rootPath == "" {
		return nil, nil
	}
	set := make(map[string]bool)
	paths := make([]string, 0, len(order))
	for _, layer := range order {
		paths = append(paths, layer.ResolvedPath)
		if layer.Encrypted {
			continue // sops-encrypted; opaque to the LSP.
		}
		absPath := layer.ResolvedPath
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(rootPath, filepath.FromSlash(layer.ResolvedPath))
		}
		// Prefer the open-document source over disk: an edit not yet
		// saved should still influence the check.
		var content []byte
		if doc := s.docs.Get(uri.File(absPath)); doc != nil {
			content = doc.Source
		} else {
			b, err := safeio.ReadFileLimit(absPath, safeio.MaxConfigFileBytes)
			if err != nil {
				continue // missing or oversized layer → skip (chameleon also tolerates absent ancestors)
			}
			content = b
		}
		for name := range collectTopLevelFieldNames(content) {
			set[name] = true
		}
	}
	return set, paths
}

// envSourceCoversEnvForDoc reports whether the env's resolve
// order in DOC's workspace chain includes at least one layer
// backed by a `kind = "env"` source. Used by the required-field
// diagnostic to suppress errors when the runtime would populate
// fields from environment variables.
//
// The "covers" semantics are deliberately broad: any env source
// in chain → no required errors anywhere on that env's leaf.
// We can't know at edit time which specific env vars are set, so
// matching the runtime's "env source layer covers all fields"
// behavior is the safest stance — false negatives (missing a
// required field that the env var isn't actually set for) are a
// runtime concern, not an edit-time one.
func (s *Workspace) envSourceCoversEnvForDoc(doc *Document, env string) bool {
	if s == nil {
		return false
	}
	docChain := s.ChainForDoc(doc)
	if docChain == nil {
		return false
	}
	for _, layer := range docChain.ResolveOrder(env) {
		if docChain.LayerIsEnvSourced(layer) {
			return true
		}
	}
	return false
}

// collectRequiredFieldNames returns the names of every field on the
// message descriptor marked `(pxf.required) = true`, in descriptor
// declaration order. Nested message fields are not recursed into —
// C3 scope is top-level required-field validation; deeper nesting
// is future work alongside chameleon's runtime semantics for the
// same.
func collectRequiredFieldNames(desc protoreflect.MessageDescriptor) []string {
	if desc == nil {
		return nil
	}
	fields := desc.Fields()
	var out []string
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if pxf.IsRequired(fd) {
			out = append(out, string(fd.Name()))
		}
	}
	return out
}

// collectTopLevelFieldNames returns the set of top-level field names
// assigned in a PXF document, via a best-effort parse. Used for the
// chain-aware "field set somewhere in the chain" check. Unparseable
// content yields an empty set rather than an error — a mid-edit
// layer file shouldn't tank required-validation on its sibling
// leaves.
//
// "Assignment" here includes both `name = value` and `block {...}`
// at the document root — chameleon's merge treats either as having
// set the named field.
func collectTopLevelFieldNames(content []byte) map[string]bool {
	doc, err := pxf.Parse(content)
	if err != nil || doc == nil {
		return nil
	}
	out := make(map[string]bool)
	for _, e := range doc.Entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			out[n.Key] = true
		case *pxf.Block:
			out[n.Name] = true
		}
	}
	return out
}

// documentStartRange returns a zero-width LSP range at the start of
// the document. C3's required-field diagnostics anchor here per the
// design — the @type directive (when present) is on line 0, and a
// document-start anchor works equally well for files that omit it.
// Future refinement: pxf could expose the @type directive's
// position, but for now line 0 is the lowest-risk choice.
func documentStartRange() protocol.Range {
	return protocol.Range{
		Start: protocol.Position{Line: 0, Character: 0},
		End:   protocol.Position{Line: 0, Character: 0},
	}
}
