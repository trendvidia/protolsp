// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"sync"
	"sync/atomic"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	schemav1 "github.com/trendvidia/protocompile/gen/protowire/schema/v1"
	"github.com/trendvidia/protowire-go/encoding/pxf"

	"github.com/trendvidia/protolsp/protoast"
)

// Document is the in-memory state for one open .pxf file. M2 stores the
// parse result alongside source bytes so subsequent handlers (M4 hover,
// future completion) don't have to re-parse on every request.
//
// Concurrency: mu guards every field below except URI (immutable
// post-construction) and workspaceDir (self-synchronized — see the
// field comment). analyze takes mu.Lock for its duration; handlers
// RLock for the duration of any read of a guarded field. The
// analyzeTimer / analyzeMu pair stays separate so timer scheduling
// doesn't serialize all reads.
type Document struct {
	URI uri.URI

	// mu guards the mutable Document state — Source, Version,
	// Parsed, ParseErr, LastGoodParse, SchemaErr, UnknownFields,
	// deprecatedUses, SecretRanges, secretValues, DatasetRegions,
	// LayerInfo. analyze acquires the write lock; handlers RLock.
	// Lock order: Documents.mu before Document.mu; never the
	// reverse.
	mu sync.RWMutex

	Version int32
	Source  []byte

	// Parsed is the result of pxf.Parse(Source), or nil when the source
	// failed to parse. ParseErr captures the parser error in that case;
	// at most one of Parsed / ParseErr is meaningful at a time.
	Parsed   *pxf.Document
	ParseErr error

	// inline is what Parsed's @proto directives compile to (#287): the
	// document's own types, which findTypeForDoc consults first.
	inline inlineSchema

	// LastGoodParse is the most recent AST that parsed successfully.
	// Falls in when Parsed is nil so completion (and any other AST-
	// aware feature) keeps working through transient mid-edit parse
	// failures — typing `field = ` produces a single broken line, but
	// the structure of the rest of the document is unchanged.
	LastGoodParse *pxf.Document

	// SchemaErr captures errors from pxf.UnmarshalDescriptor that
	// aren't per-field findings — type mismatches, missing-required,
	// oneof violations. Unknown-field errors are suppressed via
	// DiscardUnknown and reported through UnknownFields instead so
	// each occurrence gets its own diagnostic.
	SchemaErr error

	// UnknownFields and deprecatedUses are AST-walk findings the
	// validate() pass produces alongside SchemaErr. Each entry is one
	// field assignment in the source that names something the bound
	// message doesn't know (UnknownFields) or names a field marked
	// `[deprecated = true]` (deprecatedUses). diagnosticsFor turns
	// these into LSP diagnostics; the code-action handler reads
	// UnknownFields back via Diagnostic.Data to offer "Remove field"
	// quick fixes.
	UnknownFields  []fieldFinding
	deprecatedUses []fieldFinding

	// protocheckFindings records one entry per protocheck constraint
	// violation against the schema-resolved message (Phase 1). The
	// FieldPath is protocheck's dotted path; Pos is the resolved
	// source position (or {Line:1,Col:1} when the path didn't
	// resolve in the AST). diagnosticsFor renders these as
	// "protocheck" diagnostics. Always empty in the OSS build —
	// the runProtocheck implementation is gated behind a build tag.
	protocheckFindings []protocheckFinding

	// protovalidateFindings is the protovalidate counterpart of
	// protocheckFindings — same shape, same emission pipeline,
	// distinct diagnostic source ("buf/validate"). Always
	// populated regardless of build (protovalidate-go is Apache
	// 2.0). Coexists with protocheckFindings since the two engines
	// look at different annotation families and never double-report.
	protovalidateFindings []protovalidateFinding

	// SecretRanges records every pxf.Secret-typed value assignment
	// in the source. Public, no value bytes — safe for the
	// protolsp/secretRanges custom request (S2) and for the editor
	// to consume directly. The corresponding plaintext lives in
	// secretValues below, private to the server.
	SecretRanges []SecretRange

	// secretValues mirrors SecretRanges; one plaintext string per
	// range, used by internal/redact to scrub diagnostic / hover /
	// completion / code-action text before publishing. Never
	// exposed across the LSP boundary; kept on the document only
	// for the duration of analyze + downstream emit calls.
	secretValues []string

	// keyedFindings records the keyed-repeated-field schema violations
	// (draft -01 §3.13) the validate pass found via pxf.KeyedDiagnostics:
	// duplicate entry names, empty keys, explicit key-field assignments
	// disagreeing with their entry name, and quoted entry names outside a
	// keyed field. Reset each analyze pass; rendered by diagnosticsFor as
	// "protolsp/keyed" diagnostics. The strict decoder surfaces the first
	// of these as SchemaErr too, so validate suppresses keyed errors from
	// SchemaErr to avoid double-reporting.
	keyedFindings []pxf.KeyedError

	// DatasetRegions records every @dataset block detected in the
	// source. Populated by analyze via the line-based scanner
	// from the dataset-regions design (R1). Surfaces to editors
	// through the protolsp/datasetRegions custom request.
	DatasetRegions []DatasetRegion

	// LayerInfo caches the result of matching this document against
	// the workspace's chameleon chain (C2 of chameleon-integration).
	// Computed once at didOpen — the URI doesn't change, the chain
	// is workspace-scoped, and the result depends only on those two.
	// nil means "not computed yet" (no didOpen has run); a non-nil
	// pointer with IsLayer=false means "computed, file is not a
	// layer". The protolsp/layerInfo request reads this directly.
	LayerInfo *LayerInfoResult

	// workspaceDir caches the absolute path of the directory
	// holding this document's nearest-ancestor `.protolsp.pxf`
	// (W4 of workspace-config). Usually populated at didOpen via
	// EnsureWorkspaceForDoc; consulted by all chain-aware features
	// (through the WorkspaceDir accessor) to pick the right
	// per-workspace config / chain.
	//
	// Self-synchronized (atomic) rather than mu-guarded (#187):
	// readers — SubWorkspaceForDoc and every path behind it — run
	// both with mu held (Analyze's validate pass holds the write
	// lock; DefinitionFor holds RLock) and without it (the
	// watched-files reanalyze pass), so tying the field to mu
	// would force a *Locked-variant split through every accessor.
	// The field is also not set-once in practice: discovery is
	// lazy (nil/"" means "not yet discovered" and the next
	// EnsureWorkspaceForDoc call re-attempts), so a
	// `.protolsp.pxf` appearing on disk later lets the reanalyze
	// path populate it long after didOpen published the doc.
	workspaceDir atomic.Pointer[string]

	// analyzeTimer is the pending deferred-analyze timer (I3 of
	// incremental-sync). nil when no analyze is queued. didChange
	// stops any existing timer and starts a new one; didClose
	// stops the timer to avoid firing on a closed document.
	// Guarded by analyzeMu — separate from the Documents-store
	// lock so analyze pipeline runs don't serialize all document
	// operations.
	analyzeTimer *time.Timer
	analyzeMu    sync.Mutex

	// ProtoDiagnostics is populated for .proto documents by
	// analyzeProto — single-file protocompile compile errors
	// translated to LSP diagnostics. Replaces the entire diagnostic
	// set for .proto docs (the pxf-specific UnknownFields /
	// deprecatedUses / SchemaErr fields are all nil for proto docs).
	// Empty / nil for .pxf documents.
	ProtoDiagnostics []protocol.Diagnostic

	// protoAST is the parsed AST for .proto documents, retained
	// between analyze passes so hover + go-to-definition (Phase 2
	// of the proto-authoring plan) can resolve cursor positions to
	// declarations without re-parsing per request. nil until
	// analyzeProto runs successfully on this URI; reset on every
	// Analyze pass and re-populated by the parse step (the new
	// parser always yields a — possibly partial — AST, so the AST
	// routinely exists alongside non-empty ProtoDiagnostics). nil
	// for .pxf documents.
	protoAST *protoast.File

	// rfc001Calls holds the function-call sites the compile pass
	// resolved inside opaque engine-expression fragments (#220) —
	// plain byte-offset records, nothing referencing the compile
	// session. Reset with protoAST on every analyze pass; empty for
	// files without RFC-001 constructs. Guarded by mu like protoAST.
	rfc001Calls []rfc001CallSite

	// rfc001SourceMap is the canonical protowire.schema.v1 source
	// map lowered from the compile pass (#220): descriptor_path →
	// source anchor for every RFC-001 construct, matching what the
	// compiled artifact carries. nil for classic proto files. Serves
	// RFC001SourceRange (runtime-violation mapping). Guarded by mu.
	rfc001SourceMap *schemav1.SourceMap
}

