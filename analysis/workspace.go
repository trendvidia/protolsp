// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Per-document workspace discovery + per-workspace state (W4 of
// the workspace-config design). Monorepos place a `.protolsp.pxf`
// in each sub-project; the LSP must route each open document to
// the chain-aware features of its NEAREST-ancestor config rather
// than the workspace-root one.
//
// Two pieces live here:
//
//   1. **Discovery.** FindWorkspaceDirFor walks up from a doc's
//      filesystem path looking for `.protolsp.pxf`, stopping at
//      the LSP-reported workspace root. The result caches on
//      Document.WorkspaceDir at didOpen so we don't restat the
//      filesystem on every analyze.
//
//   2. **Per-workspace state.** Server.subWorkspaces holds the
//      lazily-loaded fileConfig + chain for each non-root
//      workspace we've discovered. The ROOT workspace's state
//      stays on Server directly (s.ws.rootFileConfig / s.chain) so the
//      single-workspace path is unchanged.
//
// W4b (per-workspace schema lookups) shipped alongside the
// discovery walk: WorkspaceState owns its own Lookup +
// LookupCloser + rebuild bookkeeping, dialed independently of the
// root via rebuildLookupAsyncForWorkspace. Sub-workspaces that
// don't declare their own registry fall through to the root at
// resolution time (lookupForDoc). W2 added a parallel ProtoFiles
// slot for the proto_files schema source.
//
// Teardown-on-last-doc-close also landed (R4 of the audit
// roadmap) via MaybeEvictSubWorkspace.

package analysis

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/trendvidia/protolsp/chain"
	"github.com/trendvidia/protolsp/schema"
	"github.com/trendvidia/protolsp/workspaceconfig"
)

// WorkspaceState bundles the per-workspace state W4 splits out
// of Server. The root workspace's state lives on Server directly
// (s.ws.rootFileConfig / s.chain) — this type holds the same shape for
// each sub-workspace discovered via `.protolsp.pxf` ancestry.
type WorkspaceState struct {
	// Dir is the absolute path of the directory containing this
	// workspace's `.protolsp.pxf`. Used as the map key in
	// Server.subWorkspaces and stored on Document.WorkspaceDir
	// so subsequent operations route to the right workspace.
	Dir string

	// FileConfig is the parsed workspace config. Loaded lazily
	// the first time a doc maps to this dir. Nil when the
	// `.protolsp.pxf` was unparseable or empty.
	FileConfig *workspaceconfig.Config

	// Chain is the loaded chameleon chain spec when FileConfig
	// declares a chameleon block. Nil otherwise / on load failure.
	Chain *chain.ActiveChain

	// Lookup is the user-registry-backed schema.Lookup for this
	// sub-workspace (W4b of workspace-config). Mirrors the
	// machinery on Server for the root workspace; lets sub-
	// workspaces dial DIFFERENT registries than the root when
	// their `.protolsp.pxf` declares its own schema_sources.
	// Nil when the sub-workspace doesn't declare a registry —
	// callers fall through to the root lookup in that case.
	Lookup       schema.Lookup
	LookupCloser io.Closer

	// ProtoFiles is the W2 proto_files-backed schema.Lookup for
	// this sub-workspace, populated when the sub's `.protolsp.pxf`
	// declares one or more `proto_files { ... }` blocks. Sits
	// alongside Lookup the same way LookupManager.protoFiles does
	// on the root; W3 will compose the two via WithFallback.
	ProtoFiles       schema.Lookup
	ProtoFilesCloser io.Closer

	// CompileErrors captures any protocompile errors raised while
	// building ProtoFiles. Surfaced as diagnostics on the
	// sub-workspace's `.protolsp.pxf` by config_diagnostics.go.
	CompileErrors []schema.CompileError

	lookupMu sync.RWMutex

	// lastRebuiltCfg + rebuildMu + rebuildCancel mirror the
	// server-wide fields. Each sub-workspace runs its OWN
	// async rebuild keyed on its OWN effective registry config,
	// so a flurry of edits to one sub-workspace's .protolsp.pxf
	// doesn't cancel the root rebuild (or vice versa).
	// Unexported rebuild bookkeeping (#179); observers use
	// LastRebuiltConfig / RebuildInFlight.
	lastRebuiltCfg schema.Config
	rebuildMu      sync.Mutex
	rebuildCancel  context.CancelFunc
}

