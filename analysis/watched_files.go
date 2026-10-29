// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// W5 of the workspace-config design: react to file-system changes
// in .protolsp.pxf and the active chain spec so users can edit
// config / chain.pxf without an LSP restart.
//
// The flow:
//   1. Client → server: workspace/didChangeWatchedFiles with a
//      list of FileEvents (created / changed / deleted).
//   2. Server picks events that affect known config paths:
//      - workspace-root .protolsp.pxf
//      - the chain spec path resolved from the current fileConfig
//        (when one was set) OR the well-known `chain.pxf` filename
//        anywhere in the workspace (defensive — common convention)
//      - chain LAYER files (exact ResolvedPath match or containment
//        under a declared fs source root) — no reload needed, but
//        open leaves must re-analyze because their chain-aware
//        diagnostics read sibling/ancestor layers from disk
//   3. For matching events: reload the corresponding state
//      (fileConfig, chain) and re-analyze every open document so
//      diagnostics + LayerInfo reflect the new world.
//
// Soft semantics throughout: an event for an unrelated PXF file
// is ignored; a delete event leaves the affected state nil and the
// next load reconstructs from disk; reload failures log + leave
// the cleared state (server keeps running in degraded mode).

package analysis

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/chain"
)

// ScheduleWatchedFilesProcess either runs the watched-files
// pipeline immediately (window == 0, today's test-default
// behavior) or buffers the events and starts / resets a timer
// that drains the buffer when no further events arrive within
// the window.
//
// The buffer-and-drain approach is necessary because the
// concrete reload work depends on the union of all events that
// arrived during the window — a sequence of (config-change,
// chain-spec-change, proto-file-change) compresses into one
// pipeline run, not three.
//
// The deferred goroutine uses context.Background — the request
// context that fired the notification is dead by the time the
// timer fires.
func (s *Workspace) ScheduleWatchedFilesProcess(ctx context.Context, events []*protocol.FileEvent) {
	if s.watchedFilesWindow <= 0 {
		s.processWatchedFiles(ctx, events)
		return
	}
	s.watchedFilesMu.Lock()
	defer s.watchedFilesMu.Unlock()
	s.watchedFilesPending = append(s.watchedFilesPending, events...)
	if s.watchedFilesTimer != nil {
		s.watchedFilesTimer.Stop()
	}
	s.watchedFilesTimer = time.AfterFunc(s.watchedFilesWindow, func() {
		s.watchedFilesMu.Lock()
		pending := s.watchedFilesPending
		s.watchedFilesPending = nil
		s.watchedFilesTimer = nil
		s.watchedFilesMu.Unlock()
		s.processWatchedFiles(context.Background(), pending)
	})
}

