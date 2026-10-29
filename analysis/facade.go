// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Workspace construction + the exported facade knobs.
//
// The Workspace type itself (the former internal/server engine) is
// declared in engine.go; this file owns everything an embedder or
// the LSP shell needs to build and configure one:
//
//   - New — allocates a fully-wired Workspace with a no-op Publisher.
//   - SetPublisher — installs the outbound seam (LSP shell installs
//     its jsonrpc2/RPC fan-out; in-process embedders install
//     callbacks).
//   - The knob setters/accessors the LSP shell and the test suite
//     use in place of the direct field pokes they had when Server
//     and engine shared a package.
package analysis

import (
	"context"
	"log/slog"
	"os"
	"time"

	"buf.build/go/protovalidate"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/schema"
)

// defaultWatchedFilesWindow is the production debounce window for
// workspace/didChangeWatchedFiles processing. Hand-picked: short
// enough that a save → diagnostics-update cycle feels instant,
// long enough that a multi-file rewrite (git pull, large refactor)
// coalesces into one classify + reload + reanalyze pass instead
// of N. Tests override to 0 via SetWatchedFilesWindow for
// synchronous dispatch.
const defaultWatchedFilesWindow = 200 * time.Millisecond

// New creates a Workspace with an empty document store and a no-op
// Publisher. Callers that want push results (diagnostics, secret /
// dataset / effective-config change events) install their seam via
// SetPublisher before feeding documents in.
func New(log *slog.Logger) *Workspace {
	if log == nil {
		log = slog.Default()
	}
	// Wire the optional protocheck validator. Returns (nil, nil)
	// under the default (OSS) build; the trendvidia build's tagged
	// implementation constructs a real, descriptor-less
	// *protocheck.Validator so validation works before any
	// workspace root/config is known. The initialize-time
	// RebuildProtocheckValidator swaps in the config-aware one.
	checker, checkerClose := newProtocheckValidator(log)
	// Build the protovalidate validator. Always present (Apache
	// 2.0 dep, no tag gating). Failure logs + leaves nil so the
	// data pass skips silently without taking the workspace down.
	pv, err := protovalidate.New()
	if err != nil {
		log.Warn("protovalidate validator construction failed; buf.validate diagnostics disabled",
			slog.String("err", err.Error()))
		pv = nil
	}
	w := &Workspace{
		docs:               newDocuments(),
		log:                log,
		pub:                NopPublisher{},
		cfg:                newConfigState(),
		ws:                 newWorkspaceManager(),
		internalLookup:     schema.NewInternalLookup(),
		rootProtoFilesErrs: &rootProtoFilesCompileErrors{},
		watchedFilesWindow: defaultWatchedFilesWindow,
		bufLintEnabled:     true,
		protocheck:         checker,
		protocheckClose:    checkerClose,
		protovalidate:      pv,
	}
	// Never-matching sentinel so the first rebuild always swaps in
	// a validator built against the resolved engine config.
	w.protocheckBaseline = "\x00unbuilt"
	// The manager fires the Publisher's LookupChanged seam whenever
	// an install/teardown lands. The closure reads w.pub at
	// fire time so a SetPublisher installed after construction is
	// honored. The manager owns the ROOT workspace's lookups, hence
	// workspaceDir "" — sub-workspace slots fire with their dir via
	// notifyLookupChanged.
	w.lookupMgr = newLookupManager(func() {
		// A lookup swap can change what an import path resolves to, so
		// drop the cross-file registry AST cache (goed#119) before
		// anything reads it against the new lookup.
		w.clearRegistryASTCache()
		// It can also flip whether the engine config's
		// function_libraries resolve, so re-derive the protocheck
		// validator. Fingerprint-diffed inside — free when no
		// libraries are configured. Safe here: Swap fires onChanged
		// after releasing lookupMu, so Snapshot() doesn't deadlock.
		w.RebuildProtocheckValidator(context.Background())
		w.pub.LookupChanged(context.Background(), "")
	})
	return w
}

