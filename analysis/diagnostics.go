// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"errors"
	"fmt"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/trendvidia/protowire-go/encoding/pxf"

	"github.com/trendvidia/protolsp/redact"
)

// Data payload kinds embedded in Diagnostic.Data so the code-action
// handler can recognize the diagnostic it's offering a fix for.
const (
	// DiagDataKindUnknownField marks a diagnostic produced by the
	// AST walk for a field assignment whose name doesn't exist on
	// the bound message. Payload also carries the field name and the
	// 0-indexed source line, used by the "Remove field" quick fix.
	DiagDataKindUnknownField = "unknown_field"
)

// UnknownFieldData is the JSON shape of Diagnostic.Data for unknown-
// field diagnostics. The kind discriminator lets the code-action
// handler ignore unrelated diagnostics it might receive in the same
// request.
type UnknownFieldData struct {
	Kind      string `json:"kind"`
	FieldName string `json:"field_name"`
	Line      uint32 `json:"line"`
}

// Analyze runs the full per-change pipeline against the document's
// current source: parse → (if @type bound and a lookup is configured)
// schema validation. ParseErr and SchemaErr are reset on each pass.
//
// LastGoodParse is *not* reset on parse failure: completion and other
// AST-aware features fall back to it through transient mid-edit
// failures (typing `field = ` produces a single broken line, but the
// structure of the rest of the document hasn't changed).
func (s *Workspace) Analyze(doc *Document) {
	doc.mu.Lock()
	defer doc.mu.Unlock()

	// Non-file URIs are virtual / read-only views protolsp serves
	// itself (registry:// schema previews under the proto-files
	// resolver, untitled://, etc.). They have no editor-side state
	// to analyze; the resolver-side path populated their content
	// with canonical bytes that don't change post-open. Both the
	// proto and pxf pipelines below assume file-system filenames
	// (URI.FsPath() panics on non-file schemes; pxf.Parse on a
	// non-pxf source produces noisy fake diagnostics), so short-
	// circuit here.
	if !IsFileURI(doc.URI) {
		return
	}

	// Phase 1 of proto authoring: .proto docs go through their own
	// pipeline (single-file protocompile compile → diagnostics).
	// None of the pxf-specific state below applies — Parsed,
	// SchemaErr, UnknownFields, deprecatedUses, SecretRanges,
	// DatasetRegions, LayerInfo all stay zero/empty for proto docs
	// and diagnosticsFor short-circuits on IsProtoURI to return
	// the dedicated ProtoDiagnostics slice.
	if IsProtoURI(doc.URI) {
		s.analyzeProto(doc)
		return
	}

	doc.Parsed = nil
	doc.ParseErr = nil
	doc.SchemaErr = nil
	doc.keyedFindings = nil

	// Dataset-region scan (R1). Source-based; doesn't depend on
	// the parser succeeding. Runs unconditionally so editors get
	// region info even when the document has no @type binding
	// (or the parser bailed mid-edit). Threshold for the "huge"
	// flag is server-configurable; 0 means use the default.
	doc.DatasetRegions = DetectDatasetRegions(doc.Source, s.hugeRegionBytes, s.positionEncoding)

	parsed, err := pxf.Parse(doc.Source)
	if err != nil {
		doc.ParseErr = err
		return
	}
	doc.Parsed = parsed
	doc.LastGoodParse = parsed

	// The document's own @proto types, before anything resolves a
	// type: the embedded schema is authoritative.
	loadInlineSchema(doc)

	// R2 enrichment: the line-based scanner already ran above
	// against the raw bytes; now that parse succeeded we can fill
	// in RowCount + Resolvable from pxf.Document.Datasets, resolving
	// each row type as findTypeForDoc does.
	doc.DatasetRegions = enrichDatasetRegionsTyped(
		doc.DatasetRegions, parsed, docTypeLookup{s, doc},
		s.hugeRegionRows, s.hugeRegionBytes, doc.datasetType,
	)

	// Secret-range walk (S1). Builds the SecretRange table + the
	// private secretValues mirror; both reset per analyze pass. The
	// @dataset rows are walked here, not in validate: a dataset
	// document has no @type, so validate returns before its body walk.
	doc.SecretRanges = nil
	doc.secretValues = nil
	s.collectDatasetSecretRanges(doc)

	s.validate(doc)
}

