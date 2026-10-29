// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// LSP dispatch wrappers: one thin function per feature method that
// unmarshals jsonrpc2 params, calls into the engine, and replies.
// Relocated here from the per-feature files so those files hold
// pure engine logic and can graduate to the public analysis
// package. Behavior-preserving moves only.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/trendvidia/protolsp/analysis"
	"github.com/trendvidia/protolsp/redact"
	"github.com/trendvidia/protolsp/workspaceconfig"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// --- from hover.go ---

// dispatchHover handles textDocument/hover. Looks up the parsed AST for
// the document, resolves the field under the cursor against the bound
// schema, and renders a markdown summary (field name + type + defining
// file + leading doc comment, when available).
//
// M4 uses a line-match heuristic for "what's under the cursor": the
// pxf AST records only start positions for entries, so we pick the
// entry whose Pos.Line equals the cursor's line. Multi-entry lines
// resolve to the first match — acceptable for MVP, refinable later
// when the parser surfaces token end positions.
func (s *Server) dispatchHover(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.HoverParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling HoverParams: "+err.Error()))
	}
	hover := s.HoverFor(params.TextDocument.URI, params.Position)
	// Pass a literal nil interface for "no hover" — passing the typed
	// nil pointer wraps it in a non-nil interface{}, which JSON-marshals
	// to a zero Hover object rather than null. Clients reject the
	// difference (the LSP spec wants null/omitted for "nothing here").
	if hover == nil {
		return reply(ctx, nil, nil)
	}
	return reply(ctx, hover, nil)
}

// --- from completion.go ---

// dispatchCompletion handles textDocument/completion. Routes by
// document URI: `.proto` files go to ProtoCompletionFor (phase 4 of
// the proto-authoring plan — keywords + field types), every other
// URI goes to the pxf CompletionFor (field-name + enum-value +
// @type + ${field.path} completion). LSP clients filter the
// returned candidates against the user's in-progress identifier;
// the server returns the full set for the scope.
func (s *Server) dispatchCompletion(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.CompletionParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling CompletionParams: "+err.Error()))
	}
	var list *protocol.CompletionList
	if IsProtoURI(uri.URI(params.TextDocument.URI)) {
		list = s.ProtoCompletionFor(params.TextDocument.URI, params.Position)
	} else {
		list = s.CompletionFor(params.TextDocument.URI, params.Position)
	}
	if list == nil {
		// LSP allows null for "no completions"; some clients prefer an
		// empty list with isIncomplete:false. Either is spec-compliant.
		// We return null via the literal-nil interface to dodge the
		// typed-nil-pointer-in-interface gotcha that bit M4.
		return reply(ctx, nil, nil)
	}
	return reply(ctx, list, nil)
}

// --- from definition.go ---

// dispatchDefinition handles textDocument/definition. Resolves the
// cursor's target — a field assignment, a block name, or the FQN in
// an `@type` directive — to the descriptor that defines it, then
// returns a Location pointing at the descriptor's `.proto` source
// file with line/column from SourceLocations().
//
// The `.proto` file path on a FileDescriptor is relative (e.g.
// "acme/demo/v1/greeting.proto"). We resolve it against the
// workspace root reported in initialize. When no workspace root
// was provided, or when the resolved file doesn't exist on disk,
// the editor reports "file not found" — a follow-up could fetch
// source bytes from the registry and serve them via a custom URI
// scheme.
func (s *Server) dispatchDefinition(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.DefinitionParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling DefinitionParams: "+err.Error()))
	}
	loc := s.DefinitionFor(params.TextDocument.URI, params.Position)
	if loc == nil {
		return reply(ctx, nil, nil)
	}
	return reply(ctx, []protocol.Location{*loc}, nil)
}

// --- from codeaction.go ---

// dispatchCodeAction handles textDocument/codeAction. Looks at the
// diagnostics carried in CodeActionContext, switches on each one's
// Data payload, and emits matching quick-fix actions. Today the only
// kind is "unknown_field" → "Remove field 'X'", which deletes the
// entire line.
func (s *Server) dispatchCodeAction(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.CodeActionParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling CodeActionParams: "+err.Error()))
	}
	doc := s.Documents().Get(params.TextDocument.URI)
	if doc == nil {
		return reply(ctx, nil, nil)
	}
	doc.RLock()
	actions := s.CodeActionsFor(doc, params.Context.Diagnostics)
	doc.RUnlock()
	if len(actions) == 0 {
		return reply(ctx, nil, nil)
	}
	// LSP expects []CodeAction (or []Command); marshal as the slice.
	return reply(ctx, actions, nil)
}

// --- from document_symbol.go ---

