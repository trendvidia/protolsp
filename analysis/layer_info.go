// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// C2 of the chameleon-integration design: the protolsp/layerInfo
// custom request plus the per-document layer-detection cache that
// backs it. Pull-only; cheap; never errors.
//
// Detection rule (from chameleon-integration.md §"Layer detection"):
//   1. Compute the document's path relative to the workspace root.
//   2. Match against every LayerInstance.ResolvedPath from the
//      active chain. First match wins.
//   3. Unmatched files are standalone — chain-aware behaviors skip.
//
// The result is cached on Document.LayerInfo at didOpen time. The
// URI doesn't change for an open document and the chain is
// workspace-scoped (rebuilt only on chain.pxf reload, C2+), so a
// one-shot compute is fine — no didChange invalidation.
//
// "Not configured" / "no workspace root" / "non-file URI" all
// degrade to isLayer=false. Editors render their own affordance
// from that signal.

package analysis

import (
	"log/slog"
	"path/filepath"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/chain"
)

// MethodLayerInfo is the custom request returning the chain-layer
// classification for a document. Pull-only: layer assignment
// changes when chain.pxf changes, but the design's sketched
// chainStatusChanged push notification never shipped — editors
// re-fetch via this request after `effectiveConfigChanged` /
// `workspace/didChangeWatchedFiles` events fire.
const MethodLayerInfo = "protolsp/layerInfo"

// LayerInfoParams is the request shape: which document to classify.
type LayerInfoParams struct {
	TextDocument protocol.TextDocumentIdentifier `json:"textDocument"`
}

// LayerInfoResult mirrors the design doc's shape. Optional fields
// (environment, ancestors, strategy, source) are empty / nil when
// isLayer is false — the JSON tag uses ",omitempty" so the wire
// payload stays clean.
//
// Encrypted is non-optional: it carries useful signal even on
// non-layer files (always false) and editors don't need to special-
// case "field absent" vs "field set to false".
type LayerInfoResult struct {
	IsLayer     bool     `json:"isLayer"`
	Environment string   `json:"environment,omitempty"`
	Ancestors   []string `json:"ancestors,omitempty"`
	Strategy    string   `json:"strategy,omitempty"`
	Source      string   `json:"source,omitempty"`
	Encrypted   bool     `json:"encrypted"`
}

// DetectLayerInfo computes the document's classification and
// installs it on the Document. Called from dispatchDidOpen (before
// the first analyze) and from the reanalyze paths (after a chain
// or config reload).
//
// Safe to call when chain is nil or workspaceRoot is empty — the
// result is just IsLayer=false. Takes doc.mu.Lock briefly to install
// the result so concurrent handler reads via doc.mu.RLock see a
// coherent value.
func (s *Workspace) DetectLayerInfo(doc *Document) {
	if doc == nil {
		return
	}
	info := s.ComputeLayerInfo(doc.URI)
	doc.mu.Lock()
	doc.LayerInfo = info
	doc.mu.Unlock()
}