// LastRebuiltConfig returns the effective registry config this
// sub-workspace's lookup was last rebuilt against (zero value when
// no rebuild has completed yet). Concurrent-safe.
func (ws *WorkspaceState) LastRebuiltConfig() schema.Config {
	ws.rebuildMu.Lock()
	defer ws.rebuildMu.Unlock()
	return ws.lastRebuiltCfg
}

// RebuildInFlight reports whether an async lookup rebuild is
// currently running for this sub-workspace. Concurrent-safe.
func (ws *WorkspaceState) RebuildInFlight() bool {
	ws.rebuildMu.Lock()
	defer ws.rebuildMu.Unlock()
	return ws.rebuildCancel != nil
}

// SubWorkspaceCount returns the number of lazily-loaded
// sub-workspaces. Concurrent-safe. Pairs with SubWorkspaceByDir as
// the sanctioned view of the sub-workspace map (#179).
func (s *Workspace) SubWorkspaceCount() int {
	s.ws.workspacesMu.Lock()
	defer s.ws.workspacesMu.Unlock()
	return len(s.ws.subWorkspaces)
}

// FindWorkspaceDirFor walks up from docPath looking for the
// nearest directory containing `.protolsp.pxf`. Walks stop at
// workspaceRoot — we never look at config files outside what the
// editor told us is the workspace.
//
// Returns workspaceRoot when no `.protolsp.pxf` is found in the
// ancestry (including the case where one exists at the root,
// since that's still the root workspace). The discovery is
// monotonic: a deeper `.protolsp.pxf` always wins over a
// shallower one.
//
// Non-file docs (URIs without a local filesystem mapping) yield
// ("", false) — those documents have no workspace context.
func FindWorkspaceDirFor(docPath, workspaceRoot string) (string, bool) {
	if docPath == "" {
		return "", false
	}
	docPath = filepath.Clean(docPath)
	if workspaceRoot != "" {
		workspaceRoot = filepath.Clean(workspaceRoot)
	}
	dir := filepath.Dir(docPath)
	for {
		candidate := filepath.Join(dir, workspaceconfig.ConfigFileName)
		if _, err := os.Stat(candidate); err == nil {
			return dir, true
		}
		// Don't walk past the workspace root.
		if workspaceRoot != "" && dir == workspaceRoot {
			return workspaceRoot, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root without finding either a
			// `.protolsp.pxf` or the workspace root. Fall back to
			// workspaceRoot when we have it; otherwise signal "no
			// workspace context."
			if workspaceRoot != "" {
				return workspaceRoot, true
			}
			return "", false
		}
		dir = parent
	}
}

// EnsureWorkspaceForDoc populates the doc's workspace directory if
// not already set and lazily loads the sub-workspace state when the
// doc belongs to one. Safe to call multiple times; second + calls
// short-circuit on the cached WorkspaceDir. Safe to call
// concurrently (#187): the check-then-set below isn't atomic, but
// racing callers compute the same wsDir from the same inputs, and
// EnsureSubWorkspace serializes internally on workspacesMu — the
// worst case is a redundant idempotent store.
//
// Caller must hold no locks. This function takes
// s.ws.workspacesMu via ensureSubWorkspace if a sub-workspace load
// is needed.
func (s *Workspace) EnsureWorkspaceForDoc(doc *Document) {
	if doc == nil || doc.WorkspaceDir() != "" {
		return
	}
	docPath, ok := URIToFilePath(doc.URI)
	if !ok {
		return
	}
	rootPath, _ := URIToFilePath(s.ws.workspaceRoot)
	wsDir, ok := FindWorkspaceDirFor(docPath, rootPath)
	if !ok {
		return
	}
	doc.SetWorkspaceDir(wsDir)
	// Sub-workspace? Lazy-load.
	if wsDir != rootPath {
		s.EnsureSubWorkspace(wsDir)
	}
}