// dispatchDocumentSymbol handles textDocument/documentSymbol. Returns
// a hierarchical DocumentSymbol tree built from the cached parse
// result — no work happens on the request path beyond an AST walk.
//
// Clients that advertise hierarchicalDocumentSymbolSupport render the
// children directly in the outline; clients that don't fall back to a
// flat view of the top-level entries (LSP allows servers to return
// DocumentSymbol[] regardless and the client picks what it can use).
//
// Returns nil (LSP "no symbols") when the document hasn't been opened
// or hasn't parsed yet. The LastGoodParse fallback that other handlers
// use to stay live through mid-edit parse failures doesn't apply here
// — the outline showing slightly stale structure is fine, and so is an
// empty outline; both are better than a wrong one taken from a parse
// that never matched the user's edits.
func (s *Server) dispatchDocumentSymbol(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.DocumentSymbolParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling DocumentSymbolParams: "+err.Error()))
	}
	symbols := s.DocumentSymbolsFor(params.TextDocument.URI)
	if symbols == nil {
		return reply(ctx, nil, nil)
	}
	return reply(ctx, symbols, nil)
}

// --- from folding_range.go ---

// dispatchFoldingRange handles textDocument/foldingRange. Routes by
// document URI: `.proto` schemas walk the protocompile AST (message /
// enum / service / oneof / rpc / extend bodies, the import group, and
// comment blocks); every other URI walks the pxf AST (multi-line blocks
// and directives). Pure structure — no dataset enrichment, which stays
// in the specialized protolsp/datasetRegionsChanged push.
//
// Returns nil (LSP "no ranges") when the document hasn't been opened or
// hasn't parsed. Like documentSymbol, there's no LastGoodParse fallback:
// stale-but-empty folds beat folds taken from a parse that never matched
// the user's edits.
func (s *Server) dispatchFoldingRange(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.FoldingRangeParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling FoldingRangeParams: "+err.Error()))
	}
	var ranges []protocol.FoldingRange
	if IsProtoURI(params.TextDocument.URI) {
		ranges = s.ProtoFoldingRangesFor(params.TextDocument.URI)
	} else {
		ranges = s.FoldingRangesFor(params.TextDocument.URI)
	}
	if ranges == nil {
		return reply(ctx, nil, nil)
	}
	return reply(ctx, ranges, nil)
}

// --- from semantic_tokens.go ---

// dispatchSemanticTokensFull handles textDocument/semanticTokens/full.
// Walks the parsed AST and emits one token per schema-meaningful
// element. Returns a non-nil SemanticTokens (possibly with empty
// Data) when the document is open; nil for unopened docs.
//
// LSP convention: clients merge semantic-token output ON TOP of
// their own syntactic highlighting (tmLanguage / Monarch). We
// intentionally don't emit punctuation / whitespace tokens — that's
// the editor's syntactic layer to handle.
func (s *Server) dispatchSemanticTokensFull(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.SemanticTokensParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling SemanticTokensParams: "+err.Error()))
	}
	tokens := s.SemanticTokensFor(params.TextDocument.URI)
	if tokens == nil {
		return reply(ctx, nil, nil)
	}
	return reply(ctx, tokens, nil)
}

// --- from formatting.go ---

// dispatchFormatting handles textDocument/formatting. Delegates the
// actual layout work to protowire-go's pxf.FormatDocument so the LSP
// formatter and any CLI / library consumer produce identical output.
// Returns an empty edit slice when the document already matches the
// canonical form — clients use that to avoid dirtying the buffer on
// every save.
//
// We don't honor client-supplied FormattingOptions (tab size,
// insertSpaces). The PXF format is canonical, gofmt-style: 2-space
// indent, fixed value-side layout. Treating editor preferences as
// advisory keeps tooling consistent across IDE / CLI / CI.
//
// Parse-broken documents return nil (LSP's "I have no edits") rather
// than an error — the user is mid-edit, and clobbering their buffer
// with a rewrite from the last good parse would lose work.
func (s *Server) dispatchFormatting(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.DocumentFormattingParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling DocumentFormattingParams: "+err.Error()))
	}
	docURI := params.TextDocument.URI
	var edits []protocol.TextEdit
	if IsProtoURI(docURI) {
		edits = s.ProtoFormattingEditsFor(ctx, docURI)
	} else {
		edits = s.FormattingEditsFor(docURI)
	}
	if edits == nil {
		return reply(ctx, nil, nil)
	}
	return reply(ctx, edits, nil)
}

// --- from references.go ---