// processWatchedFiles is the actual classify → reload →
// reanalyze pipeline. Called either inline (window == 0) or from
// the watched-files debouncer goroutine.
func (s *Workspace) processWatchedFiles(ctx context.Context, events []*protocol.FileEvent) {
	if len(events) == 0 {
		return
	}
	cls := s.classifyWatchedEvents(events)
	if cls.reloadConfig {
		s.LoadWorkspaceFileConfig()
		// W2/W3: rebuild proto_files BEFORE the registry rebuild
		// so the dial goroutine sees the freshest proto_files
		// snapshot for its WithFallback wiring.
		s.RebuildRootProtoFilesLookup()
		s.RebuildLookupFromEffectiveConfig(ctx)
		// A change to .protolsp.pxf may flip the chameleon block on
		// or off, or point at a different chain spec — always reload
		// chain when config changed so derived state stays in sync.
		cls.reloadChain = true
		// Config edit already rebuilt proto_files above; skip the
		// dedicated rebuild below to avoid two compiles per event.
		cls.reloadProtoFiles = false
		// Proto-authoring phase 3: .protolsp.pxf changes can shift
		// the workspace root or rename a chameleon path that the
		// index might want to skip. Cheaper to rebuild the index
		// than to track the cross-cutting invariants.
		cls.reloadProtoIndex = true
	}
	if cls.reloadProtoFiles {
		// A .proto file under one of the configured proto_files
		// search paths changed. Recompile the lookup so hover /
		// completion / definition see the new schema.
		s.RebuildRootProtoFilesLookup()
		s.RebuildLookupFromEffectiveConfig(ctx)
	}
	if cls.reloadChain {
		s.LoadWorkspaceChain()
	}
	if cls.reloadProtoIndex {
		// Phase 3 step 5: live invalidation. A .proto change
		// somewhere under the index's include paths, or a
		// buf.yaml / buf.lock edit (which can rewrite the
		// include path set entirely), invalidates the cached
		// AST + file map. A full rebuild is cheap — discovery
		// is sub-millisecond for typical workspaces, parsing
		// is lazy. Older Index instances stay reachable only
		// through references already in flight (hover requests
		// that snapshotted before the swap); they GC when those
		// complete.
		s.LoadProtoIndex()
	}
	// W4b: sub-workspace events. Reload each affected sub-
	// workspace so its FileConfig + Chain + Lookup re-derive
	// from disk. Live-reload from didChange takes a different
	// path (it consults the open-doc buffer); these events
	// typically fire on save or external edits.
	for dir := range cls.subWorkspaceDirs {
		s.reloadSubWorkspace(dir)
	}
	if cls.reloadEngineConfig {
		// §9.4 engine config changed on disk — re-derive the
		// protocheck validator so data diagnostics reflect the new
		// function_libraries. Fingerprint-diffed internally; no-op
		// in the OSS build.
		s.RebuildProtocheckValidator(ctx)
	}
	if cls.reloadConfig || cls.reloadChain || cls.reloadProtoFiles || cls.reloadProtoIndex ||
		cls.reloadEngineConfig || cls.reanalyzeChainDocs || len(cls.subWorkspaceDirs) > 0 {
		s.reanalyzeOpenDocs(ctx)
	}
}

// watchedFilesClassification is the bundle of reload decisions
// classifyWatchedEvents derives from a batch of FileEvent inputs.
// Each bool flips true at most once per call; the caller branches
// on the union (multiple bools can be true simultaneously when a
// single batch carried events for both the config file and a
// .proto under proto_files, for example).
//
// Grew from a multi-return tuple to a struct when phase 3 step 5
// added reloadProtoIndex — past five returns the tuple was a maze
// at every call site.
type watchedFilesClassification struct {
	reloadConfig     bool
	reloadChain      bool
	reloadProtoFiles bool
	reloadProtoIndex bool
	// reloadEngineConfig flips on a protowire.config.textproto event
	// anywhere in the workspace — the §9.4 loader resolves the
	// NEAREST config walking up from the root, so any edit in the
	// tree may change the effective answer. The rebuild it triggers
	// is fingerprint-diffed, so over-matching costs one Load walk.
	reloadEngineConfig bool
	subWorkspaceDirs   map[string]struct{}

	// reanalyzeChainDocs flips when a batch touches a chain LAYER
	// file (not the spec): open leaves compute chain-aware
	// required-field diagnostics from the union of fields set
	// across their resolve-order siblings/ancestors READ FROM DISK,
	// so an on-disk edit to any layer can flip a leaf's diagnostics
	// without the leaf itself changing. Nothing needs
	// reloading — layer contents are read fresh at analyze time —
	// only the reanalyze fan-out was missing.
	reanalyzeChainDocs bool
}

