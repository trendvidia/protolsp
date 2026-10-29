// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Runtime-violation overlay (#231, the transport half deferred from
// #220/#230). A protowire runtime reports constraint violations
// keyed by (file, descriptor_path) — RFC-001 §8.3.1. A producer
// (protocheck's runtime feed, an editor extension bridging a
// deployed service) pushes them in over the
// protolsp/setRuntimeViolations custom notification; this file owns
// the store and the rendering.
//
// Semantics:
//
//   - Full replace per document. Each notification overwrites the
//     document's entire violation set (the usual contract for a
//     runtime overlay — the producer owns the state and re-sends the
//     complete picture). An empty / absent list clears it.
//   - Violations describe a *runtime* state, not the buffer. The
//     store is workspace-scoped and never touched by the analyze
//     pipeline, so the set survives re-analysis, document close /
//     reopen, and transient mid-edit compile failures. Only the next
//     setRuntimeViolations notification changes it.
//   - Resolution happens at publish time: every publish for the
//     document re-resolves each stored violation through the
//     canonical protowire.schema.v1 source map (RFC001SourceRange,
//     #230). Violations whose descriptor_path no longer resolves —
//     the construct was edited away, or the file currently fails to
//     compile — are silently dropped from that publish, NOT evicted
//     from the store: an undo (or a fixed compile) makes the key
//     resolve again and the overlay reappears without the producer
//     re-sending.
//   - The join key is the violation's source_ref when present (the
//     §8.3.1 rule identity a v0.20.0+ engine emits), the positional
//     descriptorPath otherwise — see RuntimeViolation.
//   - Rendered as DiagnosticSeverity.Information with the dedicated
//     source string "protolsp/runtime" so editors can color / filter
//     the overlay independently of "protolsp/proto" and "buf/lint".
//     Only .proto documents carry source maps, so the overlay only
//     renders there; violations stored for other URIs stay dormant.

package analysis

import (
	"context"
	"path/filepath"
	"strings"

	"go.lsp.dev/protocol"
)

// MethodSetRuntimeViolations is the custom client→server
// notification replacing a document's runtime-violation overlay.
// Naming follows the state-push precedent (protolsp/setOverrides,
// pxfed/setBwSession): the client *sets* server-held state.
const MethodSetRuntimeViolations = "protolsp/setRuntimeViolations"

// RuntimeViolation is one runtime-reported constraint violation.
// The file half of the RFC-001 (file, descriptor_path) key is the
// notification's textDocument URI; DescriptorPath is the other half,
// verbatim as the runtime emitted it (e.g.
// "t.M[t.check#0]/arg#0/call#0").
//
// SourceRef and ForKey mirror the protowire.schema.v1
// EnrichedViolation fields of the same names (report.proto, vendored
// in protocompile v0.20.0), so a producer forwarding Report
// violations passes them through without re-keying:
//
//   - When SourceRef is set, its DescriptorPath is the join key and
//     the top-level DescriptorPath is ignored — source_ref is the
//     §8.3.1 rule identity, populated by engines whenever the rule
//     resolved through an embedded source map. Its File half guards
//     against mis-joins (see RuntimeSourceRef).
//   - ForKey marks a violation against a map entry's key rather than
//     its value. Key and value violations of the same entry-level
//     rule join to the same descriptor path and anchor, so the flag
//     is surfaced in the rendered message instead.
type RuntimeViolation struct {
	DescriptorPath string            `json:"descriptorPath,omitempty"`
	Message        string            `json:"message"`
	ForKey         bool              `json:"forKey,omitempty"`
	SourceRef      *RuntimeSourceRef `json:"sourceRef,omitempty"`
}

// RuntimeSourceRef mirrors protowire.schema.v1.SourceRef: the
// originating SourceMap.file plus the canonical §8.3.1 descriptor
// path. File is the artifact's module-relative path (as the compiler
// recorded it), while protolsp compiles open documents under their
// absolute filenames — so File matches when it equals the document's
// source-map file or is a path-boundary suffix of it. A File that
// matches neither drops the violation from the publish (a key for a
// different file must not resolve against this document's ordinals).
// Empty File imposes no constraint beyond the notification's
// textDocument scoping.
type RuntimeSourceRef struct {
	File           string `json:"file,omitempty"`
	DescriptorPath string `json:"descriptorPath"`
}

