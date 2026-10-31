// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package analysis is protolsp's transport-free analysis engine
// : the document store, schema lookup, workspace/chain/config
// state, and every analysis pass (parse + schema diagnostics, hover,
// completion, definition, code actions, secret ranges, dataset
// regions, effective config) — importable without jsonrpc2 or
// Connect-RPC.
//
// The entry point is the Workspace: construct one with New, install
// a Publisher to receive pushes (diagnostics + *Changed events), and
// feed documents through Documents(). The in-repo LSP server
// (internal/server) is one consumer; in-process embedders such as
// goed link this package directly and get the exact pipeline the LSP
// runs — same debounce, same workspace discovery, same secret
// handling.
//
// # API tiers
//
// The exported surface has two tiers. The EMBEDDER core is what an
// in-process consumer needs and is what we try hardest to keep
// stable: New + the Set* configuration knobs, Publisher and its
// payload types (SecretRange, DatasetRegion, ConfigValue), the
// Documents store and Document sync flow (EnsureWorkspaceForDoc,
// DetectLayerInfo, RunAnalyzeAndMaybeLiveReload,
// ScheduleAnalyzePipeline, CancelPendingAnalyze,
// PublishDiagnosticsRaw, MaybeEvictSubWorkspace), the feature
// queries (HoverFor, CompletionFor, DefinitionFor, CodeActionsFor,
// and friends), ScheduleWatchedFilesProcess, and the
// CancelBackground/Close teardown pair. Everything else — the LSP
// request/response DTOs, the protolsp/* Method constants, and the
// manager types' wider method sets — exists for the in-repo LSP/RPC
// shell and carries no compatibility promise while the module is
// pre-1.0.
//
// # Threading model
//
// Feature queries are safe to call concurrently with the analyze
// pipeline and with each other: they take per-document read locks
// and snapshot manager state internally. The one exception is
// CodeActionsFor, whose caller must hold doc.RLock so the computed
// edits stay consistent with the diagnostics passed in (see its doc
// comment). The Set* configuration knobs are setup-time plain
// writes: call them after New and before feeding documents or
// spawning queries, not concurrently with either. Mutating entry
// points — document open/update/close, the LoadWorkspace*/Rebuild*
// loaders, and watched-files processing — are individually
// thread-safe, but
// embedders are encouraged to serialize them on one goroutine the
// way both in-tree consumers do (the LSP shell via its connection
// loop, goed via a single worker): it makes operation ordering
// deterministic and keeps slow loads from interleaving. Publisher
// callbacks fire on pipeline goroutines (including debounce timers)
// and must not block; payload slices are shared with engine state
// and must be copied if retained.
//
// URIs must be file-scheme (uri.File); guard foreign URIs with
// IsFileURI before feeding them in — several internal paths resolve
// URI.Filename, which panics on non-file schemes.
package analysis

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trendvidia/protolsp/protoast"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"google.golang.org/protobuf/proto"

	"buf.build/go/protovalidate"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoindex"
	"github.com/trendvidia/protolsp/schema"
)