// SecretRange records the source range of one pxf.Secret-typed
// value assignment. Public; surfaces to editors via the
// protolsp/secretRanges custom request (S2 phase).
type SecretRange struct {
	// Range covers the value token (scalar form) or the
	// `value = "..."` assignment line inside a block form — NOT
	// the field name. Editors mask exactly this range.
	Range protocol.Range
	// Path is the dotted field path, e.g. "credentials.password".
	Path string
	// Form names the surface syntax — useful for editor UI
	// affordances ("Reveal" placement, etc.).
	Form SecretForm
}

// SecretForm names the two PXF surface forms a pxf.Secret value
// can take. Per protowire-go's wellknown.go: scalar shorthand
// `pw = "x"` and block form `pw { value = "x"; hint = "..." }`.
type SecretForm int

const (
	// SecretFormScalar is `pw = "x"` — scalar shorthand sugar for
	// the inner `value` field. Range covers the string literal.
	SecretFormScalar SecretForm = iota
	// SecretFormBlock is `pw { value = "x"; hint = "..." }`. Range
	// covers the inner `value = ...` line; hint and fingerprint
	// are non-sensitive by design and stay un-redacted.
	SecretFormBlock
)

// SecretValues returns a copy of the private plaintext slice the
// redact package consumes. Defensive copy so callers can't poke
// at the server's internal state. Used by the diagnostic /
// hover / code-action emit paths to redact secret bytes before
// sending to the client.
//
// Takes doc.mu.RLock. For callers that already hold the lock
// (publishDiagnostics, the handler RLock pattern), use
// secretValuesLocked to avoid the unsafe recursive RLock.
func (d *Document) SecretValues() []string {
	if d == nil {
		return nil
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.secretValuesLocked()
}

// secretValuesLocked is the lock-free body of SecretValues.
// Caller MUST hold doc.mu.RLock (or Lock). Returns a defensive
// copy so the caller can release the lock and process the slice
// without worrying about the underlying buffer being rewritten.
func (d *Document) secretValuesLocked() []string {
	if len(d.secretValues) == 0 {
		return nil
	}
	out := make([]string, len(d.secretValues))
	copy(out, d.secretValues)
	return out
}

// snapshotParsed returns the current Parsed pointer under
// doc.mu.RLock. Analyze installs a freshly-allocated *pxf.Document
// on every pass and never mutates one after publication, so the
// returned pointer is safe to read after the lock is released —
// only the field itself needs the lock (#184: the watched-files
// reanalyze pass rewrites it concurrently with the didOpen /
// scheduled-analyze classification reads).
//
// Callers that already hold doc.mu must read d.Parsed directly —
// recursive RLock deadlocks against a waiting writer.
func (d *Document) snapshotParsed() *pxf.Document {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.Parsed
}

// WorkspaceDir returns the absolute path of the directory holding
// this document's nearest-ancestor `.protolsp.pxf`, or "" when
// discovery hasn't found one yet (the next EnsureWorkspaceForDoc
// call re-attempts). May equal the LSP workspace root — the root
// workspace's state lives on Workspace directly.
//
// Safe to call from any lock context: the backing field is atomic,
// not doc.mu-guarded (see the field comment).
func (d *Document) WorkspaceDir() string {
	if d == nil {
		return ""
	}
	if p := d.workspaceDir.Load(); p != nil {
		return *p
	}
	return ""
}

// SetWorkspaceDir installs the doc → workspace-directory mapping.
// Production discovery goes through EnsureWorkspaceForDoc; exported
// for embedders and tests that construct Documents with a known
// mapping. Concurrent callers are last-writer-wins — discovery is
// deterministic for a given on-disk config layout, so racing
// writers carry the same value (or a fresher one when a config
// file just appeared).
func (d *Document) SetWorkspaceDir(dir string) {
	d.workspaceDir.Store(&dir)
}

// updateSource writes Source + Version under the document write
// lock. Used by Documents.Update — keeps the doc-level mutation
// separate from the Documents map mutex (lock order: Documents.mu
// for the lookup; Document.mu for the field write).
func (d *Document) updateSource(version int32, source []byte) {
	d.mu.Lock()
	d.Version = version
	d.Source = source
	d.mu.Unlock()
}

// SnapshotForPipeline returns the version / secret-ranges /
// dataset-regions triple that runAnalyzePipeline feeds into
// notification helpers AFTER analyze has run. Defensive copies
// for the slices so notify callsites can compare against the
// prior snapshot without holding the lock.
func (d *Document) SnapshotForPipeline() (version int32, secrets []SecretRange, datasets []DatasetRegion) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	version = d.Version
	if len(d.SecretRanges) > 0 {
		secrets = append([]SecretRange(nil), d.SecretRanges...)
	}
	if len(d.DatasetRegions) > 0 {
		datasets = append([]DatasetRegion(nil), d.DatasetRegions...)
	}
	return version, secrets, datasets
}