// dispatchReferences handles textDocument/references. Returns every
// Assignment / Block whose dotted path matches the path under the
// cursor across the workspace's .pxf files on disk plus any open
// documents inside the same workspace.
//
// Scope choice: cross-file inside one workspace fits the chameleon
// layering use case (a value set in `base.pxf`, overridden in
// `dev.pxf` / `prod.pxf`) — clicking on a field in one layer should
// surface every layer that touches it. Cross-workspace references
// would conflate unrelated tenants in a multi-root setup; the
// WorkspaceDir scope prevents that.
//
// Filesystem walk vs. open-document-only: clients aren't reliably
// going to open every layer file before the user asks "where is
// this referenced?" — they'd see a partial list and assume the
// answer is complete. The walk reads .pxf files on disk to fill
// the gap, with the open-document set overlaying so unsaved edits
// still take precedence. Sub-workspaces (nested `.protolsp.pxf`
// ancestors) and hidden directories are skipped.
//
// Bounded parallelism: a worker pool sized to runtime.NumCPU()
// processes file reads + parses concurrently — a single click on
// a 1000-file workspace runs in ~1/NumCPU of the serial time.
// Workspaces past maxReferencesWorkspaceFiles trigger a truncation
// warning so the user can refine scope rather than wait silently.
func (s *Server) dispatchReferences(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.ReferenceParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling ReferenceParams: "+err.Error()))
	}
	locs := s.ReferencesFor(ctx, params.TextDocument.URI, params.Position, params.Context.IncludeDeclaration)
	if locs == nil {
		return reply(ctx, nil, nil)
	}
	return reply(ctx, locs, nil)
}

// --- from rename.go ---

// dispatchRename handles textDocument/rename. Builds a WorkspaceEdit
// that swaps the leaf identifier under the cursor at every matching
// Assignment / Block across the workspace's .pxf files — both the
// open-document set and on-disk files reached by the same filesystem
// walk used for references.
//
// Scope mirrors references for consistency: clicking "Find
// References" then "Rename Symbol" on the same identifier sees the
// same hit set in both surfaces. The chameleon layering use case
// (rename `host` to `endpoint` once, every layer that touches
// `db.host` gets the new key) doesn't require the user to have
// already opened every layer file.
//
// Per LSP spec, WorkspaceEdit.Changes can target URIs the client
// hasn't opened; modern editors apply those edits directly to disk
// (re-reading the file, applying the TextEdit, writing back) without
// surfacing a tab. Document this expectation on the client side if
// your editor handles WorkspaceEdit differently.
//
// Caveat: PXF field names are usually schema-defined. Renaming a
// field in PXF without a corresponding change to the underlying
// `.proto` produces an "unknown field" diagnostic on the next
// parse. The server doesn't prevent this — surfacing the rename
// at the LSP layer lets the user coordinate the cross-repo edit
// (PXF + proto + registry rebuild) through their editor's diff
// view rather than hunting field references by grep.
func (s *Server) dispatchRename(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.RenameParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling RenameParams: "+err.Error()))
	}
	// New-name validation depends on what the cursor targets. A keyed
	// repeated field's entry key (draft -01 §3.13) may be any non-empty
	// single-line string — the rewrite quotes it when it isn't
	// identifier-safe — whereas an ordinary field/block must stay a
	// valid PXF identifier.
	if s.RenameTargetKind(params.TextDocument.URI, params.Position) == analysis.RenameTargetKeyedEntry {
		if !IsValidKeyedEntryKey(params.NewName) {
			return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InvalidParams,
				"new name is not a valid keyed entry key: "+params.NewName))
		}
	} else if !IsValidPxfIdentifier(params.NewName) {
		// LSP convention: invalid new name returns an error so the
		// editor can show a meaningful message in the rename
		// dialog rather than silently producing a broken document.
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InvalidParams,
			"new name is not a valid PXF identifier: "+params.NewName))
	}
	edit := s.RenameEditFor(ctx, params.TextDocument.URI, params.Position, params.NewName)
	if edit == nil {
		return reply(ctx, nil, nil)
	}
	return reply(ctx, edit, nil)
}

// --- from secret_ranges_request.go ---

// dispatchSecretRanges handles MethodSecretRanges. Returns the
// current snapshot for the requested document — empty when the
// document isn't open or has no secrets. Never errors — like
// the other "view" requests (registryStatus, getEffectiveConfig),
// editor status-bar code shouldn't have to model failure modes.
func (s *Server) dispatchSecretRanges(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	// Mark this session as a legacy secret-ranges client so the
	// matching *Changed notifications keep firing. RPC-only
	// clients never reach here, so notifySecretRangesChanged
	// gates on the bool and skips conn.Notify entirely for them.
	s.legacyLspClients.secretRanges.Store(true)
	var params SecretRangesParams
	if raw := req.Params(); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
				"unmarshalling SecretRangesParams: "+err.Error()))
		}
	}
	doc := s.Documents().Get(params.TextDocument.URI)
	if doc == nil {
		return reply(ctx, SecretRangesResult{Ranges: []SecretRangeEntry{}}, nil)
	}
	doc.RLock()
	entries := ToSecretRangeEntries(doc.SecretRanges)
	doc.RUnlock()
	return reply(ctx, SecretRangesResult{Ranges: entries}, nil)
}

