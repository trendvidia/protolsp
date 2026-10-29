// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Live reload for protolsp's own config files: when the user
// edits `.protolsp.pxf` or `chain.pxf` in their editor, the LSP
// re-derives fileConfig / chain from the open-document source
// rather than disk, so chain-aware features in OTHER open docs
// react to the in-progress edits without requiring a save.
//
// Symmetry with the existing patterns:
//   - W5's didChangeWatchedFiles handler reloads on save events.
//     Live reload covers the in-flight-edit case W5 can't reach.
//   - The same load functions (LoadWorkspaceFileConfig,
//     LoadWorkspaceChain) drive both flows. Their
//     configContentForLoad helper checks the docs store first,
//     so calling them post-edit naturally picks up the buffer.
//   - reanalyzeOpenDocs walks every open document; we pass the
//     doc that triggered the reload so we don't redundantly
//     re-analyze it (runAnalyzePipeline already ran for it as
//     part of the dispatch path).
//
// Recursion avoidance: live reload is only invoked from the
// RunAnalyzeAndMaybeLiveReload wrapper (called from didOpen +
// ScheduleAnalyzePipeline). reanalyzeOpenDocs uses
// runAnalyzePipeline directly, which does NOT trigger live
// reload. So the cycle "reload → reanalyze → reload again"
// can't happen.

package analysis

import (
	"context"
	"log/slog"

	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/chain"
	"github.com/trendvidia/protolsp/workspaceconfig"
)

// RunAnalyzeAndMaybeLiveReload is the dispatch-path wrapper around
// runAnalyzePipeline. After the analyze step finishes, it checks
// whether the just-analyzed doc was protolsp's own config or
// chain spec and, if so, kicks off a live reload + re-analyze of
// every other open doc.
//
// Used by dispatchDidOpen (synchronous) and the scheduleAnalyze
// path (which fires after the debounce window). The plain
// runAnalyzePipeline keeps its original signature and is what
// reanalyzeOpenDocs uses — that path skips the live-reload step
// to prevent infinite recursion.
func (s *Workspace) RunAnalyzeAndMaybeLiveReload(
	ctx context.Context,
	docURI uri.URI,
	previousSecrets []SecretRange,
	previousDatasets []DatasetRegion,
) {
	s.runAnalyzePipeline(ctx, docURI, previousSecrets, previousDatasets)
	doc := s.docs.Get(docURI)
	if doc == nil {
		return
	}
	switch {
	case s.IsWorkspaceConfigDoc(doc):
		s.liveReloadWorkspaceConfig(ctx, doc)
	case s.IsChainSpecDoc(doc):
		s.liveReloadChainSpec(ctx, doc)
	}
}

// IsWorkspaceConfigDoc reports whether the given doc IS the
// workspace's .protolsp.pxf. Routing is by @type FQN (most
// reliable, survives renamed files), falling back to URI basename
// match for docs that haven't parsed yet or that omit the
// directive.
//
// Takes doc.mu.RLock to snapshot Parsed — the watched-files
// reanalyze pass rewrites it concurrently (#184). Callers must
// not already hold doc.mu.
func (s *Workspace) IsWorkspaceConfigDoc(doc *Document) bool {
	if doc == nil {
		return false
	}
	if parsed := doc.snapshotParsed(); parsed != nil && parsed.TypeURL == workspaceconfig.ExpectedType {
		return true
	}
	// URI fallback: basename match. The W4 multi-root walk will
	// refine this with workspace-relative path checks; for now
	// any `.protolsp.pxf` that's open counts.
	return doc.URI.Filename() != "" &&
		filenameMatches(doc.URI, workspaceconfig.ConfigFileName)
}

// IsChainSpecDoc reports whether the given doc IS the workspace's
// active chain spec. Like isWorkspaceConfigDoc, prefers @type and
// falls back to a path comparison against the currently-loaded
// chain's Path (so the user editing the exact file we loaded
// definitely counts).
//
// Takes doc.mu.RLock to snapshot Parsed — same discipline (and
// same race, #184) as IsWorkspaceConfigDoc. Callers must not
// already hold doc.mu.
func (s *Workspace) IsChainSpecDoc(doc *Document) bool {
	if doc == nil {
		return false
	}
	if parsed := doc.snapshotParsed(); parsed != nil && parsed.TypeURL == chain.ExpectedType {
		return true
	}
	if c := s.SnapshotChain(); c != nil && c.Path != "" && doc.URI.Filename() == c.Path {
		return true
	}
	return false
}

