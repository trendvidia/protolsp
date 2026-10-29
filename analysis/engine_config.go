// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Engine-configuration surface (#260 §3). A protowire project's
// engine selection and engine-level knobs live in a single
// protowire.config.textproto at the project root, resolved by the
// RFC-001 §9.4 discovery/precedence rules. protocompile's
// engineconfig package is the shared reference loader — `pxf build`
// resolves through it, and protolsp resolving through anything else
// would hand the editor a different effective configuration than the
// build tool. This file adopts the shared loader and exposes the
// result over a custom request; nothing protolsp-specific is layered
// on the semantics.
//
// Resolution is per request, not cached: Load is a stat-walk up the
// directory tree plus one file read, and re-resolving every time
// means the answer always matches what `pxf build` would see on the
// current disk state — config edits need no invalidation hook.

package analysis

import (
	"github.com/trendvidia/protocompile/engineconfig"
)

// MethodEngineConfig is the custom LSP request editors call to read
// the workspace's effective engine configuration — the same answer
// `pxf build` resolves via RFC-001 §9.4 (nearest
// protowire.config.textproto walking up from the workspace root,
// PROTOWIRE_CONFIG, built-in defaults). pxfed and the VS Code
// extension render this next to the registry status so schema
// authors see which engine and libraries their expressions are
// checked against.
//
// Request:  no params (or null).
// Response: [EngineConfigResult]. Never a request error — a
// malformed configuration file is a state to report (Error field),
// not a protocol failure.
const MethodEngineConfig = "protolsp/engineConfig"

// EngineConfigResult is the response shape for [MethodEngineConfig]:
// the effective configuration after every §9.4 precedence tier, plus
// where it came from. Mirrors engineconfig.Config field-for-field
// with enums rendered as their canonical strings.
type EngineConfigResult struct {
	// Configured reports whether any configuration file was resolved
	// (discovered, named by PROTOWIRE_CONFIG, or explicit). False
	// means Engine/DefaultMode/… are the built-in defaults.
	Configured bool `json:"configured"`
	// Path is the resolved configuration file; empty on defaults.
	Path string `json:"path,omitempty"`
	// Source names the winning precedence tier: "defaults",
	// "discovered", "env", or "flag".
	Source string `json:"source"`
	// Error carries the loader's message when resolution failed (an
	// unreadable or unparseable file, an invalid execution mode —
	// hard errors per §9.4, never silently defaulted). The other
	// fields are zero when set.
	Error string `json:"error,omitempty"`

	// Engine is whatever a configuration source set, and empty
	// otherwise: RFC-001 §9.4 reserved the field (protowire#282, the
	// expression language is fixed by §5.4), so there is no default.
	Engine            string   `json:"engine,omitempty"`
	FunctionLibraries []string `json:"functionLibraries,omitempty"`
	CatalogLibraries  []string `json:"catalogLibraries,omitempty"`
	DefaultMode       string   `json:"defaultMode,omitempty"`
	MaxRecursionDepth uint32   `json:"maxRecursionDepth,omitempty"`
	StrictValidation  bool     `json:"strictValidation,omitempty"`
}

// EngineConfig resolves the workspace's effective engine
// configuration through the shared §9.4 loader. Discovery starts at
// the workspace root (the LSP twin of the CLI's working directory);
// with no file:// root installed, the loader falls back to the
// server process's working directory, same as the CLI. KnownEngines
// stays nil — protolsp reports the configured engine, it doesn't
// execute one, so an engine name protolsp has never heard of is the
// editor's information rather than protolsp's error.
func (s *Workspace) EngineConfig() EngineConfigResult {
	var dir string
	if root := s.ws.WorkspaceRoot(); root != "" && IsFileURI(root) {
		dir = root.Filename()
	}
	cfg, err := engineconfig.Load(engineconfig.Options{Dir: dir})
	if err != nil {
		return EngineConfigResult{Error: err.Error()}
	}
	return EngineConfigResult{
		Configured:        cfg.Source != engineconfig.SourceDefaults,
		Path:              cfg.Path,
		Source:            cfg.Source.String(),
		Engine:            cfg.Engine,
		FunctionLibraries: cfg.FunctionLibraries,
		CatalogLibraries:  cfg.CatalogLibraries,
		DefaultMode:       cfg.DefaultMode.String(),
		MaxRecursionDepth: cfg.MaxRecursionDepth,
		StrictValidation:  cfg.StrictValidation,
	}
}
