// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// LSP diagnostics produced by the dedicated parsers for protolsp's
// own config files. The generic schema-validation pipeline already
// covers structural correctness via the internal descriptors;
// these helpers add the SEMANTIC checks the dedicated parsers
// know about — bad enum values, multiple registries, ancestry
// cycles, missing @type, etc.
//
// Detection routes by `@type` directive: a doc whose @type is
// `protolsp.v1.WorkspaceConfig` runs through workspaceconfig.Parse;
// `chameleon.v1.ChainSpec` runs through chain.ParseAt. Docs with
// either FQN but parsed bytes that fail the dedicated parser
// produce no diagnostics from this path (the parse error itself
// already surfaces via protolsp/parse). The dedicated parsers
// always succeed on PXF-valid input and surface semantic
// disagreements as their own diagnostic structs.

package analysis

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/chain"
	"github.com/trendvidia/protolsp/schema"
	"github.com/trendvidia/protolsp/workspaceconfig"
)

// ConfigDiagnostics returns the dedicated-parser diagnostics for
// the document. Empty when the doc isn't one of protolsp's own
// config files (matched by @type). Appended to diagnosticsFor's
// output for the workspace config + chain spec.
//
// Severity is Warning across the board: the file still parses and
// loads; we're flagging things the parser found suspicious but
// can recover from. Source tag is "protolsp/workspaceconfig" or
// "protolsp/chainspec" so editors can group / color them
// distinctly from schema findings. proto_files compile errors
// (W2) surface as "protolsp/proto_files" with Error severity —
// they prevent schema-awareness on the affected types and
// deserve more attention than parser warnings.
func (s *Workspace) ConfigDiagnostics(doc *Document) []protocol.Diagnostic {
	if doc == nil || doc.Parsed == nil {
		return nil
	}
	switch doc.Parsed.TypeURL {
	case workspaceconfig.ExpectedType:
		diags := s.workspaceConfigDiagnostics(doc.Source)
		return append(diags, s.protoFilesCompileDiagnostics(doc)...)
	case chain.ExpectedType:
		return chainSpecDiagnostics(doc.URI.Filename(), doc.Source)
	}
	return nil
}

// protoFilesCompileDiagnostics returns Error-severity diagnostics
// for any protocompile errors stashed for doc's workspace (W2).
// The diagnostic anchors at the proto_files block opening in the
// .protolsp.pxf so the user lands close to the source declaration
// when they jump to the diagnostic. The compile error's File:Line
// is included in the message body so the user knows which .proto
// file to fix.
//
// Both root and sub-workspace `.protolsp.pxf` paths flow through
// here; routing happens by matching the doc's filesystem path
// against known workspace dirs.
func (s *Workspace) protoFilesCompileDiagnostics(doc *Document) []protocol.Diagnostic {
	if s == nil || doc == nil {
		return nil
	}
	docPath, ok := URIToFilePath(doc.URI)
	if !ok {
		return nil
	}
	docDir := filepath.Dir(docPath)
	var (
		errs       []schema.CompileError
		fileConfig *workspaceconfig.Config
	)
	rootPath, _ := URIToFilePath(s.ws.workspaceRoot)
	if docDir == rootPath {
		errs = s.rootProtoFilesErrs.Snapshot()
		fileConfig = s.SnapshotFileConfig()
	} else if sw, ok := s.SubWorkspaceByDir(docDir); ok && sw != nil {
		sw.lookupMu.RLock()
		if len(sw.CompileErrors) > 0 {
			errs = make([]schema.CompileError, len(sw.CompileErrors))
			copy(errs, sw.CompileErrors)
		}
		sw.lookupMu.RUnlock()
		fileConfig = sw.FileConfig
	}
	if len(errs) == 0 {
		return nil
	}
	anchor := protoFilesBlockAnchor(fileConfig)
	out := make([]protocol.Diagnostic, 0, len(errs))
	for _, e := range errs {
		out = append(out, protocol.Diagnostic{
			Range:    anchor,
			Severity: protocol.DiagnosticSeverityError,
			Source:   "protolsp/proto_files",
			Message:  e.String(),
		})
	}
	return out
}

// protoFilesBlockAnchor returns the LSP Range pointing at the
// `proto_files { ... }` block recorded in the parsed workspace
// config. Falls back to (1,1) when no block position is available
// — better to anchor at the top of the file than to drop the
// diagnostic entirely.
func protoFilesBlockAnchor(fc *workspaceconfig.Config) protocol.Range {
	if fc != nil && fc.ProtoFiles.Line > 0 {
		return rangeFromLineCol(fc.ProtoFiles.Line, fc.ProtoFiles.Column)
	}
	return rangeFromLineCol(1, 1)
}

// workspaceConfigDiagnostics runs the dedicated workspaceconfig
// parser against the source and converts its ParseWarnings to LSP
// Warning-severity diagnostics. The parser handles its own
// errors (anything that fails to parse as PXF surfaces via the
// generic parse path); we only emit the structural / semantic
// warnings here.
func (s *Workspace) workspaceConfigDiagnostics(source []byte) []protocol.Diagnostic {
	_, warnings, err := workspaceconfig.ParseWithEnv(source, s.lookupEnv)
	if err != nil {
		// Unparseable PXF — the generic parse-diagnostic path
		// already surfaces this. Don't double-report.
		return nil
	}
	out := make([]protocol.Diagnostic, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, protocol.Diagnostic{
			Range:    rangeFromLineCol(w.Line, w.Column),
			Severity: protocol.DiagnosticSeverityWarning,
			Source:   "protolsp/workspaceconfig",
			Message:  w.Message,
		})
	}
	return out
}