// EnsureSubWorkspace returns the loaded state for the given
// sub-workspace directory, creating it if needed. The first call
// reads `.protolsp.pxf` from dir + loads the chain it points at
// (if any); subsequent calls return the cached state.
//
// dir MUST be an absolute path that's NOT equal to the root
// workspace dir (root state lives on Server directly).
//
// Concurrency: holds workspacesMu across the load. The OLD
// pattern (publish placeholder → load on the published pointer
// outside the lock) let a concurrent subWorkspaceForDoc reader
// observe a half-loaded struct (FileConfig set, Chain not yet,
// etc.). Holding the lock means concurrent ensureSubWorkspace /
// SubWorkspaceForDoc callers serialize through it, but
// sub-workspace discovery only happens on the FIRST doc to
// touch each dir — once per workspace per session — so the
// added contention is negligible and the leaked-dial-on-race
// hazard a load-then-publish variant would introduce isn't
// worth the optimization.
//
// loadSubWorkspaceState calls into:
//   - configContentForLoad (Documents.mu — distinct mutex, OK)
//   - rebuildLookupAsyncForWorkspace (per-workspace
//     ws.rebuildMu + spawns a goroutine; the goroutine path
//     never re-acquires workspacesMu)
//   - chain.ParseAt (pure)
//
// so the held mutex doesn't risk lock-order issues.
func (s *Workspace) EnsureSubWorkspace(dir string) *WorkspaceState {
	s.ws.workspacesMu.Lock()
	defer s.ws.workspacesMu.Unlock()
	if sw, ok := s.ws.subWorkspaces[dir]; ok {
		return sw
	}
	if s.ws.subWorkspaces == nil {
		s.ws.subWorkspaces = map[string]*WorkspaceState{}
	}
	sw := &WorkspaceState{Dir: dir}
	s.loadSubWorkspaceState(sw)
	s.ws.subWorkspaces[dir] = sw
	return sw
}

// tearDownSubWorkspace cancels any in-flight rebuild on a
// WorkspaceState and closes its registry lookup. Caller must
// NOT hold workspacesMu (the lookup close call may take its
// own time). Shared between Server.Close (drains every
// workspace at shutdown) and MaybeEvictSubWorkspace (drops a
// single idle workspace at didClose).
func (s *Workspace) tearDownSubWorkspace(ws *WorkspaceState) {
	if ws == nil {
		return
	}
	ws.rebuildMu.Lock()
	if ws.rebuildCancel != nil {
		ws.rebuildCancel()
		ws.rebuildCancel = nil
	}
	ws.rebuildMu.Unlock()
	if old := s.SwapSubWorkspaceLookup(ws, nil, nil); old != nil {
		if err := old.Close(); err != nil {
			s.log.Warn("closing sub-workspace lookup",
				slog.String("dir", ws.Dir),
				slog.String("err", err.Error()))
		}
	}
	if old := s.swapSubWorkspaceProtoFiles(ws, nil, nil); old != nil {
		if err := old.Close(); err != nil {
			s.log.Warn("closing sub-workspace proto_files lookup",
				slog.String("dir", ws.Dir),
				slog.String("err", err.Error()))
		}
	}
}

// MaybeEvictSubWorkspace removes the sub-workspace entry for
// dir IF no remaining open document maps to it. Called from
// dispatchDidClose after the closing doc has been removed from
// the store. R4 of the audit roadmap.
//
// Eviction is exact (no idle timer or LRU): a sub-workspace
// stays loaded as long as ANY of its docs is open, and gets
// torn down the moment the last one closes. Re-opening a doc
// inside it triggers a fresh discover-and-load via
// ensureSubWorkspace.
//
// Root workspaces are never evicted — their state lives on
// Server directly (s.ws.rootFileConfig / s.chain), not in
// s.ws.subWorkspaces, and we keep them for the session.
//
// Concurrency: lock-order is Documents.mu (via URIs/Get) →
// workspacesMu. Multiple concurrent evictions for the same
// dir are safe; the loser finds the map entry already deleted
// and no-ops.
func (s *Workspace) MaybeEvictSubWorkspace(dir string) {
	if s == nil || dir == "" {
		return
	}
	rootPath, _ := URIToFilePath(s.ws.workspaceRoot)
	if dir == rootPath {
		return
	}

	for _, u := range s.docs.uris() {
		d := s.docs.Get(u)
		if d != nil && d.WorkspaceDir() == dir {
			return
		}
	}

	s.ws.workspacesMu.Lock()
	ws, ok := s.ws.subWorkspaces[dir]
	if !ok {
		s.ws.workspacesMu.Unlock()
		return
	}
	delete(s.ws.subWorkspaces, dir)
	s.ws.workspacesMu.Unlock()

	s.tearDownSubWorkspace(ws)
	s.log.Debug("evicted idle sub-workspace",
		slog.String("dir", dir))
}

