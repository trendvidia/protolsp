// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Server-side wiring for the proto workspace index. Phase 3 step 3
// of the proto-authoring plan. Owns the lifecycle (build at
// initialize / rebuild on workspace-config reload) and the
// concurrency-safe snapshot accessor the hover + definition
// handlers call.

package analysis

import (
	"log/slog"

	"github.com/trendvidia/protolsp/bufconfig"
	"github.com/trendvidia/protolsp/protoindex"
)

// LoadProtoIndex (re)builds s.protoIndex from the current
// workspace root + buf.yaml / buf.lock. Soft-failure throughout:
// any error during bufconfig discovery degrades to a naive-fallback
// index (workspace root + proto/ subdir) rather than no index, so
// users in buf-less workspaces still get cross-file resolution for
// flat / proto-subdir layouts.
//
// Called from dispatchInitialize after the workspace root is set
// and from ReloadAfterConfigEdit so a .protolsp.pxf or buf.yaml
// edit reflects in the next hover / go-to-def.
func (s *Workspace) LoadProtoIndex() {
	if s == nil {
		return
	}
	rootPath, _ := URIToFilePath(s.ws.workspaceRoot)
	if rootPath == "" {
		// No workspace root → nothing to index. Leave any
		// previously-installed index in place (defensive — the
		// LSP shouldn't normally see an empty root after init).
		return
	}
	cfg, lock, err := bufconfig.FindBufYAML(rootPath)
	if err != nil {
		// buf.yaml or buf.lock present but malformed. Surface
		// at Warn so the user sees the parse error in the trace
		// panel; then fall through to building with cfg=nil,
		// which gives the naive-fallback include paths. That
		// trades "no resolution at all" for "best-effort
		// resolution against the workspace layout we can see."
		s.log.Warn("LoadProtoIndex: bufconfig parse failed",
			slog.String("workspace", rootPath),
			slog.String("err", err.Error()))
		cfg = nil
	}
	var idx *protoindex.Index
	if s.protoIndexCacheRoot != "" {
		// Test-only override: skip bufconfig.ModuleCacheDir() and
		// use the staged fake cache root. Empty falls through to
		// the production constructor.
		idx = protoindex.NewWithCacheRoot(rootPath, cfg, lock, s.protoIndexCacheRoot, s.log)
	} else {
		idx = protoindex.New(rootPath, cfg, lock, s.log)
	}
	s.protoIndexMu.Lock()
	s.protoIndex = idx
	s.protoIndexMu.Unlock()
}

// SnapshotProtoIndex returns the currently-installed index, or nil
// when LoadProtoIndex hasn't run yet. The returned pointer is safe
// to use without re-locking — the index itself is internally
// concurrent-safe, and we never mutate an installed Index (LoadProtoIndex
// swaps in a freshly built one).
func (s *Workspace) SnapshotProtoIndex() *protoindex.Index {
	if s == nil {
		return nil
	}
	s.protoIndexMu.RLock()
	defer s.protoIndexMu.RUnlock()
	return s.protoIndex
}