// NopPublisher discards every push. It is the Publisher New installs
// so the analyze pipeline can always call s.pub without nil checks;
// real consumers replace it via SetPublisher.
type NopPublisher struct{}

func (NopPublisher) PublishDiagnostics(context.Context, uri.URI, uint32, []protocol.Diagnostic) {}
func (NopPublisher) ShowMessage(context.Context, protocol.MessageType, string)                  {}
func (NopPublisher) SecretRangesChanged(context.Context, uri.URI, int32, []SecretRange)         {}
func (NopPublisher) DatasetRegionsChanged(context.Context, uri.URI, int32, []DatasetRegion)     {}
func (NopPublisher) EffectiveConfigChanged(context.Context, string, map[string]ConfigValue)     {}
func (NopPublisher) LookupChanged(context.Context, string)                                      {}

// SetPublisher installs the outbound seam every server→client push
// flows through. A nil p resets to the no-op publisher. Call before
// the first document lands — the pipeline reads the field without a
// lock (same discipline the LSP shell followed when it assigned the
// field directly at construction time).
func (s *Workspace) SetPublisher(p Publisher) {
	if p == nil {
		p = NopPublisher{}
	}
	s.pub = p
}

// Publisher returns the installed outbound seam.
func (s *Workspace) Publisher() Publisher { return s.pub }

// Logger returns the Workspace's structured logger — the one New
// received (or slog.Default). The LSP shell logs dispatch-level
// events through the same logger so a session's output interleaves
// coherently.
func (s *Workspace) Logger() *slog.Logger { return s.log }

// lookupEnv is the `${env.X}` resolver this Workspace passes to
// workspaceconfig.ParseWithEnv at every config parse. There is no
// process-global SetEnvLookup installation, so two Workspaces in one
// process do not share a resolver. Reads the Workspace's
// bwSession override under RLock; falls back to os.Getenv when no
// override is set or when the requested var is something other
// than BW_SESSION.
//
// Scoping the override to BW_SESSION specifically (rather than
// shadowing ALL env reads) keeps the surface narrow: pxfed only
// exposes the bw session as user-controllable runtime state.
// Future env knobs (1Password tokens, vault sessions) would add
// their own branches here.
func (s *Workspace) lookupEnv(name string) string {
	if name == "BW_SESSION" {
		s.bwSessionMu.RLock()
		v := s.bwSession
		s.bwSessionMu.RUnlock()
		if v != "" {
			return v
		}
	}
	return os.Getenv(name)
}

// SetBwSession updates the workspace-side BW_SESSION override and
// triggers a reanalyze of every open document so cached config
// values that previously resolved `${env.BW_SESSION}` to the
// empty string (or to a stale token) re-evaluate with the new
// value. Called from the pxfed/setBwSession notification handler
// (LSP) or directly by in-process embedders.
func (s *Workspace) SetBwSession(ctx context.Context, token string) {
	s.bwSessionMu.Lock()
	s.bwSession = token
	s.bwSessionMu.Unlock()
	// Invalidate the lookup-manager baseline BEFORE the reload.
	// RebuildLookupFromEffectiveConfig diff-skips when the
	// registry config hasn't changed, but a BW_SESSION change
	// can flip the outcome of a previously-failed dial WITHOUT
	// touching the config itself (the user's registry server
	// may have become reachable / authentic between the failed
	// initial dial and now). Without the invalidation, a sticky
	// failed-lookup state survives the reload — hasUserLookup()
	// keeps returning false, validate() keeps bailing, and the
	// downstream features the user is trying to recover (secret
	// detection, schema-driven go-to-def) stay broken.
	s.lookupMgr.invalidateBaseline()
	// Workspace configs may have ${env.BW_SESSION} references in
	// chain spec paths, schema FQNs, proto_files paths, etc.
	// They were parsed once at the prior didOpen with the old
	// (empty / stale) value; reload them now so cached refs
	// pick up the new token. Empty workspaceDir routes through
	// the root-workspace reload path, which rebuilds the proto
	// files lookup, schema lookup (now uncached), and chain —
	// same surface an external .protolsp.pxf edit would touch.
	s.ReloadAfterConfigEdit(ctx, "")
}