// chainSpecDiagnostics runs the dedicated chain parser against
// the source and converts its Errors to LSP Warning-severity
// diagnostics. Same shape as workspaceConfigDiagnostics.
//
// path is used by chain.ParseAt as anchoring metadata only —
// expansion + ancestry computation don't depend on it for
// chain-spec-internal diagnostics.
//
// After the parser-level diagnostics, emits a Hint per layer whose
// path template, expanded across every declared environment, points
// at no file that actually exists on disk. Triggered the
// "secrets-prod uses `layers/` instead of `secrets/`"-style typos
// that silently no-op'd before (chameleon skips missing ONCE layers
// when `required = false`, so the misconfig produced no error
// signal at all).
func chainSpecDiagnostics(path string, source []byte) []protocol.Diagnostic {
	ac, err := chain.ParseAt(path, source)
	if err != nil || ac == nil {
		return nil
	}
	out := make([]protocol.Diagnostic, 0, len(ac.Errors))
	for _, d := range ac.Errors {
		out = append(out, protocol.Diagnostic{
			Range:    rangeFromLineCol(d.Line, d.Column),
			Severity: protocol.DiagnosticSeverityWarning,
			Source:   "protolsp/chainspec",
			Message:  d.Message,
		})
	}
	out = append(out, missingLayerFileHints(path, ac)...)
	return out
}

// missingLayerFileHints emits one diagnostic per layer whose path
// template doesn't resolve to any extant file when expanded against
// every declared environment. Hint severity — these are "you probably
// meant something different here" findings, not hard errors, because
// chameleon supports lazy-by-design layers (a ONCE layer with
// `${env}` that exists for only one env is a legitimate pattern: the
// path expands per env, missing ones are silently skipped at
// resolution).
//
// Skipped scenarios:
//   - source == "env" / path == "(env)": sentinel non-file layer
//     (env-source materializes from process env vars, not disk).
//   - chain.pxf isn't on disk yet (Filename returned ""): nothing to
//     anchor relative paths to.
//   - The chain spec has no environments declared: no env matrix to
//     test against.
//
// The check uses os.Stat directly; runs on every chain.pxf analyze
// (typically a handful of stats per save) so the cost is negligible.
func missingLayerFileHints(chainPath string, ac *chain.ActiveChain) []protocol.Diagnostic {
	if ac == nil || ac.Spec == nil || chainPath == "" {
		return nil
	}
	chainDir := filepath.Dir(chainPath)
	if chainDir == "" || chainDir == "." {
		return nil
	}
	// Build the env list once; expansion for ONCE+${env} layers
	// tries each env name.
	envNames := make([]string, 0, len(ac.Spec.Environments))
	for _, e := range ac.Spec.Environments {
		envNames = append(envNames, e.Name)
	}
	if len(envNames) == 0 {
		return nil
	}

	var out []protocol.Diagnostic
	for _, le := range ac.Spec.Layers {
		if le.Source == "env" || le.Path == "(env)" {
			continue // sentinel; not a real file path.
		}
		strategy := strings.ToUpper(le.ForEach)
		if strategy == "" {
			strategy = "ONCE"
		}
		// Compute candidate absolute paths to stat. For PER_ENV +
		// for ONCE-with-${env}, that's one per env. For literal-path
		// ONCE layers, just the one.
		var candidates []string
		if strategy == "PER_ENV" || strings.Contains(le.Path, "${env}") {
			for _, name := range envNames {
				resolved := strings.ReplaceAll(le.Path, "${env}", name)
				candidates = append(candidates, filepath.Join(chainDir, resolved))
			}
		} else {
			candidates = []string{filepath.Join(chainDir, le.Path)}
		}
		anyExists := false
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				anyExists = true
				break
			}
		}
		if anyExists {
			continue
		}
		// Build message that names the layer + the resolved paths
		// it tried. Users debugging "why isn't my layer loading"
		// shouldn't have to mentally expand the template — list
		// the actual paths protolsp checked.
		expansions := make([]string, 0, len(candidates))
		for _, name := range envNames {
			expansions = append(expansions, strings.ReplaceAll(le.Path, "${env}", name))
		}
		// PER_ENV always uses every env; ONCE-with-${env} same.
		// Literal-path layer expansions == 1 entry with the path
		// unchanged — fall back to that.
		if strategy != "PER_ENV" && !strings.Contains(le.Path, "${env}") {
			expansions = []string{le.Path}
		}
		msg := fmt.Sprintf(
			"chain layer %q path %q resolves to no existing file (tried: %s) — typo, or files moved?",
			le.ID, le.Path, strings.Join(uniq(expansions), ", "),
		)
		out = append(out, protocol.Diagnostic{
			Range:    rangeFromLineCol(le.PathPos.Line, le.PathPos.Column),
			Severity: protocol.DiagnosticSeverityHint,
			Source:   "protolsp/chainspec",
			Message:  msg,
		})
	}
	return out
}

// uniq returns the input slice with duplicate consecutive entries
// removed (preserves order). Used to deduplicate expansion candidates
// when the path template doesn't actually contain ${env} but the
// branch lists every env anyway.
func uniq(in []string) []string {
	if len(in) <= 1 {
		return in
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// rangeFromLineCol converts a 1-indexed (line, col) pair from the
// PXF parsers into a zero-width LSP Range. (line=0, col=0) — the
// sentinel meaning "no specific position" the parsers use for
// document-wide issues like a missing @type — anchors at the top
// of the doc.
func rangeFromLineCol(line, col int) protocol.Range {
	if line < 1 {
		line = 1
	}
	if col < 1 {
		col = 1
	}
	pos := protocol.Position{
		Line:      uint32(line - 1), // #nosec G115 -- bounded by sane source positions
		Character: uint32(col - 1),  // #nosec G115 -- bounded by sane source positions
	}
	return protocol.Range{Start: pos, End: pos}
}
