// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// W1 of the workspace-config design: discover .protolsp.pxf at the
// workspace root, parse it, and expose its values as Tier 2 of the
// config-protocol model. W4 will lift the single-workspace
// assumption with a nearest-ancestor walk; W5 will reload on
// workspace/didChangeWatchedFiles. For W1 we read once at
// initialize and serve that snapshot for the session's lifetime.

package analysis

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/chain"
	"github.com/trendvidia/protolsp/safeio"
	"github.com/trendvidia/protolsp/schema"
	"github.com/trendvidia/protolsp/workspaceconfig"
)

// configContentForLoad reads the source of a protolsp config file
// (.protolsp.pxf or chain.pxf), preferring the LSP's open-document
// store over disk. When the user is editing the file in their
// editor, the in-memory buffer is the source of truth — disk is
// just whatever was saved before. This keeps the "no save required"
// property of LSP analysis intact for config / chain reloads.
//
// Returns (content, true) on success and (nil, false) on miss.
// Logs at warn level for I/O errors other than "file not found"
// — missing config / chain spec is a soft fail (the LSP runs in
// degraded mode); permission errors or similar are loud so users
// know something's wrong.
func (s *Workspace) configContentForLoad(absPath string) ([]byte, bool) {
	if doc := s.docs.Get(uri.File(absPath)); doc != nil {
		return doc.Source, true
	}
	content, err := safeio.ReadFileLimit(absPath, safeio.MaxConfigFileBytes)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("reading config file failed; continuing with previously loaded state",
				slog.String("path", absPath),
				slog.String("err", err.Error()),
			)
		}
		return nil, false
	}
	return content, true
}

// LoadWorkspaceFileConfig discovers + parses .protolsp.pxf at the
// LSP-reported workspace root. Stores the result on Server.fileConfig.
// Failure modes are all soft — no workspace root, file missing, file
// unreadable, file unparseable — log + continue with fileConfig = nil.
// The server stays usable through any failure mode.
//
// Parse warnings (missing @type, unrecognized blocks) are logged at
// warn level so users see them in the editor's LSP log channel
// without disrupting the session. A future phase will publish these
// as diagnostics on the .protolsp.pxf file itself.
func (s *Workspace) LoadWorkspaceFileConfig() {
	// Reset first so a reload from W5's didChangeWatchedFiles path
	// observes the new state even when the file was deleted or
	// emptied since the previous load. Initial-load callers see no
	// behavior change — fileConfig starts as nil.
	s.SwapFileConfig(nil)
	if s.ws.workspaceRoot == "" {
		return
	}
	path, ok := URIToFilePath(s.ws.workspaceRoot)
	if !ok {
		// Non-file workspace root (e.g. a remote-workspace scheme).
		// Nothing to read locally.
		return
	}
	configPath := filepath.Join(path, workspaceconfig.ConfigFileName)
	content, ok := s.configContentForLoad(configPath)
	if !ok {
		return
	}
	cfg, warnings, err := workspaceconfig.ParseWithEnv(content, s.lookupEnv)
	if err != nil {
		s.log.Warn(".protolsp.pxf is unparseable; continuing with editor settings only",
			slog.String("path", configPath),
			slog.String("err", err.Error()),
		)
		return
	}
	for _, w := range warnings {
		s.log.Warn(".protolsp.pxf warning",
			slog.String("path", configPath),
			slog.String("message", w.String()),
		)
	}
	if cfg.HasContent {
		s.SwapFileConfig(cfg)
		s.log.Info(".protolsp.pxf loaded",
			slog.String("path", configPath),
			slog.String("registry_address", cfg.Registry.Address),
			slog.String("registry_namespace", cfg.Registry.Namespace),
			slog.Bool("proto_files", !cfg.ProtoFiles.IsEmpty()),
		)
	}
}

// rootProtoFilesCompileErrors holds the last set of compile errors
// returned by OpenProtoFiles for the ROOT workspace. Stashed on
// the Server because the root's proto_files lookup lives on
// LookupManager (not on a WorkspaceState), and the diagnostic
// surfacing path needs a place to read errors from. Sub-workspaces
// keep their errors on WorkspaceState.CompileErrors.
//
// Reset on every rebuild; nil when no proto_files source is
// configured for the root.
type rootProtoFilesCompileErrors struct {
	mu   sync.RWMutex
	errs []schema.CompileError
}

func (r *rootProtoFilesCompileErrors) Snapshot() []schema.CompileError {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.errs) == 0 {
		return nil
	}
	out := make([]schema.CompileError, len(r.errs))
	copy(out, r.errs)
	return out
}