// classifyWatchedEvents inspects the FileEvent list and returns
// what needs reloading. Unrelated events (anything outside the
// known config + chain + proto + buf paths) are dropped silently —
// clients vary in how literal they treat glob patterns, so we
// expect some noise and filter rather than registering narrowly.
//
// Multiple flags can flip true within a single call: a batch that
// contains both a .protolsp.pxf edit and a buf.lock update fires
// the schema-lookup rebuild AND the proto-index rebuild.
func (s *Workspace) classifyWatchedEvents(events []*protocol.FileEvent) watchedFilesClassification {
	cls := watchedFilesClassification{
		subWorkspaceDirs: map[string]struct{}{},
	}
	if len(events) == 0 {
		return cls
	}
	rootPath, hasRoot := URIToFilePath(s.ws.workspaceRoot)
	currentChainPath := s.activeChainPath()
	configuredChainPath := s.ResolvedChainSpecPath()
	// Snapshot the absolute proto_files search roots once so .proto
	// event matching doesn't re-resolve per event. Empty when the
	// workspace has no proto_files configured.
	protoFilesRoots := s.absoluteProtoFilesPaths(rootPath)
	// Phase 3 step 5: protoIndex include paths. Disjoint set from
	// proto_files (the W2/W3 schema lookup may cover only a subset
	// of what the proto-authoring index walks — e.g. BSR cache
	// dirs, or the naive-fallback workspace root). Resolved once
	// per batch.
	protoIndexRoots := s.absoluteProtoIndexPaths()
	// Chain LAYER files. One matcher per loaded chain (root +
	// sub-workspaces), each anchored at the workspace dir its
	// relative layer paths resolve against. nil when no chain is
	// loaded anywhere, so non-chameleon workspaces pay nothing.
	chainMatchers := s.chainLayerMatchers(rootPath, hasRoot)
	// §7 catalog files of the current protocheck validator; filled
	// on the first .textproto event of the batch.
	var catalogPaths []string
	for _, ev := range events {
		if ev == nil {
			continue
		}
		path, ok := URIToFilePath(ev.URI)
		if !ok {
			continue
		}
		base := filepath.Base(path)
		// .protolsp.pxf at the workspace root.
		if base == ".protolsp.pxf" && hasRoot && filepath.Dir(path) == rootPath {
			cls.reloadConfig = true
			continue
		}
		// .protolsp.pxf in a known sub-workspace dir. Looking it
		// up rather than discovering ad-hoc avoids resurrecting
		// every `.protolsp.pxf` under the workspace tree; a new
		// sub-workspace's config gets discovered by the FIRST
		// didOpen on a file inside it (workspace.go's
		// EnsureWorkspaceForDoc), not via watcher events alone.
		if base == ".protolsp.pxf" {
			if _, ok := s.SubWorkspaceByDir(filepath.Dir(path)); ok {
				cls.subWorkspaceDirs[filepath.Dir(path)] = struct{}{}
				continue
			}
		}
		// protowire.config.textproto — the §9.4 engine config.
		if base == "protowire.config.textproto" {
			cls.reloadEngineConfig = true
			continue
		}
		// A §7 catalog library file the current validator's
		// catalogs were merged from. Matched against the resolved
		// absolute paths (snapshotted lazily — most batches carry
		// no .textproto events) so unrelated textproto edits don't
		// trigger the engine-config cascade.
		if strings.HasSuffix(base, ".textproto") && !cls.reloadEngineConfig {
			if catalogPaths == nil {
				catalogPaths = s.protocheckCatalogPathsSnapshot()
			}
			for _, p := range catalogPaths {
				if p == path {
					cls.reloadEngineConfig = true
					break
				}
			}
			if cls.reloadEngineConfig {
				continue
			}
		}
		// buf.yaml / buf.lock at the workspace root — the protoIndex
		// derives its include paths from these, so a change here
		// can rewrite the entire workspace import-resolution map.
		// Full rebuild via reloadProtoIndex.
		if (base == "buf.yaml" || base == "buf.lock") && hasRoot && filepath.Dir(path) == rootPath {
			cls.reloadProtoIndex = true
			continue
		}
		// The root chain spec, either currently-loaded or the
		// path fileConfig points at (the spec might be created
		// after a load attempt failed).
		if currentChainPath != "" && path == currentChainPath {
			cls.reloadChain = true
			continue
		}
		if configuredChainPath != "" && path == configuredChainPath {
			cls.reloadChain = true
			continue
		}
		// Sub-workspace chain spec — find the sub-workspace whose
		// chain it is.
		if dir, ok := s.subWorkspaceDirForChainPath(path); ok {
			cls.subWorkspaceDirs[dir] = struct{}{}
			continue
		}
		// Chain LAYER file: an on-disk edit to any layer can
		// flip the chain-aware required-field diagnostics of open
		// LEAVES that never changed (they're computed from the union
		// of fields set across the resolve order, read from disk).
		// Match exactly against expanded ResolvedPaths, or — the
		// same safety net ComputeLayerInfo uses — containment under
		// a declared fs source root. One hit flags the whole batch;
		// remaining events skip the check.
		if !cls.reanalyzeChainDocs && len(chainMatchers) > 0 &&
			strings.HasSuffix(strings.ToLower(path), ".pxf") {
			for _, m := range chainMatchers {
				if s.matchesChainLayerFile(m, path) {
					cls.reanalyzeChainDocs = true
					break
				}
			}
			if cls.reanalyzeChainDocs {
				continue
			}
		}
		// .proto under one of the configured proto_files search
		// paths. Flipping reloadProtoFiles once is enough — the
		// caller does a single rebuild regardless of how many
		// proto events fired in this batch.
		if strings.HasSuffix(path, ".proto") {
			if !cls.reloadProtoFiles {
				for _, root := range protoFilesRoots {
					if pathIsUnder(path, root) {
						cls.reloadProtoFiles = true
						break
					}
				}
			}
			// Phase 3 step 5: a .proto under any of the proto-
			// authoring index's include paths invalidates the
			// cached entry's AST. We rebuild the index wholesale
			// rather than invalidating per-file — cheaper to
			// engineer, and discovery is sub-ms anyway.
			if !cls.reloadProtoIndex {
				for _, root := range protoIndexRoots {
					if pathIsUnder(path, root) {
						cls.reloadProtoIndex = true
						break
					}
				}
			}
		}
	}
	return cls
}