// Documents is a thread-safe map of URI → Document. The LSP handler
// holds a single instance and mutates it from didOpen/didChange/didClose.
type Documents struct {
	mu  sync.RWMutex
	all map[uri.URI]*Document
}

// newDocuments creates an empty document store.
func newDocuments() *Documents {
	return &Documents{all: make(map[uri.URI]*Document)}
}

// Open adds a document to the store, replacing any existing entry for
// the same URI. Used by textDocument/didOpen.
func (d *Documents) Open(doc *Document) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.all[doc.URI] = doc
}

// Update replaces an existing document's source and bumps its version.
// Returns false if the URI was not previously opened.
//
// Lock-order: takes Documents.mu briefly to find the doc, then
// Document.mu to write. Concurrent reads of other documents (via
// Get) aren't blocked by an in-flight Update.
func (d *Documents) Update(u uri.URI, version int32, source []byte) bool {
	d.mu.RLock()
	doc, ok := d.all[u]
	d.mu.RUnlock()
	if !ok {
		return false
	}
	doc.updateSource(version, source)
	return true
}

// Close removes a document from the store. Returns false if it was not
// open.
func (d *Documents) Close(u uri.URI) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.all[u]; !ok {
		return false
	}
	delete(d.all, u)
	return true
}

// Get returns the document for a URI, or nil if not open. The returned
// pointer is safe to read but not to mutate concurrently with Update.
func (d *Documents) Get(u uri.URI) *Document {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.all[u]
}

// Len returns the number of currently-open documents. Used by tests.
func (d *Documents) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.all)
}

// URIs returns a snapshot slice of every open document's URI. Safe
// to call concurrently with mutations — the slice is a defensive
// copy taken under the read lock, so it stays valid even when
// subsequent Open / Close calls touch d.all.
//
// Used by W5's reanalyze pass: when fileConfig or the chain
// reloads, every open document needs its LayerInfo refreshed +
// diagnostics republished. Order isn't specified (Go map iteration
// is unordered).
func (d *Documents) uris() []uri.URI {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]uri.URI, 0, len(d.all))
	for u := range d.all {
		out = append(out, u)
	}
	return out
}

// Lock / Unlock / RLock / RUnlock expose the Document's state mutex
// to callers outside the analysis package (the LSP dispatch shell
// snapshots Source/Analysis under it; tests inspect state through
// it). Same lock the analyze pipeline takes — lock order remains
// Documents.mu before Document.mu.
func (d *Document) Lock()    { d.mu.Lock() }
func (d *Document) Unlock()  { d.mu.Unlock() }
func (d *Document) RLock()   { d.mu.RLock() }
func (d *Document) RUnlock() { d.mu.RUnlock() }