func (r *rootProtoFilesCompileErrors) Swap(errs []schema.CompileError) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.errs = errs
	r.mu.Unlock()
}

// RebuildRootProtoFilesLookup compiles the root workspace's
// `proto_files { ... }` sources (W2). The root counterpart to
// rebuildProtoFilesLookupForWorkspace: same shape, but the lookup
// installs on LookupManager and compile errors stash on the
// Server's rootProtoFilesErrs slot for the config-diagnostics path
// to read.
//
// Soft-failure throughout: setup failures (path walks, fs errors)
// log + leave the previous state alone; compile errors land in
// the stashed slice without disturbing the installed lookup.
func (s *Workspace) RebuildRootProtoFilesLookup() {
	if s == nil {
		return
	}
	rootDir, _ := URIToFilePath(s.ws.workspaceRoot)
	cfg, hasContent := workspaceProtoFilesConfig(rootDir, s.SnapshotFileConfig())
	s.rootProtoFilesErrs.Swap(nil)
	if !hasContent {
		if old := s.lookupMgr.swapProtoFiles(nil, nil); old != nil {
			if err := old.Close(); err != nil {
				s.log.Warn("closing previous root proto_files lookup",
					slog.String("err", err.Error()))
			}
		}
		return
	}
	lookup, closer, compileErrs, err := schema.OpenProtoFiles(context.Background(), cfg, s.log)
	if err != nil {
		s.log.Warn("root proto_files compile setup failed",
			slog.String("err", err.Error()))
		return
	}
	s.rootProtoFilesErrs.Swap(compileErrs)
	if old := s.lookupMgr.swapProtoFiles(lookup, closer); old != nil {
		if err := old.Close(); err != nil {
			s.log.Warn("closing previous root proto_files lookup",
				slog.String("err", err.Error()))
		}
	}
	for _, ce := range compileErrs {
		s.log.Warn("root proto_files compile error",
			slog.String("error", ce.String()))
	}
	s.log.Info("root proto_files lookup rebuilt",
		slog.Int("compile_errors", len(compileErrs)))
}

// LoadWorkspaceChain reads the chain.pxf path declared in the
// workspace's chameleon block (C1 of the chameleon-integration
// design), loads + expands it, and stores the result on Server.chain.
//
// Soft-failure model matches LoadWorkspaceFileConfig: any failure
// here is logged at warn level and leaves s.chain nil. The LSP keeps
// running in standalone mode — chain-aware behaviors degrade
// gracefully.
//
// Pre-conditions checked by caller: fileConfig is non-nil and has a
// chameleon block whose ChainSpec is set + Enabled is true.
func (s *Workspace) LoadWorkspaceChain() {
	// Reset first (same reason as LoadWorkspaceFileConfig): a W5
	// reload must observe deletions / disable toggles.
	s.SwapChain(nil)
	fileConfig := s.SnapshotFileConfig()
	if fileConfig == nil || fileConfig.Chameleon.ChainSpec == "" {
		return
	}
	if !fileConfig.Chameleon.Enabled {
		// Workspace explicitly disabled chain mode — silent no-op.
		return
	}
	root, ok := URIToFilePath(s.ws.workspaceRoot)
	if !ok {
		// Non-file workspace root; chain mode requires local disk.
		return
	}
	resolved := fileConfig.Chameleon.ChainSpec
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(root, resolved)
	}
	content, ok := s.configContentForLoad(resolved)
	if !ok {
		return
	}
	loaded, err := chain.ParseAt(resolved, content)
	if err != nil {
		s.log.Warn("parsing chameleon chain spec; continuing in standalone mode",
			slog.String("path", resolved),
			slog.String("err", err.Error()),
		)
		return
	}
	s.SwapChain(loaded)
	s.log.Info("chameleon chain spec loaded",
		slog.String("path", resolved),
		slog.Int("environments", len(loaded.Environments)),
		slog.Int("layers", len(loaded.Layers)),
		slog.Int("errors", len(loaded.Errors)),
	)
}

// ResolvedChainSpecPath returns the absolute filesystem path of
// the active chain spec, or "" when no chain is configured / the
// workspace root isn't on local disk. The path is what
// LoadWorkspaceChain would feed to chain.Load — chain.pxf relative
// to the .protolsp.pxf's directory, resolved against the workspace
// root.
//
// Used by W5's watcher registration to register a watcher for the
// specific chain spec file when its name isn't the conventional
// `chain.pxf` (which is covered by the static `**/chain.pxf`
// pattern). Returns "" for the common path so the caller doesn't
// double-register.
func (s *Workspace) ResolvedChainSpecPath() string {
	if s == nil {
		return ""
	}
	fileConfig := s.SnapshotFileConfig()
	if fileConfig == nil {
		return ""
	}
	if fileConfig.Chameleon.ChainSpec == "" || !fileConfig.Chameleon.Enabled {
		return ""
	}
	rootPath, ok := URIToFilePath(s.ws.workspaceRoot)
	if !ok {
		return ""
	}
	p := fileConfig.Chameleon.ChainSpec
	if !filepath.IsAbs(p) {
		p = filepath.Join(rootPath, p)
	}
	return p
}