// loadSubWorkspaceState fills sw.FileConfig + sw.Chain from disk
// (preferring open-doc state via configContentForLoad). Soft-
// failure throughout: anything that can't be loaded leaves the
// corresponding field nil and logs at warn level.
func (s *Workspace) loadSubWorkspaceState(sw *WorkspaceState) {
	configPath := filepath.Join(sw.Dir, workspaceconfig.ConfigFileName)
	content, ok := s.configContentForLoad(configPath)
	if !ok {
		return
	}
	cfg, warnings, err := workspaceconfig.ParseWithEnv(content, s.lookupEnv)
	if err != nil {
		s.log.Warn("sub-workspace .protolsp.pxf unparseable; continuing without",
			slog.String("path", configPath),
			slog.String("err", err.Error()))
		return
	}
	for _, w := range warnings {
		s.log.Warn("sub-workspace .protolsp.pxf warning",
			slog.String("path", configPath),
			slog.String("message", w.String()))
	}
	if cfg.HasContent {
		sw.FileConfig = cfg
	}
	// W2: proto_files sources first — compile is synchronous
	// (no network dial), and W3 needs the proto_files lookup
	// installed before the registry dial goroutine starts so the
	// dial reads a populated WithFallback parent.
	s.rebuildProtoFilesLookupForWorkspace(sw)
	// Per-workspace schema lookup (W4b). If this sub-workspace's
	// `.protolsp.pxf` declares its own registry, we dial it
	// independently of the root. Sub-workspaces that don't
	// declare a registry fall through to the root lookup at
	// resolution time (see lookupForDoc) — no rebuild fires
	// because effectiveRegistryConfigForWorkspace returns the
	// editor-settings-only config, which equals the root's
	// effective config and shares it implicitly.
	if sw.FileConfig != nil && !sw.FileConfig.Registry.IsEmpty() {
		s.rebuildLookupAsyncForWorkspace(context.Background(), sw)
	}
	// Chain spec, relative to sw.Dir (NOT the LSP workspace root).
	if sw.FileConfig == nil || sw.FileConfig.Chameleon.ChainSpec == "" {
		return
	}
	if !sw.FileConfig.Chameleon.Enabled {
		return
	}
	chainPath := sw.FileConfig.Chameleon.ChainSpec
	if !filepath.IsAbs(chainPath) {
		chainPath = filepath.Join(sw.Dir, chainPath)
	}
	chainContent, ok := s.configContentForLoad(chainPath)
	if !ok {
		return
	}
	ac, err := chain.ParseAt(chainPath, chainContent)
	if err != nil {
		s.log.Warn("sub-workspace chain spec unparseable; chain mode off",
			slog.String("path", chainPath),
			slog.String("err", err.Error()))
		return
	}
	sw.Chain = ac
	s.log.Info("sub-workspace loaded",
		slog.String("dir", sw.Dir),
		slog.Int("environments", len(ac.Environments)),
		slog.Int("layers", len(ac.Layers)))
}

// SubWorkspaceForDoc returns the loaded sub-workspace state for
// doc, or nil when doc belongs to the root workspace (or has no
// workspace mapping at all). Callers use the accessors
// fileConfigForDoc / chainForDoc / resolvedChainSpecPathForDoc
// rather than this helper directly.
func (s *Workspace) SubWorkspaceForDoc(doc *Document) *WorkspaceState {
	if doc == nil {
		return nil
	}
	return s.subWorkspaceForDir(doc.WorkspaceDir())
}