// --- from dataset_regions.go ---

// dispatchDatasetRegions handles MethodDatasetRegions. Returns
// the document's current snapshot; empty (non-nil) when the
// document isn't open or has no @dataset blocks. Never errors —
// editor status-bar code shouldn't model failure for what's
// essentially a UI hint.
func (s *Server) dispatchDatasetRegions(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	// Mark this session as a legacy dataset-regions client so the
	// matching *Changed notifications keep firing. See #113.
	s.legacyLspClients.datasetRegions.Store(true)
	var params DatasetRegionsParams
	if raw := req.Params(); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
				"unmarshalling DatasetRegionsParams: "+err.Error()))
		}
	}
	doc := s.Documents().Get(params.TextDocument.URI)
	if doc == nil {
		return reply(ctx, DatasetRegionsResult{Regions: []DatasetRegion{}}, nil)
	}
	doc.RLock()
	regions := make([]DatasetRegion, len(doc.DatasetRegions))
	copy(regions, doc.DatasetRegions)
	doc.RUnlock()
	return reply(ctx, DatasetRegionsResult{Regions: regions}, nil)
}

// --- from chain_status.go ---

// dispatchChainStatus handles MethodChainStatus. Always replies with
// a non-nil ChainStatusResult; missing chain → configured=false +
// empty environments/errors. Errors-populated + empty environments
// → "configured but broken" — chainSpecPath shows where the editor
// can point the user.
func (s *Server) dispatchChainStatus(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	if raw := req.Params(); len(raw) > 0 && string(raw) != "null" {
		var params ChainStatusParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
				"unmarshalling ChainStatusParams: "+err.Error()))
		}
	}
	return reply(ctx, s.ChainStatusSnapshot(), nil)
}

// --- from layer_info.go ---

// dispatchLayerInfo handles MethodLayerInfo. Returns the document's
// cached classification; computes on the fly when the cache is
// empty (e.g. an editor that calls layerInfo before any didOpen
// fires — unusual but not wrong).
//
// Never errors — UI hint contract. Unknown URI yields
// IsLayer=false; null params is tolerated for editors that
// pre-encode the request shape.
func (s *Server) dispatchLayerInfo(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params LayerInfoParams
	if raw := req.Params(); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
				"unmarshalling LayerInfoParams: "+err.Error()))
		}
	}
	doc := s.Documents().Get(params.TextDocument.URI)
	if doc == nil {
		// Document not open — return a fresh classification rather
		// than the empty fallback so the editor still gets useful
		// info (e.g. for a file it's about to open).
		return reply(ctx, s.ComputeLayerInfo(params.TextDocument.URI), nil)
	}
	doc.RLock()
	info := doc.LayerInfo
	doc.RUnlock()
	if info == nil {
		// Cache miss (didOpen hadn't computed it for some reason).
		// Fill it now so the next request hits cache.
		info = s.ComputeLayerInfo(doc.URI)
		doc.Lock()
		doc.LayerInfo = info
		doc.Unlock()
	}
	return reply(ctx, info, nil)
}

// --- from effective_config.go ---

// dispatchGetEffectiveConfig handles MethodGetEffectiveConfig.
// Always succeeds (no JSON-RPC errors) — editor status-bar code
// shouldn't have to model failure modes for what is essentially a
// UI hint request.
func (s *Server) dispatchGetEffectiveConfig(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	// Mark this session as a legacy effective-config client so
	// the matching *Changed notifications keep firing. See #113.
	s.legacyLspClients.effectiveConfig.Store(true)
	var params GetEffectiveConfigParams
	if raw := req.Params(); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
				"unmarshalling GetEffectiveConfigParams: "+err.Error()))
		}
	}
	sections := params.Sections
	if len(sections) == 0 {
		sections = KnownSections
	}

	values := make(map[string]ConfigValue, len(sections))
	for _, section := range sections {
		v := s.ResolveSection(section)
		if !params.RevealSecrets && redact.IsSensitiveSection(section) && v.Value != "" && v.Value != nil {
			v.Value = redact.Placeholder
		}
		values[section] = v
	}
	return reply(ctx, GetEffectiveConfigResult{
		WorkspaceDir: params.WorkspaceDir,
		Values:       values,
	}, nil)
}

// --- from overrides.go ---