// CancelBackground stops the pending watched-files debounce timer
// and every pending per-document analyze timer. First half of
// teardown — the LSP shell calls it before stopping its RPC sidecar
// so no timer fires into a server that is going away, then finishes
// with Close.
func (s *Workspace) CancelBackground() {
	// Stop any pending watched-files debounce timer so its
	// callback doesn't fire against a torn-down workspace. The
	// associated pending events get dropped — by the time
	// teardown runs the session is over anyway.
	s.watchedFilesMu.Lock()
	if s.watchedFilesTimer != nil {
		s.watchedFilesTimer.Stop()
		s.watchedFilesTimer = nil
		s.watchedFilesPending = nil
	}
	s.watchedFilesMu.Unlock()
	// Stop any pending per-document analyze timers (debounce window
	// in flight when teardown arrived). Without this a fired timer
	// would publish diagnostics through an about-to-close seam
	// and reanalyze documents whose workspace state is being torn
	// down.
	for _, u := range s.docs.uris() {
		s.CancelPendingAnalyze(s.docs.Get(u))
	}
}

// Close tears down sub-workspace lookups and the schema-lookup
// manager. Safe to call multiple times; safe to call when no lookup
// was configured. Callers that run background timers should call
// CancelBackground first (the LSP shell does, with its RPC-sidecar
// shutdown in between).
func (s *Workspace) Close() error {
	// Tear down sub-workspace lookups (W4b follow-up). Snapshot
	// the map under lock to avoid holding workspacesMu while we
	// close registry connections — closers may make their own
	// goroutines / I/O. Sub-workspace state itself stays in the
	// map (cheap) so any concurrent reader observes a consistent
	// "closed but present" entry rather than a missing one.
	s.ws.workspacesMu.Lock()
	subWorkspaces := make([]*WorkspaceState, 0, len(s.ws.subWorkspaces))
	for _, ws := range s.ws.subWorkspaces {
		subWorkspaces = append(subWorkspaces, ws)
	}
	s.ws.workspacesMu.Unlock()
	for _, ws := range subWorkspaces {
		s.tearDownSubWorkspace(ws)
	}

	// lookupMgr.Close cancels any in-flight rebuild (so its
	// goroutine doesn't try to install into a torn-down workspace)
	// AND closes the current lookup. Single call, both
	// teardown responsibilities encapsulated in the manager.
	err := s.lookupMgr.Close()

	// Release the protocheck validator's pooled VMs AFTER the
	// lookup teardown — its onChanged may fire one last rebuild,
	// and closing afterward guarantees the final validator is the
	// one released.
	s.closeProtocheck()
	return err
}

// Documents exposes the document store — the entry point embedders
// use to feed content in and tests use to reach per-document state.
func (s *Workspace) Documents() *Documents { return s.docs }

// Lookups exposes the schema-lookup manager. Tests install fakes via
// Lookups().Swap(fake, nil) — same shape as production; embedders can
// snapshot the active lookup for their own schema browsing.
func (s *Workspace) Lookups() *LookupManager { return s.lookupMgr }

// Config exposes the Tier 3/4 config-state owner (editor settings,
// session overrides, client-capability snapshot).
func (s *Workspace) Config() *ConfigState { return s.cfg }

// Workspaces exposes the workspace manager: the root workspace's
// fileConfig + chain plus the sub-workspace map.
func (s *Workspace) Workspaces() *WorkspaceManager { return s.ws }

// ClientCapabilities returns the client-capability snapshot captured
// at initialize time (zero value for in-process embedders).
func (s *Workspace) ClientCapabilities() ClientCapabilities { return s.cfg.clientCaps }

