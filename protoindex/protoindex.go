// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package protoindex maps `import "<path>"` strings to on-disk
// .proto files for a workspace. Step 2 of the proto-authoring
// phase 3 plan: the cross-file symbol resolver needs to ask "given
// this import path, where's the file?" and the index answers in O(1)
// after a one-time discovery walk.
//
// Include-path resolution order matches what buf does at build
// time:
//
//  1. Each buf.yaml module root (workspace-relative, joined with
//     the workspace root), in declaration order.
//  2. Each buf.lock BSR dep, mapped to its cached files directory
//     under `<UserCacheDir>/buf/v3/modules/<name>/<commit>/files/`.
//
// First include path wins on duplicate import keys — same precedence
// buf uses, so a local override of a remote file shadows the remote
// during resolution.
//
// Naive fallback (no buf.yaml present): the workspace root itself
// plus a `proto/` subdir become the include paths. Lets users edit
// .proto in unconfigured workspaces without losing cross-file
// navigation. Per the proto-authoring design doc's open question D,
// this is the chosen behavior over "feature off without buf.yaml."
//
// AST parsing is lazy: discovery only records file locations; the
// actual parse happens on first Lookup of each import path. A
// sync.Once per entry makes the race-safe single-parse contract
// explicit without holding a coarse mutex during disk IO.

package protoindex

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/trendvidia/protolsp/bufconfig"
	"github.com/trendvidia/protolsp/protoast"
)

// Index is the workspace-wide map of import path → .proto file
// location, plus the lazy parse cache that hydrates ASTs on demand.
// Safe for concurrent use; lookups are RLock-only after discovery
// completes.
type Index struct {
	// workspaceRoot is the absolute path the resolver walks from.
	// Empty when the workspace had no usable root (defensive — the
	// index returns no results in that case).
	workspaceRoot string

	// includePaths are absolute directories the index walked to
	// build the file map, in priority order. Exposed for the
	// resolver to log "where did we look?" diagnostics.
	includePaths []string

	// files maps the import path (forward-slash relative path from
	// some include path) to the entry that knows how to load the
	// file + cache its parsed AST.
	//
	// Built once at construction; entries themselves are mutable
	// (lazy parse) but the map shape doesn't change after the
	// discovery walk completes.
	mu    sync.RWMutex
	files map[string]*entry

	log *slog.Logger
}

// entry is one file the discovery walk found. AST is loaded lazily
// on first call to AST() — keeps construction cheap for workspaces
// with hundreds of .proto files that aren't all hovered.
type entry struct {
	absPath string

	once     sync.Once
	ast      *protoast.File
	parseErr error
}

// AST returns the parsed file for this entry, loading + parsing
// the file on first call. Subsequent calls return the cached AST
// (or nil if the initial read failed). The read error is retained
// for diagnostic logging but not re-raised on subsequent calls —
// hover / definition treat a nil AST as "no resolution available,"
// same surface as an unknown import path.
//
// Parse diagnostics are swallowed inside protoast.Parse — they're
// an editor concern for the OPEN copy of the file. The index is
// the LOAD copy used by the resolver; bad bytes here simply yield
// a partial (or empty) AST.
func (e *entry) AST(log *slog.Logger) *protoast.File {
	e.once.Do(func() {
		data, err := os.ReadFile(e.absPath) //#nosec G304 -- absPath is discovered by walking caller-supplied workspace dirs; not user input
		if err != nil {
			e.parseErr = err
			return
		}
		e.ast = protoast.Parse(e.absPath, data)
	})
	if e.parseErr != nil && log != nil {
		log.Debug("protoindex: read failed",
			slog.String("path", e.absPath),
			slog.String("err", e.parseErr.Error()))
	}
	return e.ast
}