// dispatchSetOverrides handles MethodSetOverrides. Steps:
//
//  1. Parse params. Empty / nil overrides map is allowed (no-op).
//  2. Validate each section: must be in KnownSections, value must
//     be of the expected type for that section (or null to clear).
//  3. Snapshot effective config before applying.
//  4. Mutate Server.overrides under lock — null clears, non-null
//     sets.
//  5. Snapshot effective config after applying.
//  6. Build the response (per-section previousSource + newValue)
//     and the notification (only effective-value changes).
//  7. Reply, then emit effectiveConfigChanged.
//
// Steps 3–5 happen under the overrides mutex to keep the
// snapshots coherent against concurrent readers.
func (s *Server) dispatchSetOverrides(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	// Mark this session as a legacy effective-config client —
	// setOverrides and getEffectiveConfig share the matching
	// LSP notification surface. See #113.
	s.legacyLspClients.effectiveConfig.Store(true)
	var params SetOverridesParams
	if raw := req.Params(); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
				"unmarshalling SetOverridesParams: "+err.Error()))
		}
	}

	if err := ValidateOverrides(params.Overrides); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InvalidParams, err.Error()))
	}

	// Snapshot → apply → snapshot pattern. Holds overridesMu for
	// the whole window so a concurrent getEffectiveConfig can't
	// observe a half-applied state across multi-section calls.
	// The locked snapshot helpers read s.overrides directly under
	// the held mutex (avoiding the otherwise-recursive
	// resolveFromOverride re-acquisition).
	sections := SectionsOf(params.Overrides)
	s.Config().LockOverrides()
	before := s.SnapshotSectionsLocked(sections)
	s.ApplyOverridesLocked(params.Overrides)
	after := s.SnapshotSectionsLocked(sections)
	s.Config().UnlockOverrides()

	applied := make(map[string]SetOverridesApplied, len(params.Overrides))
	for section := range params.Overrides {
		applied[section] = SetOverridesApplied{
			PreviousSource: before[section].Source,
			NewValue:       after[section].Value,
		}
	}

	changes := DiffSnapshots(before, after)
	if err := reply(ctx, SetOverridesResult{Applied: applied}, nil); err != nil {
		return err
	}
	if len(changes) > 0 {
		s.NotifyEffectiveConfigChanged(ctx, params.WorkspaceDir, changes)
	}
	return nil
}

// --- from list_messages.go ---

// dispatchListMessages handles [MethodListMessages]. Thin transport
// shim over [analysis.Workspace.ListMessages] — the dedupe /
// prefix-filter / origin-attribution / sort recipe lives there so
// the RPC sidecar and in-process embedders share it.
func (s *Server) dispatchListMessages(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params ListMessagesParams
	// Params are optional. An empty / missing body is treated as
	// "no filter" — Unmarshal of nil into a zero-value struct is
	// fine, but a literal `null` body needs the empty-check first.
	if raw := req.Params(); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
				"unmarshalling ListMessagesParams: "+err.Error()))
		}
	}
	res, err := s.ListMessages(ctx, params.Prefix)
	if err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InternalError,
			"listMessages cancelled: "+err.Error()))
	}
	return reply(ctx, res, nil)
}

// --- from source_content.go ---

// dispatchSourceContent handles the protolsp/sourceContent custom
// request. Thin transport shim over
// [analysis.Workspace.SourceContent]: InvalidParams for a
// malformed URI, InternalError for missing lookup / fetch failures,
// and a clean content payload on success. The `_internal_`
// namespace routing to the compiled-in lookup lives in the engine
// recipe.
func (s *Server) dispatchSourceContent(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params SourceContentParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling SourceContentParams: "+err.Error()))
	}
	res, err := s.SourceContentForWorkspace(ctx, params.URI, params.WorkspaceDir)
	if err != nil {
		code := jsonrpc2.InternalError
		if errors.Is(err, analysis.ErrNotRegistryURI) {
			code = jsonrpc2.InvalidParams
		}
		return reply(ctx, nil, jsonrpc2.NewError(code, err.Error()))
	}
	return reply(ctx, res, nil)
}

// --- from registry_status.go ---

// dispatchEngineConfig answers [MethodEngineConfig]: the workspace's
// effective RFC-001 §9.4 engine configuration, resolved through the
// same shared loader `pxf build` uses. Always replies with a result —
// a malformed configuration file travels in the Error field.
func (s *Server) dispatchEngineConfig(ctx context.Context, reply jsonrpc2.Replier, _ jsonrpc2.Request) error {
	return reply(ctx, s.EngineConfig(), nil)
}

// dispatchRegistryStatus answers [MethodRegistryStatus]. Always
// returns a result (never errors) — the editor's status-bar code
// path shouldn't have to handle JSON-RPC errors for what is
// essentially a UI hint. Thin shim over
// [analysis.Workspace.RegistryStatus].
func (s *Server) dispatchRegistryStatus(ctx context.Context, reply jsonrpc2.Replier, _ jsonrpc2.Request) error {
	return reply(ctx, s.RegistryStatus(), nil)
}

// --- from watched_files.go ---