// ComputeLayerInfo is the pure detection function shared by the
// didOpen warm-up and the dispatch handler's fallback path.
//
// Returns a non-nil result; an empty / non-matching environment
// gives IsLayer=false. Path matching is exact against
// LayerInstance.ResolvedPath after computing the document's path
// relative to the workspace root.
//
// Encrypted bit comes from the spec's static `encrypted = true`
// flag; the content-sniff for sops streams (chameleon-integration
// §"sops-encrypted layers") is a later refinement that lives
// alongside the diagnostic-skip behavior (C3+).
func (s *Workspace) ComputeLayerInfo(docURI uri.URI) *LayerInfoResult {
	empty := &LayerInfoResult{IsLayer: false}
	if s == nil {
		return empty
	}
	// Resolve to the doc's WORKSPACE chain (W4) — the doc may
	// nest under a sub-workspace whose `.protolsp.pxf` declares a
	// different chain than the root. Layer paths in that chain
	// are relative to the SUB-workspace's dir.
	var docChain = s.SnapshotChain()
	var workspaceDir string
	if d := s.docs.Get(docURI); d != nil {
		s.EnsureWorkspaceForDoc(d)
		docChain = s.ChainForDoc(d)
		workspaceDir = s.workspaceDirForDoc(d)
	}
	if docChain == nil || len(docChain.Layers) == 0 {
		s.log.Debug("layerInfo: no chain or empty layers",
			slog.String("uri", string(docURI)),
			slog.Bool("chain_nil", docChain == nil),
			slog.Int("layers_len", func() int {
				if docChain == nil {
					return -1
				}
				return len(docChain.Layers)
			}()),
		)
		return empty
	}
	if workspaceDir == "" {
		// No doc in the store yet (the layerInfo request can fire
		// before didOpen). Fall back to the LSP-reported root.
		root, ok := URIToFilePath(s.ws.workspaceRoot)
		if !ok {
			s.log.Debug("layerInfo: no workspace dir + LSP root not a file URI",
				slog.String("uri", string(docURI)),
				slog.String("ws_root", string(s.ws.workspaceRoot)),
			)
			return empty
		}
		workspaceDir = root
	}
	docPath, ok := URIToFilePath(docURI)
	if !ok {
		s.log.Debug("layerInfo: doc URI not a file URI",
			slog.String("uri", string(docURI)),
		)
		return empty
	}
	rel, err := filepath.Rel(workspaceDir, docPath)
	if err != nil {
		s.log.Debug("layerInfo: Rel failed",
			slog.String("workspace_dir", workspaceDir),
			slog.String("doc_path", docPath),
			slog.String("err", err.Error()),
		)
		return empty
	}
	// filepath.Rel can return paths like "../outside" when the
	// document lives outside the workspace dir; treat those as
	// non-layer.
	if strings.HasPrefix(rel, "..") {
		s.log.Debug("layerInfo: doc outside workspace",
			slog.String("workspace_dir", workspaceDir),
			slog.String("doc_path", docPath),
			slog.String("rel", rel),
		)
		return empty
	}
	// Normalize separators for cross-platform comparison: chain
	// spec authors write forward slashes ("env/dev.pxf") regardless
	// of OS. filepath.Rel uses the platform separator. Convert both
	// sides to forward slashes for the match.
	relSlash := filepath.ToSlash(rel)
	candidates := make([]string, 0, len(docChain.Layers))
	for _, layer := range docChain.Layers {
		candidates = append(candidates, filepath.ToSlash(layer.ResolvedPath))
		if filepath.ToSlash(layer.ResolvedPath) != relSlash {
			continue
		}
		s.log.Debug("layerInfo: matched layer",
			slog.String("uri", string(docURI)),
			slog.String("rel", relSlash),
			slog.String("layer_id", layer.ID),
			slog.String("environment", layer.Environment),
		)
		result := &LayerInfoResult{
			IsLayer:     true,
			Environment: layer.Environment,
			Strategy:    layer.Strategy,
			Source:      layer.Source,
			Encrypted:   layer.Encrypted,
		}
		// Ancestors only meaningful for PER_ENV layers where the
		// environment is bound. ONCE layers have no environment, so
		// ancestry is irrelevant; leaving Ancestors nil drops it
		// from the wire payload (omitempty).
		if layer.Environment != "" {
			if info, ok := docChain.Environments[layer.Environment]; ok && len(info.Ancestors) > 0 {
				result.Ancestors = append([]string(nil), info.Ancestors...)
			}
		}
		return result
	}
	// Permissive fallback: if no expanded LayerInstance matched
	// exactly, see whether the file lives under any declared fs
	// source's `root`. Real-world chain specs sometimes have layer
	// `path` templates that don't combine cleanly with the source
	// `root` (e.g. `path = "secrets/${env}.pxf"` paired with
	// `source.root = "./layers/secrets"`), and the runtime tolerates
	// this in ways that aren't trivial to mirror exactly. Treating
	// files under a known source root as layers anyway lets schema-
	// aware features (go-to-def, hover, completion) light up for
	// what is clearly intended to be a layer file, while still
	// keeping IsLayer=false for files outside any declared source.
	if strings.HasSuffix(strings.ToLower(relSlash), ".pxf") {
		if src := s.fsSourceContaining(docChain, relSlash); src != nil {
			s.log.Debug("layerInfo: matched fs source root",
				slog.String("uri", string(docURI)),
				slog.String("rel", relSlash),
				slog.String("source", src.Name),
			)
			return &LayerInfoResult{
				IsLayer: true,
				Source:  src.Name,
			}
		}
	}
	s.log.Debug("layerInfo: no matching layer",
		slog.String("uri", string(docURI)),
		slog.String("workspace_dir", workspaceDir),
		slog.String("doc_path", docPath),
		slog.String("rel", relSlash),
		slog.Any("candidates", candidates),
	)
	return empty
}

// fsSourceContaining returns the fs-kind SourceRef whose `root`
// option contains the file (relative to the workspace dir, forward-
// slashed), or nil if none match. Source roots written as `./layers`
// match files like `layers/foo.pxf`; `./layers/secrets` matches
// `layers/secrets/dev.pxf`. Longest-matching root wins so a nested
// source ("./layers/secrets") takes precedence over its parent
// ("./layers"), letting the secrets-specific affordances apply.
func (s *Workspace) fsSourceContaining(
	ac *chain.ActiveChain,
	relSlashPath string,
) *chain.SourceRef {
	if ac == nil || ac.Sources == nil {
		return nil
	}
	var best *chain.SourceRef
	var bestLen int
	for name, src := range ac.Sources {
		if src.Kind != "fs" {
			continue
		}
		root := src.Options["root"]
		if root == "" {
			continue
		}
		root = strings.TrimPrefix(root, "./")
		root = strings.TrimSuffix(root, "/")
		if root == "" {
			continue
		}
		if !strings.HasPrefix(relSlashPath, root+"/") {
			continue
		}
		if len(root) > bestLen {
			matched := src
			matched.Name = name
			best = &matched
			bestLen = len(root)
		}
	}
	return best
}