// New builds the index for workspaceRoot using the buf.yaml +
// buf.lock that bufconfig.FindBufYAML returned. Pass cfg=nil to
// trigger the naive fallback (workspace root + proto/).
//
// The constructor walks include paths synchronously — for typical
// workspaces (a few dozen .proto files), this is sub-millisecond.
// Slower workspaces pay the cost once at LSP startup / workspace-
// config reload; lookups stay O(1).
//
// log may be nil — disables diagnostic logging entirely. Tests
// usually pass nil; the server passes its slog.Logger.
//
// BSR cache root: New consults bufconfig.ModuleCacheDir() to find
// the user's buf CLI cache. Tests that need to stage their own
// cache layout under a temp dir should use NewWithCacheRoot.
func New(workspaceRoot string, cfg *bufconfig.Config, lock bufconfig.Lock, log *slog.Logger) *Index {
	cacheRoot, _ := bufconfig.ModuleCacheDir() // empty cacheRoot is fine; resolveIncludePaths skips
	return NewWithCacheRoot(workspaceRoot, cfg, lock, cacheRoot, log)
}

// NewWithCacheRoot is New with an explicit BSR cache root override.
// Production code calls New; tests stage their own cache fixture
// under a temp dir and pass that root here so they can verify the
// cache lookup actually fires for `deps:` entries in buf.lock.
func NewWithCacheRoot(workspaceRoot string, cfg *bufconfig.Config, lock bufconfig.Lock, cacheRoot string, log *slog.Logger) *Index {
	idx := &Index{
		workspaceRoot: workspaceRoot,
		files:         map[string]*entry{},
		log:           log,
	}
	if workspaceRoot == "" {
		return idx
	}
	idx.includePaths = resolveIncludePathsWithCache(workspaceRoot, cfg, lock, cacheRoot)
	idx.discover()
	return idx
}

// Lookup returns the parsed AST for importPath, or nil when the
// import doesn't resolve to any file in the index. importPath is
// the literal string from a proto `import "..."` directive — the
// caller doesn't need to normalize separators (we accept both / and
// \ defensively, though buf only emits forward slashes).
func (idx *Index) Lookup(importPath string) *protoast.File {
	if idx == nil {
		return nil
	}
	key := normalizeImportPath(importPath)
	idx.mu.RLock()
	e := idx.files[key]
	idx.mu.RUnlock()
	if e == nil {
		return nil
	}
	return e.AST(idx.log)
}

// ImportPaths returns the sorted list of import paths the index
// holds files for. Useful for diagnostic logging and for
// completion (Phase 4) — not for hover / definition, which only
// need Lookup.
func (idx *Index) ImportPaths() []string {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make([]string, 0, len(idx.files))
	for k := range idx.files {
		out = append(out, k)
	}
	return out
}