// absoluteProtoIndexPaths returns the protoIndex's current include
// paths. Used by the watched-files classifier to spot .proto edits
// that touch the cross-file resolver's source set even when they
// fall outside the schema lookup's proto_files search roots.
//
// Returns nil when no index has been built yet — the classifier
// then degrades to never flipping reloadProtoIndex on .proto
// events, which is the safe choice (we'd rebuild from scratch on
// the next index access anyway).
func (s *Workspace) absoluteProtoIndexPaths() []string {
	idx := s.SnapshotProtoIndex()
	if idx == nil {
		return nil
	}
	return idx.IncludePaths()
}

// absoluteProtoFilesPaths returns the configured proto_files search
// roots resolved to absolute filesystem paths. Empty when the
// workspace has no proto_files config or when the workspace root
// isn't a usable filesystem path (e.g. inmemory:// URIs from
// monaco-vscode-api defaults).
func (s *Workspace) absoluteProtoFilesPaths(workspaceRoot string) []string {
	if workspaceRoot == "" {
		return nil
	}
	fc := s.SnapshotFileConfig()
	if fc == nil {
		return nil
	}
	pfPaths := fc.ProtoFiles.Paths
	if len(pfPaths) == 0 {
		return nil
	}
	out := make([]string, 0, len(pfPaths))
	for _, p := range pfPaths {
		if p == "" {
			continue
		}
		if filepath.IsAbs(p) {
			out = append(out, filepath.Clean(p))
			continue
		}
		out = append(out, filepath.Clean(filepath.Join(workspaceRoot, p)))
	}
	return out
}

// chainLayerMatcher pairs a loaded chain with the absolute
// workspace dir its relative layer paths anchor to (the LSP root
// for the root chain, the sub-workspace's dir otherwise — same
// anchoring ComputeLayerInfo and chainSetFieldUnionForDoc use).
type chainLayerMatcher struct {
	dir string
	ac  *chain.ActiveChain
}

// chainLayerMatchers snapshots every loaded chain (root + sub-
// workspaces) for the watched-files classifier. Returns nil when
// no chain is loaded anywhere — the per-event layer-file check
// then short-circuits and non-chameleon workspaces pay nothing.
func (s *Workspace) chainLayerMatchers(rootPath string, hasRoot bool) []chainLayerMatcher {
	var out []chainLayerMatcher
	if hasRoot {
		if c := s.SnapshotChain(); c != nil {
			out = append(out, chainLayerMatcher{dir: rootPath, ac: c})
		}
	}
	s.ws.workspacesMu.Lock()
	for dir, ws := range s.ws.subWorkspaces {
		if ws.Chain != nil {
			out = append(out, chainLayerMatcher{dir: dir, ac: ws.Chain})
		}
	}
	s.ws.workspacesMu.Unlock()
	return out
}