// dispatchDidChangeWatchedFiles handles
// workspace/didChangeWatchedFiles. Notification, so no reply
// body. The actual classify + reload + reanalyze work runs
// through ScheduleWatchedFilesProcess so back-to-back
// notifications coalesce — see Server.watchedFilesWindow.
func (s *Server) dispatchDidChangeWatchedFiles(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.DidChangeWatchedFilesParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		s.Logger().Warn("didChangeWatchedFiles: bad params", slog.String("err", err.Error()))
		return reply(ctx, nil, nil)
	}
	s.ScheduleWatchedFilesProcess(ctx, params.Changes)
	return reply(ctx, nil, nil)
}

// --- from diagnostics.go ---

// dispatchDidOpen handles textDocument/didOpen. Stores the document and
// runs parse + diagnostic publishing.
func (s *Server) dispatchDidOpen(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.DidOpenTextDocumentParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		s.Logger().Warn("didOpen: bad params", slog.String("err", err.Error()))
		return reply(ctx, nil, nil) // notifications: no response on error
	}
	// Non-file URIs are virtual schemes the editor opens for
	// read-only previews — most commonly the registry:// previews
	// the schema resolver populates with .proto sources from the
	// protolsp/sourceContent RPC. They aren't on disk, they don't
	// change, and protolsp's analysis pipeline doesn't apply.
	// Skipping them outright also avoids panics: go.lsp.dev/uri's
	// Filename() (called from analyzeProto, isWorkspaceConfigDoc,
	// isChainSpecDoc, and others on this path) panics on non-file
	// schemes.
	if !IsFileURI(params.TextDocument.URI) {
		s.Logger().Debug("didOpen: ignoring non-file URI",
			slog.String("uri", string(params.TextDocument.URI)))
		return reply(ctx, nil, nil)
	}
	doc := &Document{
		URI:     params.TextDocument.URI,
		Version: params.TextDocument.Version,
		Source:  []byte(params.TextDocument.Text),
	}
	s.Documents().Open(doc)
	// Workspace discovery (W4): map the doc to its nearest-
	// ancestor `.protolsp.pxf` and lazy-load that workspace's
	// fileConfig + chain if it's a sub-workspace. Must happen
	// BEFORE layer classification so DetectLayerInfo sees the
	// right chain.
	s.EnsureWorkspaceForDoc(doc)
	// Chameleon layer classification (C2). Done before the analyze
	// pipeline so any downstream behavior that consults
	// doc.LayerInfo (C3+ — required-field weighting, etc.) sees a
	// populated cache on first run.
	s.DetectLayerInfo(doc)
	// didOpen runs the pipeline synchronously — users expect
	// immediate diagnostics on file open. Previous SecretRanges
	// and DatasetRegions snapshots are empty (the document is
	// new); any present on first analyze fire one notification.
	// The wrapper routes through liveReload for config / chain
	// docs so opening a `.protolsp.pxf` with unsaved-on-restore
	// edits (or the very-first open after server start) picks
	// up the in-memory state.
	s.RunAnalyzeAndMaybeLiveReload(ctx, doc.URI, nil, nil)
	return reply(ctx, nil, nil)
}

// --- from diagnostics.go ---