// Workspace is the transport-free core of protolsp: the document
// store, schema lookup, workspace/chain/config state, and every
// analysis pass (parse + schema diagnostics, hover, completion,
// definition, code actions, secret ranges, dataset regions,
// effective config). It pushes results through the Publisher seam
// and never touches jsonrpc2 or the Connect-RPC sidecar.
//
// The LSP Server (internal/server) embeds *Workspace, so dispatch
// and RPC handlers reach engine state through method promotion;
// in-process embedders (goed) construct one via New and drive
// it directly.
type Workspace struct {
	docs *Documents
	log  *slog.Logger

	// pub is the engine's outbound seam: every server→client push
	// (diagnostics, showMessage, secret/dataset/config *Changed)
	// flows through it instead of touching a transport directly.
	// The LSP server installs serverPublisher (conn notify + RPC
	// fan-out + legacy gating); in-process embedders install their
	// own. Always non-nil after New.
	pub Publisher

	// lookupMgr owns the user-registry-backed schema.Lookup plus
	// the async-rebuild bookkeeping (cancel, last-built-cfg diff).
	// Always non-nil after New. Read through lookupMgr.Snapshot;
	// write through lookupMgr.Swap. Tests that need to install a
	// fake lookup call lookupMgr.Swap(fake, nil) — same shape as
	// production.
	//
	// Type resolution callsites consult [engine.findTypeForDoc]
	// rather than the manager directly so the internal-descriptor
	// fallback (protolsp.v1.WorkspaceConfig, chameleon.v1.ChainSpec)
	// is always available even when the user has no registry
	// configured. See [engine.internalLookup].
	lookupMgr *LookupManager

	// internalLookup serves the descriptors protolsp ships in its
	// binary — the schemas for `.protolsp.pxf` and `chain.pxf`.
	// Populated once at construction time; never nil. Used as a
	// fallback when the user lookup doesn't resolve a name (or
	// when no user lookup is configured at all). The user can't
	// shadow these — they're always reachable so opening one of
	// our own config files Just Works.
	internalLookup schema.Lookup

	// cfg owns Tier 3 editor settings, Tier 4 session overrides,
	// and the per-session client-capability snapshot. T1 PR B
	// moved these off Server. Always non-nil after New.
	cfg *ConfigState

	// ws owns the LSP workspace root, the root workspace's
	// fileConfig + chain, and the sub-workspaces map W4
	// maintains. T1 PR C moved these off Server. Always non-nil
	// after New.
	ws *WorkspaceManager

	// rootProtoFilesErrs caches the last set of CompileErrors from
	// the root workspace's proto_files lookup build (W2). Read by
	// the config_diagnostics path to anchor errors on the root
	// `.protolsp.pxf` when the user opens it. Sub-workspaces keep
	// the analogous slice on WorkspaceState.CompileErrors. Always
	// non-nil after New.
	rootProtoFilesErrs *rootProtoFilesCompileErrors

	// positionEncoding is the negotiated LSP position encoding
	// (I2 of incremental-sync). UTF-16 default for clients that
	// don't advertise positionEncodings; UTF-8 for clients that
	// do (LSP 3.17+). Set once at initialize time; every position-
	// to-byte conversion site consults this so the server speaks
	// the encoding the client expects.
	positionEncoding positions.Encoding

	// debounceWindow coalesces rapid didChange events (I3 of
	// incremental-sync). When > 0, didChange returns immediately
	// after applying the edit; analyze + publish + notify run on
	// a deferred timer that resets on each subsequent edit. When
	// 0 (the default — and the value tests use), the pipeline
	// runs synchronously inside didChange. didOpen always runs
	// synchronously regardless — users expect immediate
	// diagnostics on file open.
	debounceWindow time.Duration

	// watchedFilesWindow coalesces back-to-back
	// workspace/didChangeWatchedFiles notifications so a multi-
	// file rewrite (e.g. `git pull` touching a chain spec + several
	// layer files + a .proto simultaneously) runs the
	// classify → reload → reanalyzeOpenDocs pipeline once instead
	// of N times. Defaults to 200ms; tests override to 0 via
	// initializeWithWatchSupport to keep dispatch synchronous.
	watchedFilesWindow time.Duration

	// watchedFilesMu guards the pending-events buffer + timer for
	// the watched-files debouncer. Holding this lock across the
	// dispatch handler is fine because the lock is only ever held
	// for the buffer manipulation, not the actual reanalyze pass.
	watchedFilesMu      sync.Mutex
	watchedFilesPending []*protocol.FileEvent
	watchedFilesTimer   *time.Timer

	// maxReferencesFiles overrides the file-count cap used by
	// textDocument/references walks. 0 means use
	// defaultMaxReferencesWorkspaceFiles (10000). Tests set this
	// to a small number to exercise the truncation path without
	// writing 10k .pxf files to disk; production code never
	// touches it.
	maxReferencesFiles int

	// hugeRegionBytes is the byte-size threshold above which a
	// @dataset region is marked Huge (per the dataset-regions
	// design). 0 means use the default (100 KB) — see
	// DetectDatasetRegions in dataset_regions.go.
	hugeRegionBytes int

	// hugeRegionRows is the row-count threshold above which a
	// @dataset region is marked Huge. 0 means use the default
	// (1000 rows). Only meaningful when the AST pass populates
	// RowCount; the line-based pass alone uses the byte size.
	hugeRegionRows int

	// analyzeCount counts completed pipeline runs (analyze +
	// publish + notify). Tests use it via assert.Eventually to
	// wait out a debounce window without sleeping.
	analyzeCount atomic.Int64

	// protocheck validates .pxf data against schema-declared
	// constraints (Phase 1 of the protocheck integration). With no
	// ProtocheckBackend registered this stays nil and the
	// data-validation pass short-circuits; otherwise a real
	// *protocheck.Validator is plugged in via
	// newProtocheckValidator. Validator implementations are
	// thread-safe and cache compiled expressions across calls.
	// protocheckMu guards the current validator + its closer: read
	// via protocheckValidate (RLock held for the whole Validate
	// call so a swap can't close a validator mid-eval), swapped
	// via swapProtocheck when the effective engine config (§9.4
	// function_libraries) or the schema lookup changes. Leaf lock —
	// nothing else is acquired while it's held.
	protocheckMu    sync.RWMutex
	protocheck      protocheckValidator
	protocheckClose func() // releases the validator's VMs; nil with no backend
	// protocheckCatalogs holds the §7 locale catalogs loaded from
	// the engine config's catalog_libraries, keyed by BCP 47 tag —
	// swapped atomically with the validator they were built for.
	// protocheckCatalogPaths lists the absolute files they merged
	// from, for the watched-files classifier.
	protocheckCatalogs     map[string]protocheckCatalog
	protocheckCatalogPaths []string

	// clientLocale is the editor UI locale from InitializeParams
	// (BCP 47). Written once during initialize before documents
	// land, read without a lock — the SetPublisher discipline.
	// Selects the §7 catalog violation messages render through;
	// empty means spec fallback messages.
	clientLocale string

	// protocheckRebuildMu serializes rebuildProtocheck runs and
	// guards protocheckBaseline, the fingerprint of the config +
	// library resolution the current validator was built from —
	// rebuild triggers are cheap no-ops when nothing changed.
	protocheckRebuildMu sync.Mutex
	protocheckBaseline  string

	// protovalidate runs Buf's protovalidate engine against .pxf
	// data, surfacing violations declared via the open-source
	// `(buf.validate.field|message|oneof)` annotation family.
	// Coexists with the protocheck integration (gated by build
	// tag) — both engines look at different extension descriptors
	// and emit diagnostics under distinct sources (`buf/validate`
	// vs `protocheck`) so editors can color / filter independently.
	// Always tag-free since protovalidate-go is Apache 2.0.
	// Nil only when construction fails at New (logged); the
	// downstream pass skips silently.
	protovalidate protovalidate.Validator

	// bwSession is an in-memory BW_SESSION override pxfed pushes
	// in via the pxfed/setBwSession notification (or an embedder
	// pushes via SetBwSession). Layered into workspaceconfig's env
	// lookup so `${env.BW_SESSION}` (and other env refs the user
	// might funnel through the same override) resolve correctly
	// mid-session — without restarting protolsp to refresh the
	// inherited process env. Empty / unset means "fall back to
	// os.Getenv". Guarded by bwSessionMu so the write doesn't race
	// against in-flight interpolate reads in Parse.
	bwSessionMu sync.RWMutex
	bwSession   string

	// activeEnv is the session-scoped chameleon environment the
	// user has selected via the SetActiveEnv RPC. Empty means
	// "fall back to chain.default_environment" — both reads and
	// the wire response model this as the natural unset state.
	// Today purely a UI signal (the chain badge + panel render
	// based on it); future hover / effective-config flows can
	// consult it without re-negotiating a capability.
	activeEnvMu sync.RWMutex
	activeEnv   string

	// runtimeViolations is the per-document runtime-violation
	// overlay pushed in via protolsp/setRuntimeViolations:
	// RFC-001 (file, descriptor_path)-keyed violations a protowire
	// runtime reported, rendered as Information diagnostics with
	// source "protolsp/runtime" wherever the key still resolves
	// through the document's canonical source map. Workspace-scoped
	// (NOT on Document) so the set survives re-analysis and document
	// close/reopen — it describes runtime state, not the buffer.
	// Full-replace per document; nil until the first notification.
	// Guarded by runtimeViolationsMu; lock order is doc.mu →
	// runtimeViolationsMu (diagnosticsForLocked reads the map inside
	// the document-lock window), so never take a document lock while
	// holding runtimeViolationsMu.
	runtimeViolationsMu sync.RWMutex
	runtimeViolations   map[uri.URI][]RuntimeViolation

	// bufLintEnabled gates the buf-lint pass in analyzeProto.
	// Production callers via New() get true; test helpers default
	// it to false so existing proto tests don't spin up buf
	// subprocesses against whatever binary the dev machine
	// happens to have installed (and don't produce surprise lint
	// diagnostics for fixture files that weren't designed to pass
	// buf's default ruleset). Phase-5b tests that exercise the
	// integration flip it on explicitly + point PROTOLSP_BUF_PATH
	// at a fake.
	bufLintEnabled bool

	// protoIndex is the workspace-wide .proto import-path index
	// used by phase 3 of the proto-authoring plan for cross-file
	// hover + go-to-definition. Built at workspace setup
	// (LoadProtoIndex) and rebuilt on workspace-config reloads
	// alongside the schema-lookup and chain. Nil before the first
	// build attempt; the resolver tolerates a nil index (returns
	// no cross-file results) so workspaces that fail bufconfig
	// discovery still get Phase 2's in-file experience.
	protoIndexMu sync.RWMutex
	protoIndex   *protoindex.Index

	// protoIndexCacheRoot is a BSR-cache root override for
	// protoindex.NewWithCacheRoot. Empty means "use
	// bufconfig.ModuleCacheDir()" — the production default that
	// resolves to ~/.cache/buf/v3/modules (or the platform
	// equivalent). Tests set this to a staged fake cache so the
	// end-to-end buf.lock change → fresh-dep-resolves flow is
	// observable without touching the developer's real cache.
	protoIndexCacheRoot string

	// registryAST memoizes parsed ASTs of registry:// .proto sources
	// fetched via a schema lookup, so cross-file go-to-definition from
	// a registry preview (goed#119) doesn't re-fetch+re-parse an
	// imported file on every hover-underline check. Keyed by
	// "<namespace>\x00<import-path>"; a present-but-nil value records a
	// fetch/parse miss so a known-absent import isn't retried within a
	// lookup epoch. Cleared wholesale on any lookup swap (the
	// LookupManager onChanged seam), since a rebuild can change what a
	// path resolves to. Distinct from protoIndex, which serves on-disk
	// .proto files.
	registryASTMu sync.RWMutex
	registryAST   map[string]*protoast.File
}

// protocheckValidator is the minimal surface the engine needs from
// the optional protocheck integration. Matches
// protocheck.Validator.Validate so the real validator satisfies the
// interface without an adapter. Construction is gated by a build
// tag — see protocheck_runner_real.go (//go:build protocheck) and
// protocheck_runner_stub.go (//go:build !protocheck).
//
// Keeping this interface in tag-free code lets the engine struct
// reference it regardless of build, and lets every other file in
// the package call s.protocheck.Validate without compile-tag
// gymnastics at call sites.
type protocheckValidator interface {
	Validate(msg proto.Message) error
}