// subWorkspaceForDir is the directory-keyed core behind
// SubWorkspaceForDoc: it returns the loaded sub-workspace state for
// an absolute workspace directory, or nil for the empty dir, the
// root dir, or a directory that isn't a registered sub-workspace.
//
// The registry:// resolution paths (#215) route by a workspaceDir
// carried on the request or on the preview's shadow Document rather
// than by a Document value, so they call this directly.
func (s *Workspace) subWorkspaceForDir(dir string) *WorkspaceState {
	if dir == "" {
		return nil
	}
	rootPath, _ := URIToFilePath(s.ws.workspaceRoot)
	if dir == rootPath {
		return nil
	}
	s.ws.workspacesMu.Lock()
	defer s.ws.workspacesMu.Unlock()
	return s.ws.subWorkspaces[dir]
}

// effectiveRegistryConfigForWorkspace merges a sub-workspace's
// fileConfig.Registry with the server-wide editor settings
// (Tier 3) the same way the root effectiveRegistryConfig does
// — Tier 3 wins; Tier 2 (this workspace's file) fills gaps.
//
// Sub-workspaces inherit editor settings from the LSP session
// (one set of registry settings, namespace, token, cache dir
// per LSP session). Tier 2 (the file) is per-workspace; this is
// the only piece that differs from the root computation.
func (s *Workspace) effectiveRegistryConfigForWorkspace(ws *WorkspaceState) schema.Config {
	var file schema.Config
	if ws != nil && ws.FileConfig != nil {
		file = registrySourceToConfig(ws.FileConfig.Registry)
	}
	return mergeRegistryConfig(s.SnapshotEditorConfig().Registry, file)
}

// snapshotSubWorkspaceLookup returns the current lookup for the
// given WorkspaceState, RLock-protected. Callers use the
// composite lookupForDoc rather than this helper directly.
func (s *Workspace) snapshotSubWorkspaceLookup(ws *WorkspaceState) schema.Lookup {
	if ws == nil {
		return nil
	}
	ws.lookupMu.RLock()
	defer ws.lookupMu.RUnlock()
	return ws.Lookup
}

// SwapSubWorkspaceLookup installs new lookup state on ws and
// returns the prior closer for the caller to close outside the
// lock. Mirrors the server-wide swapLookup, including the
// LookupChanged fire (#209) — sub-workspace slots don't route
// through LookupManager, so the seam is fired here.
func (s *Workspace) SwapSubWorkspaceLookup(ws *WorkspaceState, newLookup schema.Lookup, newCloser io.Closer) io.Closer {
	if ws == nil {
		return nil
	}
	ws.lookupMu.Lock()
	oldLookup := ws.Lookup
	old := ws.LookupCloser
	ws.Lookup = newLookup
	ws.LookupCloser = newCloser
	ws.lookupMu.Unlock()
	s.notifyLookupChanged(oldLookup, newLookup, ws.Dir)
	return old
}

// notifyLookupChanged is the sub-workspace twin of LookupManager's
// notifyChanged: fire the Publisher's LookupChanged seam for an
// actual install or teardown on ws's slots, silently skipping
// nil→nil swaps (e.g. tearing down a sub-workspace that never
// declared its own schema source). Runs outside ws.lookupMu so the
// callback can re-read lookup state freely.
//
// It also drops the cross-file registry AST cache, mirroring the root
// manager's onChanged (see the newLookupManager closure): now that a
// sub-workspace's registry backs registry:// resolution (#215), a
// swap on its slot can change what an import path resolves to, so a
// parse cached under this dir must not leak into resolution against
// the new lookup. clearRegistryASTCache drops the whole cache, so a
// single call covers every dir.
func (s *Workspace) notifyLookupChanged(oldLookup, newLookup schema.Lookup, dir string) {
	if s == nil {
		return
	}
	if oldLookup == nil && newLookup == nil {
		return
	}
	s.clearRegistryASTCache()
	s.pub.LookupChanged(context.Background(), dir)
}

