// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Workspace-state ownership. T1's third extraction (PR C) from
// Server's god-object: the LSP workspace root, the root
// workspace's parsed `.protolsp.pxf` + chain spec, and the
// sub-workspace map W4 maintains for nested `.protolsp.pxf`
// files. Owns Server.stateMu (now narrowed to just the workspace
// concern after PR B moved editor settings into ConfigState).
//
// Methods that orchestrate file I/O + analyze pipeline
// (LoadWorkspaceFileConfig, LoadWorkspaceChain,
// EnsureWorkspaceForDoc, reloadSubWorkspace, MaybeEvictSubWorkspace,
// tearDownSubWorkspace, chainForDoc, etc.) STAY on Server because
// they need to reach into other concerns: s.docs for
// open-document preference in configContentForLoad, s.cfg for
// the per-workspace effective registry merge, s.lookupMgr to
// rebuild per-workspace lookups, s.log for warnings. Moving them
// here would force a bidirectional reference. Server holds a
// *WorkspaceManager and the methods read / write through it.

package analysis

import (
	"sync"

	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/chain"
	"github.com/trendvidia/protolsp/workspaceconfig"
)

// WorkspaceManager owns the per-server workspace state. The
// zero value is NOT usable — use newWorkspaceManager.
//
// Concurrency:
//   - stateMu (RWMutex) guards rootFileConfig + rootChain. They
//     get written from the main goroutine (initialize, watcher
//     events) and the analyze-debounce goroutine (live-reload);
//     read from every handler plus the async rebuild's
//     reanalyzeOpenDocs.
//   - workspacesMu (Mutex) guards the subWorkspaces map. The
//     WorkspaceState values themselves have their own
//     per-workspace locks (lookupMu, rebuildMu).
//   - workspaceRoot is set once at initialize before any
//     concurrent handler can race with it; no mutex needed.
//
// Lock-order between the two: never both held at once today.
// If a composite read ever needs them, take stateMu before
// workspacesMu (alphabetical; document the choice).
type WorkspaceManager struct {
	// workspaceRoot is the client's reported workspace root URI
	// (the first WorkspaceFolders entry or, for older clients,
	// RootURI). Used by go-to-definition to resolve relative
	// `.proto` paths from FileDescriptors into absolute file URIs
	// the editor can open. Empty when the client didn't supply a
	// root (single-file editing).
	workspaceRoot uri.URI

	stateMu sync.RWMutex

	// rootFileConfig is the Tier 2 view parsed from the
	// workspace-root `.protolsp.pxf`. Nil when no file was found,
	// unreadable, or its parse failed catastrophically. Loaded at
	// initialize; refreshed by W5's watcher + live-reload paths.
	rootFileConfig *workspaceconfig.Config

	// rootChain is the loaded chameleon chain spec when
	// rootFileConfig.Chameleon is active. Nil when no chain is
	// configured, the spec failed to load catastrophically, or
	// chain mode was disabled.
	rootChain *chain.ActiveChain

	// subWorkspaces holds the lazily-loaded fileConfig + chain
	// for sub-workspaces discovered via `.protolsp.pxf`
	// nearest-ancestor walks (W4). The ROOT workspace's state
	// lives in rootFileConfig / rootChain above; this map only
	// covers nested workspaces. Keyed by the absolute path of
	// the directory containing `.protolsp.pxf`. Guarded by
	// workspacesMu; external readers go through
	// Workspace.SubWorkspaceByDir / SubWorkspaceCount (#179).
	subWorkspaces map[string]*WorkspaceState
	workspacesMu  sync.Mutex
}

func newWorkspaceManager() *WorkspaceManager {
	return &WorkspaceManager{}
}

// WorkspaceRoot returns the LSP-reported workspace root URI.
// Set once at initialize; safe to read concurrently afterward.
func (w *WorkspaceManager) WorkspaceRoot() uri.URI {
	if w == nil {
		return ""
	}
	return w.workspaceRoot
}

// SetWorkspaceRoot installs the workspace root. Called once
// from dispatchInitialize before the connection loop begins
// accepting other handlers; no synchronization needed.
func (w *WorkspaceManager) SetWorkspaceRoot(u uri.URI) {
	if w == nil {
		return
	}
	w.workspaceRoot = u
}

// SnapshotFileConfig returns the root workspace's parsed
// fileConfig (may be nil). Concurrent-safe.
func (w *WorkspaceManager) SnapshotFileConfig() *workspaceconfig.Config {
	if w == nil {
		return nil
	}
	w.stateMu.RLock()
	defer w.stateMu.RUnlock()
	return w.rootFileConfig
}

// SwapFileConfig replaces the root fileConfig pointer atomically
// and returns the prior value. The pointer is treated as
// immutable post-publish (writers swap the whole struct rather
// than mutating in place), so readers can hold the returned
// snapshot across multiple field accesses without further
// locking.
func (w *WorkspaceManager) SwapFileConfig(cfg *workspaceconfig.Config) *workspaceconfig.Config {
	if w == nil {
		return nil
	}
	w.stateMu.Lock()
	old := w.rootFileConfig
	w.rootFileConfig = cfg
	w.stateMu.Unlock()
	return old
}

// SnapshotChain returns the root workspace's loaded chain spec
// (may be nil). Concurrent-safe.
func (w *WorkspaceManager) SnapshotChain() *chain.ActiveChain {
	if w == nil {
		return nil
	}
	w.stateMu.RLock()
	defer w.stateMu.RUnlock()
	return w.rootChain
}

func (w *WorkspaceManager) SwapChain(c *chain.ActiveChain) *chain.ActiveChain {
	if w == nil {
		return nil
	}
	w.stateMu.Lock()
	old := w.rootChain
	w.rootChain = c
	w.stateMu.Unlock()
	return old
}