// validate runs schema-aware checks against a parsed document. No-ops
// when the source didn't parse, when the document has no @type
// directive, or when no Lookup is configured.
//
// Three classes of issue are detected:
//   - Per-field unknown names (AST walk; emits one diagnostic per
//     offending assignment so the code-action handler can offer a
//     "Remove field" fix per occurrence).
//   - Per-field deprecation usage (AST walk; emits Info-severity
//     diagnostics with `Tags: [Deprecated]` so editors render the
//     line with a strikethrough).
//   - Anything else pxf.UnmarshalDescriptor catches — type mismatches,
//     missing-required, oneof violations. UnmarshalDescriptor stops
//     at the first such error, so SchemaErr captures at most one.
//     DiscardUnknown is set so unknown-field errors from the decoder
//     don't duplicate the AST walk's diagnostics.
//
// Caller MUST hold doc.mu.Lock — validate is only called from
// analyze, which acquires the write lock for the full pipeline.
func (s *Workspace) validate(doc *Document) {
	if doc.Parsed == nil {
		return
	}
	// effectiveTypeURL bridges the chameleon.schema_fqn fallback —
	// without it, layer files with no @type bailed out here and
	// never got validated (no unknown-field / deprecation /
	// type-mismatch diagnostics, despite chameleon supplying the
	// type via schema_fqn).
	typeURL := s.effectiveTypeURL(doc)
	if typeURL == "" {
		return
	}
	fqn := protoreflect.FullName(typeURL)
	mt, err := s.findTypeForDoc(doc, fqn)
	if err != nil || mt == nil {
		// Type not in any lookup. Surface "unknown type" only when the
		// user opted into schema validation by configuring a registry —
		// otherwise stay silent (no registry → no validation expected).
		// The internal-only fallback successfully resolving (e.g. for
		// .protolsp.pxf) takes the `mt != nil` branch below regardless
		// of whether the user has a registry.
		if s.hasUserLookup() {
			// Where did the FQN come from? @type directive vs the
			// workspace's chameleon.schema_fqn default. The message
			// points the user at the right knob to fix.
			source := "@type directive"
			if doc.Parsed.TypeURL == "" {
				source = "chameleon.schema_fqn workspace setting"
			}
			doc.SchemaErr = fmt.Errorf("unknown type %q in %s: %w", typeURL, source, err)
		}
		return
	}
	md := mt.Descriptor()

	// Keyed-repeated-field diagnostics (draft -01 §3.13): duplicate
	// entry names, empty keys, key-field/entry-name conflicts, and
	// quoted entry names outside a keyed field. Computed against the
	// bound schema; rendered by diagnosticsFor. The strict decoder
	// below surfaces the first of these as SchemaErr too, so we drop
	// keyed errors from SchemaErr to avoid double-reporting.
	doc.keyedFindings = pxf.KeyedDiagnostics(doc.Parsed, md)

	// AST walk for per-field findings.
	doc.UnknownFields = nil
	doc.deprecatedUses = nil
	walkFieldAssignments(doc.Parsed.Entries, md, func(key string, pos pxf.Position, fd protoreflect.FieldDescriptor) {
		if fd == nil {
			doc.UnknownFields = append(doc.UnknownFields, fieldFinding{Name: key, Pos: pos})
			return
		}
		if isDeprecated(fd) {
			doc.deprecatedUses = append(doc.deprecatedUses, fieldFinding{Name: key, Pos: pos})
		}
	})

	// Secret-range walk (S1) over the body; analyze reset the table
	// and walked the @dataset rows already.
	collectSecretRanges(doc.Parsed.Entries, md, "", doc)

	// Type/oneof validation. DiscardUnknown so unknown-field errors
	// from the decoder don't double-up with our AST findings.
	// OnSecretField drops pxf.Secret values rather than letting them
	// populate Secret.value on the proto message — closes the
	// plaintext-in-heap window for the proto layer per the
	// secret-handling design (S1).
	//
	// Note: UnmarshalDescriptor in protowire-go skips the post-decode
	// pass that validates pxf.required + applies pxf.default; missing-
	// required is not part of SchemaErr today. The C3 chain-aware
	// required-field diagnostics in chainRequiredDiagnostics are the
	// only required-field check protolsp emits, and they fire only
	// for chain layers.
	msg, err := pxf.UnmarshalOptions{
		DiscardUnknown: true,
		OnSecretField:  func(_ /* path */, _ /* value */ string) error { return nil },
	}.UnmarshalDescriptor(doc.Source, md)
	if err != nil {
		// Keyed-repeated-field violations are reported comprehensively
		// (in source order, all of them) via doc.keyedFindings; the
		// strict decoder stops at the first one and would surface it as
		// a second, worse-anchored diagnostic. Drop it from SchemaErr.
		var keyedErr *pxf.KeyedError
		if !errors.As(err, &keyedErr) {
			doc.SchemaErr = err
		}
	}

	// Phase 1 of the protocheck integration — runtime validation
	// of .pxf data against the schema's declared constraints. Runs
	// only when the unmarshal produced a usable message (no err
	// and no nil msg); a SchemaErr already tells the user the
	// document is structurally invalid, and adding constraint
	// findings on top would be noise. runProtocheck gates itself:
	// nil findings when no validator is installed (degraded mode,
	// logged at construction) or in the OSS build (stub).
	doc.protocheckFindings = nil
	doc.protovalidateFindings = nil
	if err == nil && msg != nil {
		doc.protocheckFindings = s.runProtocheck(doc, msg)
		if s.protovalidate != nil {
			doc.protovalidateFindings = s.runProtovalidate(doc, msg)
		}
	}
}