// snapshotSubWorkspaceProtoFiles returns the proto_files-backed
// lookup for ws (W2), RLock-protected. Independent of the
// registry-backed Lookup so callers (lookupForDoc + the future W3
// composition path) can read each slot without conflating them.
func (s *Workspace) snapshotSubWorkspaceProtoFiles(ws *WorkspaceState) schema.Lookup {
	if ws == nil {
		return nil
	}
	ws.lookupMu.RLock()
	defer ws.lookupMu.RUnlock()
	return ws.ProtoFiles
}

// swapSubWorkspaceProtoFiles installs new proto_files lookup state
// on ws and returns the prior closer for the caller to close
// outside the lock. Mirrors swapSubWorkspaceLookup for the W2 slot,
// LookupChanged fire included — the proto_files slot serves
// LookupForDoc's per-workspace routing.
func (s *Workspace) swapSubWorkspaceProtoFiles(ws *WorkspaceState, newLookup schema.Lookup, newCloser io.Closer) io.Closer {
	if ws == nil {
		return nil
	}
	ws.lookupMu.Lock()
	oldLookup := ws.ProtoFiles
	old := ws.ProtoFilesCloser
	ws.ProtoFiles = newLookup
	ws.ProtoFilesCloser = newCloser
	ws.lookupMu.Unlock()
	s.notifyLookupChanged(oldLookup, newLookup, ws.Dir)
	return old
}

// rebuildLookupAsyncForWorkspace is the sub-workspace twin of
// Server.RebuildLookupAsync: diff against ws.lastRebuiltCfg,
// dial in a goroutine on config change, cancel any in-flight
// rebuild via ws.rebuildCancel. Independent state per
// workspace — a rebuild on svc-a doesn't interfere with svc-b.
//
// Empty Address tears down ws.Lookup synchronously (no dial to
// schedule); the sub-workspace effectively switches to "no
// registry of its own; share the root."
func (s *Workspace) rebuildLookupAsyncForWorkspace(_ context.Context, ws *WorkspaceState) {
	if s == nil || ws == nil {
		return
	}
	cfg := s.effectiveRegistryConfigForWorkspace(ws)

	ws.rebuildMu.Lock()
	if cfg == ws.lastRebuiltCfg {
		ws.rebuildMu.Unlock()
		return
	}
	ws.lastRebuiltCfg = cfg
	if cfg.Address == "" {
		if ws.rebuildCancel != nil {
			ws.rebuildCancel()
			ws.rebuildCancel = nil
		}
		ws.rebuildMu.Unlock()
		if old := s.SwapSubWorkspaceLookup(ws, nil, nil); old != nil {
			if err := old.Close(); err != nil {
				s.log.Warn("closing previous sub-workspace schema lookup",
					slog.String("dir", ws.Dir),
					slog.String("err", err.Error()))
			}
		}
		return
	}
	if ws.rebuildCancel != nil {
		ws.rebuildCancel()
	}
	rebuildCtx, cancel := context.WithCancel(context.Background())
	ws.rebuildCancel = cancel
	ws.rebuildMu.Unlock()

	go s.dialAndInstallSubWorkspaceLookup(rebuildCtx, ws, cfg)
}

// dialAndInstallSubWorkspaceLookup is the background goroutine
// the sub-workspace rebuild kicks off. Mirrors
// Server.dialAndInstallLookup but installs onto a
// WorkspaceState instead of the server-wide fields. W3 plumbs
// the sub's compiled proto_files in as a WithFallback parent on
// the dial, same as the root path.
func (s *Workspace) dialAndInstallSubWorkspaceLookup(ctx context.Context, ws *WorkspaceState, cfg schema.Config) {
	var openOpts []schema.OpenOption
	if files, types := schema.ProtoFilesParents(s.snapshotSubWorkspaceProtoFiles(ws)); files != nil && types != nil {
		openOpts = append(openOpts, schema.WithFallback(files, types))
	}
	lookup, closer, err := schema.Open(ctx, cfg, openOpts...)
	if err != nil {
		// Keep the previous lookup, but still fire LookupChanged
		// (#209): a per-workspace status surface waiting on this
		// rebuild needs to know the attempt resolved — same
		// contract as the root's failed-dial path.
		s.log.Warn("background sub-workspace schema lookup rebuild failed; keeping previous",
			slog.String("dir", ws.Dir),
			slog.String("address", cfg.Address),
			slog.String("err", err.Error()))
		s.pub.LookupChanged(ctx, ws.Dir)
		return
	}
	if ctx.Err() != nil {
		if closer != nil {
			_ = closer.Close()
		}
		return
	}
	if old := s.SwapSubWorkspaceLookup(ws, lookup, closer); old != nil {
		if err := old.Close(); err != nil {
			s.log.Warn("closing previous sub-workspace schema lookup",
				slog.String("dir", ws.Dir),
				slog.String("err", err.Error()))
		}
	}
	s.log.Info("sub-workspace schema lookup rebuilt (async)",
		slog.String("dir", ws.Dir),
		slog.String("address", cfg.Address),
		slog.String("namespace", cfg.Namespace))
	s.reanalyzeOpenDocs(ctx)
}