// SetRuntimeViolationsParams is the notification shape for
// [MethodSetRuntimeViolations]. Violations is the document's full
// new overlay set (replace, not append); empty or omitted clears.
type SetRuntimeViolationsParams struct {
	TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
	Violations   []RuntimeViolation              `json:"violations"`
}

// SetRuntimeViolations replaces the stored runtime-violation overlay
// for docURI and, when the document is open, republishes its
// diagnostics immediately so the editor reflects the new runtime
// state without waiting for the next edit. Also the embedder-facing
// programmatic entry point (the dispatch wrapper is protocol glue).
func (s *Workspace) SetRuntimeViolations(ctx context.Context, docURI protocol.DocumentURI, violations []RuntimeViolation) {
	s.runtimeViolationsMu.Lock()
	if len(violations) == 0 {
		delete(s.runtimeViolations, docURI)
	} else {
		if s.runtimeViolations == nil {
			s.runtimeViolations = make(map[protocol.DocumentURI][]RuntimeViolation)
		}
		// Defensive copy — the caller's slice may be reused (jsonrpc2
		// unmarshals into it once, but embedders can pass anything).
		s.runtimeViolations[docURI] = append([]RuntimeViolation(nil), violations...)
	}
	s.runtimeViolationsMu.Unlock()

	if doc := s.docs.Get(docURI); doc != nil {
		s.PublishDiagnostics(ctx, doc)
	}
}

// runtimeViolationDiagnosticsLocked renders the document's stored
// overlay for one publish: each violation's descriptor_path resolves
// through the current source map; hits become Information
// diagnostics with source "protolsp/runtime", misses are dropped
// from this publish (and only this publish — see the file comment
// for why the store keeps them). Caller holds doc.mu (read or
// write), same discipline as the rest of diagnosticsForLocked.
func (s *Workspace) runtimeViolationDiagnosticsLocked(doc *Document) []protocol.Diagnostic {
	s.runtimeViolationsMu.RLock()
	violations := s.runtimeViolations[doc.URI]
	s.runtimeViolationsMu.RUnlock()
	if len(violations) == 0 {
		return nil
	}
	var out []protocol.Diagnostic
	for _, v := range violations {
		key, ok := runtimeViolationKey(v, doc.rfc001SourceMap.GetFile())
		if !ok {
			continue
		}
		rng, ok := s.rfc001SourceRangeLocked(doc, key)
		if !ok {
			continue
		}
		msg := v.Message
		if v.ForKey {
			// Same rule, same anchor as the entry's value violations —
			// the flag is the only thing telling them apart.
			msg += " (map key)"
		}
		out = append(out, protocol.Diagnostic{
			Range:    rng,
			Severity: protocol.DiagnosticSeverityInformation,
			Source:   "protolsp/runtime",
			Message:  msg,
		})
	}
	return out
}

// runtimeViolationKey picks the descriptor-path join key for one
// violation: the source_ref path when present (the §8.3.1 rule
// identity), the positional DescriptorPath otherwise. ok=false when
// the source_ref names a file that isn't this document's source map
// — resolving such a key here would join another file's ordinals.
func runtimeViolationKey(v RuntimeViolation, smFile string) (string, bool) {
	if v.SourceRef == nil {
		return v.DescriptorPath, v.DescriptorPath != ""
	}
	if !sourceRefMatchesFile(v.SourceRef.File, smFile) {
		return "", false
	}
	return v.SourceRef.DescriptorPath, v.SourceRef.DescriptorPath != ""
}

// sourceRefMatchesFile reports whether a SourceRef.file names the
// document's source map. Producers carry the compiled artifact's
// module-relative path while protolsp compiles open documents under
// absolute filenames, so a path-boundary suffix match ("acme/u.proto"
// against "/work/proto/acme/u.proto") counts as the same file.
func sourceRefMatchesFile(refFile, smFile string) bool {
	if refFile == "" || refFile == smFile {
		return true
	}
	return strings.HasSuffix(filepath.ToSlash(smFile), "/"+refFile)
}