// LocalImportPaths returns the sorted import paths whose backing
// file lives under the workspace root — the workspace's own .proto
// files, excluding BSR cache deps. Callers that fold in "every name
// the workspace declares" (the declared-name export, #272) walk
// these and Lookup each one; walking ImportPaths instead would
// lazily parse the entire BSR cache — potentially thousands of
// files — for declarations the workspace never references.
func (idx *Index) LocalImportPaths() []string {
	if idx == nil || idx.workspaceRoot == "" {
		return nil
	}
	prefix := idx.workspaceRoot + string(filepath.Separator)
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	var out []string
	for key, e := range idx.files {
		if strings.HasPrefix(e.absPath, prefix) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}

// IncludePaths returns the absolute directories the index walked,
// in priority order. Surface used by the resolver's diagnostic
// path when an import doesn't resolve.
func (idx *Index) IncludePaths() []string {
	if idx == nil {
		return nil
	}
	return append([]string(nil), idx.includePaths...)
}

// resolveIncludePathsWithCache builds the priority-ordered include
// path list from cfg + lock, using cacheRoot for BSR dep lookups.
// The naive fallback (no cfg) returns workspace root + proto/
// subdir; otherwise we append module roots first, then BSR cache
// dirs.
//
// Pass cacheRoot="" to skip BSR lookups entirely — useful for tests
// that don't care about remote deps. Production callers thread the
// real bufconfig.ModuleCacheDir() result through New.
func resolveIncludePathsWithCache(workspaceRoot string, cfg *bufconfig.Config, lock bufconfig.Lock, cacheRoot string) []string {
	var paths []string
	if cfg == nil {
		// Naive fallback per the design doc's open question D.
		// Workspace root first (so a flat-layout `acme/foo.proto`
		// next to buf.yaml-less workspace resolves) and `proto/`
		// second (the convention most non-buf projects use).
		paths = append(paths, workspaceRoot)
		if p := filepath.Join(workspaceRoot, "proto"); dirExists(p) {
			paths = append(paths, p)
		}
		return paths
	}
	for _, m := range cfg.Modules {
		abs := filepath.Join(workspaceRoot, m.Path)
		paths = append(paths, abs)
	}
	// BSR cache dirs. Each pinned dep contributes one include path
	// pointing at the dep's `files/` directory. Skipped when the
	// cache lookup fails (cache absent / unreadable) — protolsp
	// surfaces this via the diagnostic log, not by failing the
	// index.
	if cacheRoot != "" {
		for _, dep := range lock.Deps {
			depPath := filepath.Join(cacheRoot, dep.Name, dep.Commit, "files")
			if dirExists(depPath) {
				paths = append(paths, depPath)
			}
		}
	}
	return paths
}

// dirExists reports whether path is an existing directory. Used to
// trim missing entries from the include list so the discovery walk
// doesn't pointlessly stat them.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// discover walks each include path, recording every .proto file
// found and keying it by its forward-slash relative path from the
// containing include path. First-include-wins on duplicate keys:
// later walks SKIP files whose key is already in the map.
//
// Subdir-skip: when walking an include path, we skip subdirectories
// that are themselves other include paths. Without this, a naive-
// fallback workspace (root + proto/) would index every file under
// proto/ TWICE — once with the proto-root-relative key (the right
// one users would type in an `import` directive) and once with a
// proto/-prefixed key (the wrong form rooted at the workspace).
// The same applies to v2 buf.yaml with multiple `modules:` entries
// nested under each other (uncommon, but legal).
func (idx *Index) discover() {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	skip := otherIncludePaths(idx.includePaths)
	for _, root := range idx.includePaths {
		idx.walkInclude(root, skip[root])
	}
	if idx.log != nil {
		idx.log.Info("protoindex: discovery complete",
			slog.Int("files", len(idx.files)),
			slog.Int("include_paths", len(idx.includePaths)))
	}
}

// otherIncludePaths returns, per include path, the set of OTHER
// include paths that should be skipped when walking it. We compute
// this once instead of re-scanning the slice for every directory
// entry the walker visits.
func otherIncludePaths(paths []string) map[string]map[string]struct{} {
	out := make(map[string]map[string]struct{}, len(paths))
	for _, p := range paths {
		skip := make(map[string]struct{})
		for _, q := range paths {
			if p != q {
				skip[q] = struct{}{}
			}
		}
		out[p] = skip
	}
	return out
}

// walkInclude visits every .proto file under root and registers it
// under the relative key (forward-slash, root-relative). Symlinks
// are followed (filepath.Walk's default). Errors at any path are
// logged but don't abort the walk — a permission error on one
// subdir shouldn't make the whole index unavailable.
//
// skipDirs lists absolute paths the walker MUST NOT descend into.
// Used to prevent re-indexing a child include path's files under
// the parent's relative-path prefix; see discover() for the why.
//
// Caller holds idx.mu.
func (idx *Index) walkInclude(root string, skipDirs map[string]struct{}) {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if idx.log != nil {
				idx.log.Debug("protoindex: walk error",
					slog.String("path", path),
					slog.String("err", walkErr.Error()))
			}
			return nil // continue walking; don't abort the whole index
		}
		if d.IsDir() {
			if _, skip := skipDirs[path]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".proto") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		key := normalizeImportPath(rel)
		if _, exists := idx.files[key]; exists {
			// Higher-priority include path already claimed this
			// key — the buf-compatible precedence rule.
			return nil
		}
		idx.files[key] = &entry{absPath: path}
		return nil
	})
	if err != nil && idx.log != nil {
		idx.log.Debug("protoindex: walk terminated",
			slog.String("root", root),
			slog.String("err", err.Error()))
	}
}

// normalizeImportPath converts a filepath-relative key to the
// forward-slash form proto `import` strings use. On Unix this is a
// no-op; on Windows it swaps backslashes.
func normalizeImportPath(p string) string {
	return filepath.ToSlash(p)
}