// dispatchDidChange handles textDocument/didChange. I1 of the
// incremental-sync design: applies LSP content-change events in
// order against the previously-stored source. Two event shapes
// supported per the LSP spec:
//
//   - Incremental: { range, text } — replace the byte range with text
//   - Full-document fallback: { text } (no range) — wholesale replace
//
// Edits in one notification apply IN ORDER; each subsequent edit's
// range is relative to the state AFTER the previous edit. Out-of-
// bounds ranges log + best-effort continue (logged with the
// offending change index for debugging).
func (s *Server) dispatchDidChange(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	// Custom params parsing: we need pointer-form Range on each
	// content change to distinguish full-document fallback from
	// incremental edits. See edit_apply.go's IncrementalChange.
	var params struct {
		TextDocument struct {
			URI     uri.URI `json:"uri"`
			Version int32   `json:"version"`
		} `json:"textDocument"`
		ContentChanges json.RawMessage `json:"contentChanges"`
	}
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		s.Logger().Warn("didChange: bad params", slog.String("err", err.Error()))
		return reply(ctx, nil, nil)
	}
	// Same non-file URI guard as dispatchDidOpen. didChange CREATES
	// a Document on the fly if Update doesn't find one (line ~162),
	// so a registry://-prefixed didChange would bypass the open-time
	// guard and crash downstream in isWorkspaceConfigDoc /
	// IsChainSpecDoc via URI.Filename().
	if !IsFileURI(params.TextDocument.URI) {
		return reply(ctx, nil, nil)
	}
	changes, err := UnmarshalIncrementalChanges(params.ContentChanges)
	if err != nil {
		s.Logger().Warn("didChange: bad contentChanges",
			slog.String("err", err.Error()))
		return reply(ctx, nil, nil)
	}
	if len(changes) == 0 {
		return reply(ctx, nil, nil)
	}

	// Capture the previous secret-range + dataset-region snapshots
	// BEFORE we re-analyze so we can diff against the post-edit
	// state for the corresponding change notifications.
	var previousSecrets []SecretRange
	var previousDatasets []DatasetRegion
	var initial []byte
	if existing := s.Documents().Get(params.TextDocument.URI); existing != nil {
		existing.RLock()
		previousSecrets = existing.SecretRanges
		previousDatasets = existing.DatasetRegions
		initial = existing.Source
		existing.RUnlock()
	}

	// Apply all changes in order. Failures (invalid range, bad
	// UTF-8) log but don't fail the dispatch — the source we end
	// up with reflects whatever applied successfully.
	source, applyErrs := ApplyContentChanges(initial, changes, s.PositionEncoding())
	for _, aerr := range applyErrs {
		s.Logger().Warn("didChange: edit apply failed; document may be out of sync",
			slog.String("err", aerr.Error()))
	}

	// Update or create the stored document with the post-edit
	// source. Analysis state stays stale until the deferred
	// Analyze fires; cursor features (hover, completion) read the
	// previous AST during the debounce window — accepted per
	// the incremental-sync design's §"Position invalidation
	// between debounce window and analyze".
	if !s.Documents().Update(params.TextDocument.URI, params.TextDocument.Version, source) {
		s.Documents().Open(&Document{
			URI:     params.TextDocument.URI,
			Version: params.TextDocument.Version,
			Source:  source,
		})
	}
	stored := s.Documents().Get(params.TextDocument.URI)
	if stored == nil {
		// Shouldn't happen after the Open above; defensive.
		return reply(ctx, nil, nil)
	}
	s.ScheduleAnalyzePipeline(ctx, stored, previousSecrets, previousDatasets)
	return reply(ctx, nil, nil)
}

// --- from diagnostics.go ---

// dispatchDidClose handles textDocument/didClose. Removes the document
// from the store and publishes an empty diagnostic list so the client
// clears any squiggles it had drawn.
func (s *Server) dispatchDidClose(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params protocol.DidCloseTextDocumentParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		s.Logger().Warn("didClose: bad params", slog.String("err", err.Error()))
		return reply(ctx, nil, nil)
	}
	// Non-file URIs were never tracked (dispatchDidOpen / didChange
	// short-circuit them), so there's nothing to clean up here.
	// Skipping also keeps PublishDiagnosticsRaw from emitting a
	// stray clear-diagnostics notification for a URI we never had
	// diagnostics on.
	if !IsFileURI(params.TextDocument.URI) {
		return reply(ctx, nil, nil)
	}
	u := params.TextDocument.URI
	// Cancel any pending deferred analyze before tearing down the
	// document — prevents a fired timer from operating on a closed
	// doc (Get returns nil, so the pipeline is a no-op anyway, but
	// stopping the timer saves a needless wakeup).
	var workspaceDir string
	if existing := s.Documents().Get(u); existing != nil {
		s.CancelPendingAnalyze(existing)
		workspaceDir = existing.WorkspaceDir()
	}
	s.Documents().Close(u)
	// Empty diagnostics clears any prior squiggles for this URI.
	s.PublishDiagnosticsRaw(ctx, u, 0, nil)
	// R4: evict the doc's sub-workspace state if it was the last
	// open doc inside it. Re-opens trigger a fresh discover-and-
	// load via ensureSubWorkspace, so this is purely a memory /
	// goroutine reclamation step. No-op for root-workspace docs
	// and for docs that never got a WorkspaceDir.
	s.MaybeEvictSubWorkspace(workspaceDir)
	return reply(ctx, nil, nil)
}

// --- from config_attrs.go ---

// dispatchConfigList answers [MethodConfigList]. Never fails:
// the attribute table is compiled in, no I/O involved.
func (s *Server) dispatchConfigList(ctx context.Context, reply jsonrpc2.Replier, _ jsonrpc2.Request) error {
	attrs := make([]ConfigAttribute, 0, len(workspaceconfig.AttributeRegistry))
	for _, a := range workspaceconfig.AttributeRegistry {
		attrs = append(attrs, ConfigAttribute{
			Name:        a.Name,
			DisplayName: a.DisplayName,
			Description: a.Description,
			Kind:        a.Kind.String(),
		})
	}
	return reply(ctx, ConfigListResult{Attributes: attrs}, nil)
}

// --- from config_attrs.go ---