// LookupForDoc returns the schema.Lookup that applies to doc.
// Routing logic:
//
//  1. If doc maps to a SUB-workspace AND that sub-workspace
//     declares its own schema source, return the sub's lookup
//     (registry-backed if present, else proto_files-backed).
//     The sub never falls back to the root once it's declared
//     a source — that's the per-workspace isolation guarantee
//     from the design.
//  2. Otherwise fall through to the server-wide root lookup
//     (lookupMgr.Snapshot, which already folds root.registry
//     and root.proto_files together).
//
// Returns nil when neither the sub-workspace nor the root has a
// configured lookup.
func (s *Workspace) LookupForDoc(doc *Document) schema.Lookup {
	if sw := s.SubWorkspaceForDoc(doc); sw != nil {
		if lk := s.snapshotSubWorkspaceLookup(sw); lk != nil {
			return lk
		}
		if lk := s.snapshotSubWorkspaceProtoFiles(sw); lk != nil {
			return lk
		}
	}
	return s.lookupMgr.Snapshot()
}

// protoFilesLookupForDoc returns the proto_files-backed lookup
// whose compile could have produced descriptors for doc. Mirrors
// LookupForDoc's routing: a sub-workspace that declares its own
// schema source never falls back to the root, so when the sub has
// a registry lookup but no proto_files block this returns nil
// rather than the root's proto_files (whose descriptors can't have
// backed this doc).
//
// Used by resolveDefinitionURI's exact-attribution step (#185).
// Callers need the raw proto_files slot — not the composed W3
// resolver — because only it retains the descriptor-path →
// disk-path mapping from the compile.
func (s *Workspace) protoFilesLookupForDoc(doc *Document) schema.Lookup {
	if sw := s.SubWorkspaceForDoc(doc); sw != nil {
		if pf := s.snapshotSubWorkspaceProtoFiles(sw); pf != nil {
			return pf
		}
		if s.snapshotSubWorkspaceLookup(sw) != nil {
			return nil
		}
	}
	return s.lookupMgr.SnapshotProtoFiles()
}

// ChainForDoc returns the chain spec that applies to doc.
func (s *Workspace) ChainForDoc(doc *Document) *chain.ActiveChain {
	if sw := s.SubWorkspaceForDoc(doc); sw != nil {
		return sw.Chain
	}
	return s.SnapshotChain()
}

// workspaceDirForDoc returns the absolute path of the directory
// doc's workspace lives in. Falls back to the LSP-reported
// workspace root when discovery hasn't run / didn't find one.
//
// Used by layer detection + path resolution against chain-spec
// templates — these need the workspace dir, not the LSP root,
// to correctly resolve relative paths inside a sub-workspace.
func (s *Workspace) workspaceDirForDoc(doc *Document) string {
	if doc != nil && doc.WorkspaceDir() != "" {
		return doc.WorkspaceDir()
	}
	root, _ := URIToFilePath(s.ws.workspaceRoot)
	return root
}