// SetClientCapabilities records the client-capability snapshot. The
// LSP shell calls it once at initialize before any concurrent
// handler can race with it; embedders leave the zero value.
func (s *Workspace) SetClientCapabilities(caps ClientCapabilities) { s.cfg.clientCaps = caps }

// PositionEncoding returns the negotiated position encoding every
// position↔byte conversion consults. UTF-16 zero value matches the
// LSP default.
func (s *Workspace) PositionEncoding() positions.Encoding { return s.positionEncoding }

// SetPositionEncoding records the negotiated position encoding. Set
// once at initialize time (LSP) or construction time (embedders that
// index with UTF-8 offsets).
func (s *Workspace) SetPositionEncoding(enc positions.Encoding) { s.positionEncoding = enc }

// SetDebounceWindow sets the didChange coalescing window. 0 (the
// default) runs the analyze pipeline synchronously inside each
// change; > 0 defers analyze + publish onto a timer that resets on
// each subsequent edit.
func (s *Workspace) SetDebounceWindow(d time.Duration) { s.debounceWindow = d }

// SetWatchedFilesWindow sets the watched-files coalescing window.
// Tests set 0 to keep dispatch synchronous; production keeps the
// default (200ms).
func (s *Workspace) SetWatchedFilesWindow(d time.Duration) { s.watchedFilesWindow = d }

// SetMaxReferencesFiles overrides the file-count cap used by
// references walks. 0 restores the default (10000). Tests use a
// small number to exercise the truncation path without writing 10k
// files to disk.
func (s *Workspace) SetMaxReferencesFiles(n int) { s.maxReferencesFiles = n }

// SetHugeRegionBytes overrides the byte-size threshold above which
// a @dataset region is marked Huge. 0 restores the default (100 KB).
func (s *Workspace) SetHugeRegionBytes(n int) { s.hugeRegionBytes = n }

// SetHugeRegionRows overrides the row-count threshold above which a
// @dataset region is marked Huge. 0 restores the default (1000).
func (s *Workspace) SetHugeRegionRows(n int) { s.hugeRegionRows = n }

// DebounceWindow reports the didChange coalescing window.
func (s *Workspace) DebounceWindow() time.Duration { return s.debounceWindow }

// WatchedFilesDebounce reports whether the watched-files debounce
// timer is armed and how many file events are buffered behind it.
// Test surface: teardown asserts both go to zero after Close.
func (s *Workspace) WatchedFilesDebounce() (timerArmed bool, pending int) {
	s.watchedFilesMu.Lock()
	defer s.watchedFilesMu.Unlock()
	return s.watchedFilesTimer != nil, len(s.watchedFilesPending)
}

// SetBufLint gates the buf-lint pass in the proto analyze path.
// New() enables it; tests flip it off so fixture files don't spin up
// buf subprocesses (and don't produce surprise lint diagnostics for
// files that weren't designed to pass buf's default ruleset).
func (s *Workspace) SetBufLint(enabled bool) { s.bufLintEnabled = enabled }

// SetProtoIndexCacheRoot overrides the BSR-cache root the proto
// import-path index resolves module deps from. Empty means the
// production default (bufconfig.ModuleCacheDir()). Tests point it at
// a staged fake cache.
func (s *Workspace) SetProtoIndexCacheRoot(root string) { s.protoIndexCacheRoot = root }

// AnalyzeCount reports completed analyze-pipeline runs (analyze +
// publish + notify). Tests use it to wait out a debounce window
// without sleeping.
func (s *Workspace) AnalyzeCount() int64 { return s.analyzeCount.Load() }

// RootProtoFilesErrs exposes the cached proto_files compile-error
// state for the root workspace. Test-surface accessor.
func (s *Workspace) RootProtoFilesErrs() *rootProtoFilesCompileErrors { return s.rootProtoFilesErrs }