// URIToFilePath converts an LSP URI to a filesystem path. Returns
// (path, true) for file:// URIs; (zero, false) for any other
// scheme (non-file editors, remote workspaces) or a parse error.
//
// Unifies what used to be three near-identical helpers
// (workspaceRootPath, documentFilePath, watchedURIToPath) — they
// all did `url.Parse + Scheme=="file" + .Path`. One signature
// covers all three because `protocol.DocumentURI` is a type alias
// for `uri.URI` (per go.lsp.dev/protocol).
//
// Non-file workspace roots are out of scope per the W1 design
// (see workspace-config.md §Subtleties on remote workspaces);
// callers should treat the (false) return as "no local path
// available; degrade gracefully."
func URIToFilePath(u uri.URI) (string, bool) {
	parsed, err := url.Parse(string(u))
	if err != nil || parsed.Scheme != "file" {
		return "", false
	}
	return parsed.Path, true
}

// EffectiveRegistryConfig merges Tier 2 (file) and Tier 3 (editor
// settings) into a single schema.Config the server can hand to
// schema.Open. Tier 3 wins per the config-protocol design — editor
// settings beat project defaults. Empty Tier 3 fields fall through
// to Tier 2 values. Tier 1 (default) is empty string for registry
// fields (per effective_config.go's defaultFor); Tier 4 (override)
// applies to individual sections at lookup time, not to the
// resolver itself.
//
// This helper exists because schema.Open takes one schema.Config —
// it doesn't know about tiers. Computing the effective config in
// one place keeps the lookup-construction path simple.
func (s *Workspace) EffectiveRegistryConfig() schema.Config {
	editor := s.SnapshotEditorConfig().Registry
	var file schema.Config
	if fc := s.SnapshotFileConfig(); fc != nil {
		file = registrySourceToConfig(fc.Registry)
	}
	return mergeRegistryConfig(editor, file)
}

// registrySourceToConfig converts the workspaceconfig.RegistrySource
// shape (parsed from .protolsp.pxf) to schema.Config (consumed by
// schema.Open). Field-for-field; exists because the two types live
// in different packages by design — workspaceconfig knows nothing
// about schema, and schema knows nothing about PXF parsing.
func registrySourceToConfig(r workspaceconfig.RegistrySource) schema.Config {
	return schema.Config{
		Address:   r.Address,
		Namespace: r.Namespace,
		Token:     r.Token,
		CacheDir:  r.CacheDir,
	}
}

// mergeRegistryConfig implements the "Tier 3 (settings) wins;
// Tier 2 (file) fills gaps" semantic shared by the root and
// sub-workspace effective-config callsites. Returns a value-typed
// merged schema.Config — settings is treated as the base, and
// each empty field is filled in from `file`. An empty `file` is
// safe (fillFromFile no-ops when both sides are empty).
func mergeRegistryConfig(settings, file schema.Config) schema.Config {
	merged := settings
	fillFromFile(&merged.Address, file.Address)
	fillFromFile(&merged.Namespace, file.Namespace)
	fillFromFile(&merged.Token, file.Token)
	fillFromFile(&merged.CacheDir, file.CacheDir)
	return merged
}

// fillFromFile copies the file value into dst only when dst is the
// empty string. Matches the "Tier 3 wins; Tier 2 fills gaps"
// semantic.
func fillFromFile(dst *string, fileValue string) {
	if *dst == "" && fileValue != "" {
		*dst = fileValue
	}
}

// RebuildLookupFromEffectiveConfig is the public seam that callers
// (initialize, W5 watcher, live-reload) use to ask the server to
// pick up registry-config changes. The actual work — diff,
// optional dial, atomic install — happens in RebuildLookupAsync.
// Kept as a thin shim so existing call sites don't need updates.
//
// Returns immediately; the dial (if any) runs in a goroutine.
// See lookup_rebuild.go for the diff + cancellation semantics.
func (s *Workspace) RebuildLookupFromEffectiveConfig(ctx context.Context) {
	s.RebuildLookupAsync(ctx)
}