// rebuildProtoFilesLookupForWorkspace compiles the sub-workspace's
// `proto_files { ... }` sources (W2). Synchronous because
// protocompile is a pure-Go compile — no network. The previous
// proto_files lookup, if any, is closed after the new one is
// installed.
//
// CompileErrors are stashed on ws.CompileErrors so the
// config_diagnostics path can surface them on the sub's
// `.protolsp.pxf` the next time its diagnostics get computed.
//
// Empty / missing sources tear down any existing proto_files
// lookup — the sub effectively switches to "no local protos."
func (s *Workspace) rebuildProtoFilesLookupForWorkspace(ws *WorkspaceState) {
	if s == nil || ws == nil {
		return
	}
	cfg, hasContent := workspaceProtoFilesConfig(ws.Dir, ws.FileConfig)
	ws.lookupMu.Lock()
	ws.CompileErrors = nil
	ws.lookupMu.Unlock()
	if !hasContent {
		if old := s.swapSubWorkspaceProtoFiles(ws, nil, nil); old != nil {
			if err := old.Close(); err != nil {
				s.log.Warn("closing previous sub-workspace proto_files lookup",
					slog.String("dir", ws.Dir),
					slog.String("err", err.Error()))
			}
		}
		return
	}
	lookup, closer, compileErrs, err := schema.OpenProtoFiles(context.Background(), cfg, s.log)
	if err != nil {
		s.log.Warn("sub-workspace proto_files compile setup failed",
			slog.String("dir", ws.Dir),
			slog.String("err", err.Error()))
		return
	}
	ws.lookupMu.Lock()
	ws.CompileErrors = compileErrs
	ws.lookupMu.Unlock()
	if old := s.swapSubWorkspaceProtoFiles(ws, lookup, closer); old != nil {
		if err := old.Close(); err != nil {
			s.log.Warn("closing previous sub-workspace proto_files lookup",
				slog.String("dir", ws.Dir),
				slog.String("err", err.Error()))
		}
	}
	for _, ce := range compileErrs {
		s.log.Warn("sub-workspace proto_files compile error",
			slog.String("dir", ws.Dir),
			slog.String("error", ce.String()))
	}
	s.log.Info("sub-workspace proto_files lookup rebuilt",
		slog.String("dir", ws.Dir),
		slog.Int("compile_errors", len(compileErrs)))
}

// workspaceProtoFilesConfig builds a schema.ProtoFilesConfig from
// the workspace's parsed `proto_files { ... }` block, resolving
// any relative paths against the workspace directory. Returns
// hasContent=false when the workspace has no source to compile.
//
// schema.ProtoFilesConfig.Sources is still a slice — the schema
// package supports multi-source even though workspaceconfig
// produces at most one. The single source becomes a one-element
// slice here.
func workspaceProtoFilesConfig(workspaceDir string, fc *workspaceconfig.Config) (schema.ProtoFilesConfig, bool) {
	if fc == nil || fc.ProtoFiles.IsEmpty() {
		return schema.ProtoFilesConfig{}, false
	}
	src := fc.ProtoFiles
	resolved := make([]string, 0, len(src.Paths))
	for _, p := range src.Paths {
		if !filepath.IsAbs(p) && workspaceDir != "" {
			p = filepath.Join(workspaceDir, p)
		}
		resolved = append(resolved, p)
	}
	return schema.ProtoFilesConfig{
		Sources: []schema.ProtoFilesSource{{Paths: resolved}},
	}, true
}

// reloadSubWorkspace re-reads the sub-workspace's `.protolsp.pxf`
// + chain after a config edit or watcher event. The previous
// state's Lookup + ProtoFiles closers are released before the
// new state takes its place so a `.protolsp.pxf` change that
// rewires the registry address doesn't leak the old gRPC conn.
//
// Returns the post-reload state, or nil when dir isn't a known
// sub-workspace (e.g. event for a .protolsp.pxf we haven't
// discovered yet — a future didOpen will lazy-load it).
func (s *Workspace) reloadSubWorkspace(dir string) *WorkspaceState {
	s.ws.workspacesMu.Lock()
	old, ok := s.ws.subWorkspaces[dir]
	s.ws.workspacesMu.Unlock()
	if !ok {
		return nil
	}
	s.tearDownSubWorkspace(old)
	sw := &WorkspaceState{Dir: dir}
	s.loadSubWorkspaceState(sw)
	s.ws.workspacesMu.Lock()
	s.ws.subWorkspaces[dir] = sw
	s.ws.workspacesMu.Unlock()
	return sw
}