// fieldFinding records one location where a field assignment in the
// pfx document triggered an AST-walk finding (unknown name, deprecated
// usage). Pos is the start position of the assignment.
type fieldFinding struct {
	Name string
	Pos  pxf.Position
}

// walkFieldAssignments traverses the entries against the given message
// descriptor, descending into Blocks whose name maps to a
// message-typed field. For each Assignment / Block / MapEntry, invokes
// fn with the key, source position, and the resolved
// FieldDescriptor — or nil when the key doesn't match any field on
// the enclosing message.
func walkFieldAssignments(
	entries []pxf.Entry,
	md protoreflect.MessageDescriptor,
	fn func(key string, pos pxf.Position, fd protoreflect.FieldDescriptor),
) {
	if md == nil {
		return
	}
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			if n.KeyQuoted {
				// A quoted key never names a field; it's valid only as a
				// keyed entry name, which the keyed diagnostics own.
				continue
			}
			fd := md.Fields().ByName(protoreflect.Name(n.Key))
			fn(n.Key, n.Pos, fd)
		case *pxf.Block:
			if n.NameQuoted {
				continue
			}
			fd := md.Fields().ByName(protoreflect.Name(n.Name))
			fn(n.Name, n.Pos, fd)
			if pxf.KeyField(fd) != nil {
				// Keyed repeated field: the block's children are entries
				// (keyed by name), not fields of the parent. Their names
				// are keys — don't report them as unknown fields — and
				// their bodies bind to the element message.
				elem := fd.Message()
				for _, ce := range n.Entries {
					if ke, ok := asKeyedEntry(ce); ok {
						walkFieldAssignments(ke.body, elem, fn)
					}
				}
				continue
			}
			if fd != nil && fd.Message() != nil {
				walkFieldAssignments(n.Entries, fd.Message(), fn)
			}
		case *pxf.MapEntry:
			fd := md.Fields().ByName(protoreflect.Name(n.Key))
			fn(n.Key, n.Pos, fd)
		}
	}
}

// isDeprecated returns true when the field is marked with
// `[deprecated = true]` in its .proto declaration.
func isDeprecated(fd protoreflect.FieldDescriptor) bool {
	opts, ok := fd.Options().(*descriptorpb.FieldOptions)
	if !ok || opts == nil {
		return false
	}
	return opts.GetDeprecated()
}

// PublishDiagnostics computes the diagnostic list for the document's
// current parse result and sends a textDocument/publishDiagnostics
// notification. Diagnostic messages are run through the secret
// redactor (S1 of the secret-handling design) so pxf.Secret values
// that triggered a finding don't echo back to the client.
func (s *Workspace) PublishDiagnostics(ctx context.Context, doc *Document) {
	// Snapshot everything we need under a single RLock — recursive
	// RLock is unsafe (a waiting Lock would deadlock both), so
	// avoid calling SecretValues() inside this window. The
	// secretValuesLocked helper returns the same defensive copy
	// without re-acquiring the mutex.
	doc.mu.RLock()
	diags := s.diagnosticsForLocked(doc)
	version := uint32(doc.Version) // #nosec G115 -- doc.Version is non-negative per LSP
	secrets := doc.secretValuesLocked()
	doc.mu.RUnlock()
	if len(secrets) > 0 {
		for i := range diags {
			redact.Diagnostic(&diags[i], secrets)
		}
	}
	s.PublishDiagnosticsRaw(ctx, doc.URI, version, diags)
}