// dispatchConfigGet answers [MethodConfigGet]. Loads the
// workspace's `.protolsp.pxf`, reads the literal value, returns
// it alongside the attribute's Kind for the client to render.
func (s *Server) dispatchConfigGet(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params ConfigGetParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling ConfigGetParams: "+err.Error()))
	}
	spec, ok := workspaceconfig.LookupAttribute(params.Name)
	if !ok {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InvalidParams,
			fmt.Sprintf("unknown attribute %q", params.Name)))
	}
	cfgPath, pathErr := s.ConfigPathForWorkspace(params.WorkspaceDir)
	if pathErr != nil {
		return reply(ctx, nil, configPathJSONError(pathErr))
	}
	doc, err := workspaceconfig.LoadDocument(cfgPath)
	if err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InternalError,
			"loading workspace config: "+err.Error()))
	}
	val, present, err := workspaceconfig.GetAttribute(doc, params.Name)
	if err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InternalError,
			"reading attribute: "+err.Error()))
	}
	return reply(ctx, ConfigGetResult{
		Name:    params.Name,
		Kind:    spec.Kind.String(),
		Value:   val,
		Present: present,
	}, nil)
}

// --- from config_attrs.go ---

// dispatchConfigSet answers [MethodConfigSet]. Round-trip:
// load → mutate AST → atomically write → fire the same reload
// cascade workspace/didChangeWatchedFiles would, so the Set
// call is indistinguishable from an external edit for every
// downstream consumer (lookup rebuild, diagnostics, etc.).
func (s *Server) dispatchConfigSet(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params ConfigSetParams
	if err := json.Unmarshal(req.Params(), &params); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling ConfigSetParams: "+err.Error()))
	}
	spec, ok := workspaceconfig.LookupAttribute(params.Name)
	if !ok {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InvalidParams,
			fmt.Sprintf("unknown attribute %q", params.Name)))
	}
	value, err := DecodeRawByKind(params.Value, spec.Kind)
	if err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InvalidParams,
			fmt.Sprintf("attribute %q: %s", params.Name, err.Error())))
	}
	cfgPath, pathErr := s.ConfigPathForWorkspace(params.WorkspaceDir)
	if pathErr != nil {
		return reply(ctx, nil, configPathJSONError(pathErr))
	}
	doc, err := workspaceconfig.LoadDocument(cfgPath)
	if err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InternalError,
			"loading workspace config: "+err.Error()))
	}
	if err := workspaceconfig.SetAttribute(doc, params.Name, value); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InvalidParams,
			err.Error()))
	}
	if err := workspaceconfig.WriteDocument(cfgPath, doc); err != nil {
		return reply(ctx, nil, jsonrpc2.NewError(jsonrpc2.InternalError,
			"writing workspace config: "+err.Error()))
	}
	s.Logger().Info("configSet rewrote workspace config",
		slog.String("path", cfgPath),
		slog.String("attribute", params.Name),
	)
	s.ReloadAfterConfigEdit(ctx, filepath.Dir(cfgPath))
	return reply(ctx, ConfigSetResult{OK: true}, nil)
}

// configPathJSONError maps ConfigPathForWorkspace's sentinel errors
// onto JSON-RPC error codes: a caller-supplied unknown workspaceDir
// is InvalidParams; a missing root workspace is a server-side
// InternalError (matches the pre-#172 inline codes).
func configPathJSONError(err error) *jsonrpc2.Error {
	if errors.Is(err, analysis.ErrUnknownWorkspaceDir) {
		return jsonrpc2.NewError(jsonrpc2.InvalidParams, err.Error())
	}
	return jsonrpc2.NewError(jsonrpc2.InternalError, err.Error())
}

// --- from runtime_violations.go ---

// dispatchSetRuntimeViolations handles the MethodSetRuntimeViolations
// custom notification: full-replace the document's runtime-violation
// overlay and republish its diagnostics immediately (when open) so
// the editor reflects the new runtime state without waiting for an
// edit. Notification semantics — bad params log + drop rather than
// error (there's no caller waiting on a reply), matching the
// pxfed/setBwSession precedent. The publish is synchronous, like
// didOpen's: it's a resolve-plus-notify, cheap enough for the
// dispatch goroutine, and it keeps tests deterministic.
func (s *Server) dispatchSetRuntimeViolations(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	var params SetRuntimeViolationsParams
	if raw := req.Params(); len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &params); err != nil {
			s.Logger().Warn("protolsp/setRuntimeViolations: bad params",
				slog.String("err", err.Error()))
			return reply(ctx, nil, nil)
		}
	}
	if params.TextDocument.URI == "" {
		s.Logger().Warn("protolsp/setRuntimeViolations: missing textDocument.uri")
		return reply(ctx, nil, nil)
	}
	s.SetRuntimeViolations(ctx, params.TextDocument.URI, params.Violations)
	return reply(ctx, nil, nil)
}