// filenameMatches reports whether the basename of uri.URI matches
// `name`. Uses uri.URI.Filename() which already strips the
// scheme + path-to-basename conversion.
func filenameMatches(u uri.URI, name string) bool {
	fn := u.Filename()
	// Filename returns the full path; strip to basename for compare.
	for i := len(fn) - 1; i >= 0; i-- {
		if fn[i] == '/' {
			return fn[i+1:] == name
		}
	}
	return fn == name
}

// liveReloadWorkspaceConfig re-derives fileConfig from the
// triggering doc, rebuilds the schema lookup if registry config
// changed, reloads the chain (in case the chameleon block was
// edited), and re-analyzes every OTHER open doc so their
// chain-aware diagnostics + LayerInfo reflect the new world.
//
// Routing (W4): the trigger doc's WorkspaceDir tells us which
// workspace was edited. Root edits hit the existing server-
// scoped load helpers; sub-workspace edits go through
// reloadSubWorkspace. Either way, reanalyzeOpenDocsExcept
// refreshes every other open doc's LayerInfo + diagnostics so
// docs that depend on the affected workspace catch up.
//
// triggerDoc is excluded from the reanalyze pass — it was just
// analyzed by the wrapper that called us.
func (s *Workspace) liveReloadWorkspaceConfig(ctx context.Context, triggerDoc *Document) {
	rootPath, _ := URIToFilePath(s.ws.workspaceRoot)
	if triggerDoc.WorkspaceDir() == "" || triggerDoc.WorkspaceDir() == rootPath {
		s.LoadWorkspaceFileConfig()
		s.RebuildLookupFromEffectiveConfig(ctx)
		s.LoadWorkspaceChain()
	} else {
		s.reloadSubWorkspace(triggerDoc.WorkspaceDir())
	}
	s.log.Debug("live-reload: workspace config",
		slog.String("triggered_by", string(triggerDoc.URI)),
		slog.String("workspace_dir", triggerDoc.WorkspaceDir()))
	s.ReanalyzeOpenDocsExcept(ctx, triggerDoc.URI)
}

// liveReloadChainSpec re-derives the active chain from the
// triggering doc and re-analyzes every other open doc — they
// may have been classified as PER_ENV layers under the old chain
// shape but no longer match, or vice versa.
//
// Routing (W4): the trigger doc's WorkspaceDir tells us which
// workspace's chain to reload. Root edits hit LoadWorkspaceChain;
// sub-workspace edits go through reloadSubWorkspace (which
// re-reads BOTH the config and the chain — the sub-workspace
// loader doesn't separate them).
func (s *Workspace) liveReloadChainSpec(ctx context.Context, triggerDoc *Document) {
	rootPath, _ := URIToFilePath(s.ws.workspaceRoot)
	if triggerDoc.WorkspaceDir() == "" || triggerDoc.WorkspaceDir() == rootPath {
		s.LoadWorkspaceChain()
	} else {
		s.reloadSubWorkspace(triggerDoc.WorkspaceDir())
	}
	s.log.Debug("live-reload: chain spec",
		slog.String("triggered_by", string(triggerDoc.URI)),
		slog.String("workspace_dir", triggerDoc.WorkspaceDir()))
	s.ReanalyzeOpenDocsExcept(ctx, triggerDoc.URI)
}

// ReanalyzeOpenDocsExcept is reanalyzeOpenDocs with a one-URI
// skip. Used by live-reload so the doc that triggered the reload
// doesn't get analyzed twice (the trigger path already ran
// runAnalyzePipeline for it).
func (s *Workspace) ReanalyzeOpenDocsExcept(ctx context.Context, skip uri.URI) {
	if s == nil || s.docs == nil {
		return
	}
	for _, u := range s.docs.uris() {
		if u == skip {
			continue
		}
		doc := s.docs.Get(u)
		if doc == nil {
			continue
		}
		_, prevSecrets, prevDatasets := doc.SnapshotForPipeline()
		s.DetectLayerInfo(doc)
		s.runAnalyzePipeline(ctx, u, prevSecrets, prevDatasets)
	}
}