func (s *Workspace) PublishDiagnosticsRaw(ctx context.Context, u uri.URI, version uint32, diags []protocol.Diagnostic) {
	s.pub.PublishDiagnostics(ctx, u, version, diags)
}

// DiagnosticsFor maps a document's analysis state to LSP diagnostics.
// A parse failure suppresses schema diagnostics (the source didn't
// reach the validator). When parse succeeds, the list contains:
//   - one diagnostic per unknown field (Error severity, Data payload
//     so the code-action handler can offer a removal quick-fix)
//   - one diagnostic per deprecated field use (Hint severity, Tags:
//     [Deprecated] so editors render strikethrough)
//   - one diagnostic for SchemaErr if non-nil (type mismatch /
//     missing-required / oneof violation from pxf.UnmarshalDescriptor)
//
// The result is NOT secret-scrubbed: unlike the PublishDiagnostics
// path (which redacts before the Publisher sees anything), a caller
// pulling DiagnosticsFor directly and displaying it is responsible
// for redaction.
//
// Takes the document read lock internally, like the other feature
// queries. Callers must NOT already hold doc.RLock —
// recursive RLock deadlocks against a queued writer (see
// PublishDiagnostics, which snapshots under one RLock window via
// diagnosticsForLocked instead).
func (s *Workspace) DiagnosticsFor(doc *Document) []protocol.Diagnostic {
	doc.mu.RLock()
	defer doc.mu.RUnlock()
	return s.diagnosticsForLocked(doc)
}