// matchesChainLayerFile reports whether the absolute path is a
// layer file of the matcher's chain: an exact match against an
// expanded LayerInstance.ResolvedPath, or a file under a declared
// fs source's root (the same permissive containment
// ComputeLayerInfo falls back to, so the two classifiers agree on
// what counts as a layer).
func (s *Workspace) matchesChainLayerFile(m chainLayerMatcher, path string) bool {
	rel, err := filepath.Rel(m.dir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	relSlash := filepath.ToSlash(rel)
	for _, layer := range m.ac.Layers {
		if filepath.ToSlash(layer.ResolvedPath) == relSlash {
			return true
		}
	}
	return s.fsSourceContaining(m.ac, relSlash) != nil
}

// pathIsUnder reports whether `child` is `parent` or one of its
// descendants. Both arguments are expected to be filepath.Clean-d
// absolute paths. Used to test whether a watched-file event for
// `child` matches a configured proto_files search root.
func pathIsUnder(child, parent string) bool {
	if child == parent {
		return true
	}
	// Append separator before comparison so `/foo/bar` doesn't
	// accidentally claim `/foo/barbaz` as a descendant.
	prefix := parent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(child, prefix)
}

// SubWorkspaceByDir returns the loaded sub-workspace for a
// given directory if one exists. Used by the watcher event
// classifier to route .protolsp.pxf events to known
// sub-workspaces; we don't surface previously-unknown dirs to
// avoid loading every .protolsp.pxf in the workspace tree just
// because the editor watched its glob pattern. New
// sub-workspaces enter the map via EnsureWorkspaceForDoc when
// the first doc inside them opens.
func (s *Workspace) SubWorkspaceByDir(dir string) (*WorkspaceState, bool) {
	if s == nil {
		return nil, false
	}
	s.ws.workspacesMu.Lock()
	defer s.ws.workspacesMu.Unlock()
	ws, ok := s.ws.subWorkspaces[dir]
	return ws, ok
}

// subWorkspaceDirForChainPath returns the sub-workspace dir
// whose active chain spec is at the given path, if any. Returns
// "" / false when no sub-workspace owns this path. O(N) in the
// number of sub-workspaces; expected to be small.
func (s *Workspace) subWorkspaceDirForChainPath(path string) (string, bool) {
	if s == nil || path == "" {
		return "", false
	}
	s.ws.workspacesMu.Lock()
	defer s.ws.workspacesMu.Unlock()
	for dir, ws := range s.ws.subWorkspaces {
		if ws.Chain != nil && ws.Chain.Path == path {
			return dir, true
		}
	}
	return "", false
}

// activeChainPath returns the absolute path of the currently-
// loaded chain spec, or "" when no chain is loaded. Used by the
// event classifier to spot changes to the chain the LSP is
// actually using.
func (s *Workspace) activeChainPath() string {
	if s == nil {
		return ""
	}
	c := s.SnapshotChain()
	if c == nil {
		return ""
	}
	return c.Path
}

// reanalyzeOpenDocs walks every open document, refreshes its
// LayerInfo (the chain may have changed which means the file's
// classification may have changed), and re-runs the analyze
// pipeline so diagnostics + secret / dataset notifications
// reflect the new world.
//
// Uses the previous SecretRanges / DatasetRegions snapshots so
// the per-edit notification helpers (notifySecretRangesChanged,
// notifyDatasetRegionsChanged) can compare against the prior
// state — same contract the didChange / didOpen pipelines use.
func (s *Workspace) reanalyzeOpenDocs(ctx context.Context) {
	if s == nil || s.docs == nil {
		return
	}
	for _, u := range s.docs.uris() {
		doc := s.docs.Get(u)
		if doc == nil {
			continue
		}
		_, prevSecrets, prevDatasets := doc.SnapshotForPipeline()
		s.DetectLayerInfo(doc)
		s.runAnalyzePipeline(ctx, u, prevSecrets, prevDatasets)
	}
}