// diagnosticsForLocked is DiagnosticsFor's body. The caller must
// hold doc.mu (read or write); none of the per-source helpers below
// re-acquire it.
func (s *Workspace) diagnosticsForLocked(doc *Document) []protocol.Diagnostic {
	// Phase 1 proto authoring: .proto docs publish their own
	// diagnostic set (populated by analyzeProto). None of the
	// pxf-specific findings below apply — ParseErr, UnknownFields,
	// deprecatedUses, SchemaErr, literal-secret etc. are all
	// .pxf concepts. Returning the snapshot directly keeps proto
	// diagnostics isolated from the pxf aggregation logic. The
	// runtime-violation overlay concatenates after the
	// analyze snapshot — resolved fresh against the current source
	// map on every publish, into a new slice so concurrent readers
	// never append into ProtoDiagnostics' backing array.
	if IsProtoURI(doc.URI) {
		rt := s.runtimeViolationDiagnosticsLocked(doc)
		if len(rt) == 0 {
			return doc.ProtoDiagnostics
		}
		out := make([]protocol.Diagnostic, 0, len(doc.ProtoDiagnostics)+len(rt))
		out = append(out, doc.ProtoDiagnostics...)
		return append(out, rt...)
	}
	if doc.ParseErr != nil {
		return []protocol.Diagnostic{diagnosticFromError(doc.ParseErr, "protolsp/parse")}
	}
	var out []protocol.Diagnostic
	for _, uf := range doc.UnknownFields {
		out = append(out, protocol.Diagnostic{
			Range:    pxfPosToRange(uf.Pos),
			Severity: protocol.DiagnosticSeverityError,
			Source:   optString("protolsp/schema"),
			Message:  protocol.String(fmt.Sprintf("unknown field %q on this message", uf.Name)),
			Data: mustAny(UnknownFieldData{
				Kind:      DiagDataKindUnknownField,
				FieldName: uf.Name,
				Line:      lspLineFromPxf(uf.Pos),
			}),
		})
	}
	for _, du := range doc.deprecatedUses {
		out = append(out, protocol.Diagnostic{
			Range:    pxfPosToRange(du.Pos),
			Severity: protocol.DiagnosticSeverityHint,
			Source:   optString("protolsp/schema"),
			Message:  protocol.String(fmt.Sprintf("field %q is deprecated", du.Name)),
			Tags:     protocol.NewDiagnosticTags(protocol.DiagnosticTagDeprecated),
		})
	}
	if doc.SchemaErr != nil {
		out = append(out, diagnosticFromError(doc.SchemaErr, "protolsp/schema"))
	}
	out = append(out, doc.inline.diags...)
	// Keyed-repeated-field violations (draft -01 §3.13). Distinct
	// source "protolsp/keyed" so editors can group / filter them
	// apart from ordinary schema-shape findings.
	out = append(out, s.keyedDiagnostics(doc)...)
	// Phase 1 protocheck data validation — emits one diagnostic per
	// constraint violation, source "protocheck". Distinct source
	// string so editors can color / filter independently from
	// "protolsp/schema" (structural errors) and
	// "protocheck/expression" (broken constraint exprs from Phase 0).
	out = append(out, protocheckDataDiagnostics(doc)...)
	// Phase 1 protovalidate side, source "buf/validate". Coexists
	// cleanly with protocheck because the two engines look at
	// different extension annotations.
	out = append(out, protovalidateDataDiagnostics(doc)...)
	// S3: literal-secret warnings. Source is "protolsp/security" so
	// editors can color them distinctly from schema findings.
	out = append(out, s.literalSecretDiagnostics(doc)...)
	// R4: per-cell type-mismatch diagnostics for @dataset rows.
	// Source is "protolsp/dataset" — distinct from "protolsp/schema"
	// so editors can group / color dataset findings separately.
	out = append(out, s.validateDatasetRegions(doc)...)
	// C3: chain-aware required-field diagnostics. For chain layers
	// these replace pxf's per-file required check (we ran the
	// decoder with SkipPostDecode=true above). Standalone files get
	// nil here and keep today's behavior.
	out = append(out, s.chainRequiredDiagnostics(doc)...)
	// C4: ${field.path} interpolation unresolvable-reference
	// diagnostics. Runs regardless of chain mode — any document
	// bound to a schema can carry interpolations the schema
	// doesn't know about.
	out = append(out, s.fieldReferenceDiagnostics(doc)...)
	// C4b: ${field.path} reference-cycle diagnostics. Top-level
	// scope only; nested-block cycles are documented as a known gap.
	out = append(out, s.cycleReferenceDiagnostics(doc)...)
	// C7: pxf.default redundancy hints. Hint severity (the
	// stylistic-feedback level) so users can address them at
	// their own pace; not a correctness issue.
	out = append(out, s.defaultRedundancyDiagnostics(doc)...)
	// Dedicated-parser warnings for protolsp's own config files
	// (.protolsp.pxf, chain.pxf). Surfaces semantic issues the
	// generic schema validation doesn't catch — bad inline_secrets
	// values, ancestry cycles, missing @type on the spec, etc.
	out = append(out, s.ConfigDiagnostics(doc)...)
	return out
}

// lspLineFromPxf converts a 1-indexed pxf line to a 0-indexed LSP
// line, clamping zero/negative inputs to the document origin.
func lspLineFromPxf(p pxf.Position) uint32 {
	if p.Line <= 1 {
		return 0
	}
	return uint32(p.Line - 1) // #nosec G115 -- bounded above by parser
}

// diagnosticFromError builds an LSP Diagnostic from a pxf.Error (which
// carries a Position) or any other error (which lands at the document
// origin as a fallback). Used for both parse errors and schema
// validation errors — pxf.UnmarshalDescriptor surfaces positions on
// the same Error type the parser uses.
func diagnosticFromError(err error, source string) protocol.Diagnostic {
	severity := protocol.DiagnosticSeverityError
	var pxfErr *pxf.Error
	if errors.As(err, &pxfErr) {
		return protocol.Diagnostic{
			Range:    pxfPosToRange(pxfErr.Pos),
			Severity: severity,
			Source:   optString(source),
			Message:  protocol.String(pxfErr.Msg),
		}
	}
	return protocol.Diagnostic{
		Range: protocol.Range{
			Start: protocol.Position{Line: 0, Character: 0},
			End:   protocol.Position{Line: 0, Character: 1},
		},
		Severity: severity,
		Source:   optString(source),
		Message:  protocol.String(err.Error()),
	}
}
