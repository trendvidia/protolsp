// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package workspaceconfig

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/trendvidia/protolsp/interp"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// ParseWarning is one item in the slice Parse returns alongside the
// Config. Treat as informational — Parse only returns an error when
// the file is unparseable as PXF at all; everything else (missing
// @type, unknown blocks, type-mismatched values) shows up as a
// warning so the editor can surface diagnostics without failing
// the load entirely.
type ParseWarning struct {
	Line    int
	Column  int
	Message string
}

// String formats a warning for log output: "line:col: <message>".
// When Line is 0 (the warning isn't tied to a specific position —
// e.g. "missing @type directive"), the position is omitted.
func (w ParseWarning) String() string {
	if w.Line == 0 {
		return w.Message
	}
	return fmt.Sprintf("%d:%d: %s", w.Line, w.Column, w.Message)
}

// Parse turns the content of a .protolsp.pxf into a Config plus any
// warnings encountered. Returns an error only for unparseable PXF
// content; structural / semantic issues surface as warnings so
// callers can log them and continue with a partial config.
//
// `${env.X}` references resolve through the process-global lookup
// (os.Getenv unless SetEnvLookup replaced it). Callers that own
// their env state — e.g. an analysis.Workspace with a per-session
// BW_SESSION override — should use ParseWithEnv instead so two
// workspaces in one process don't share a resolver.
func Parse(content []byte) (*Config, []ParseWarning, error) {
	return ParseWithEnv(content, nil)
}

// ParseWithEnv is Parse with an explicit `${env.X}` resolver. A nil
// getenv falls back to the process-global lookup (SetEnvLookup /
// os.Getenv), making ParseWithEnv(content, nil) identical to Parse.
func ParseWithEnv(content []byte, getenv func(name string) string) (*Config, []ParseWarning, error) {
	if getenv == nil {
		getenv = globalEnvLookup()
	}
	doc, err := pxf.Parse(content)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing .protolsp.pxf: %w", err)
	}
	if doc == nil {
		return nil, nil, errors.New("pxf.Parse returned nil document")
	}

	cfg := &Config{}
	var warnings []ParseWarning

	// @type directive sanity check. Missing or mismatched is a
	// warning, not an error — the config still loads.
	if doc.TypeURL == "" {
		warnings = append(warnings, ParseWarning{
			Message: "missing @type directive; expected `@type " + ExpectedType + "`",
		})
	} else if doc.TypeURL != ExpectedType {
		warnings = append(warnings, ParseWarning{
			Message: fmt.Sprintf("unexpected @type %q; expected %q", doc.TypeURL, ExpectedType),
		})
	}

	// Walk top-level entries. The canonical shape is direct
	// `registry { ... }` / `proto_files { ... }` blocks at the top
	// level alongside `security` and `chameleon`. The legacy
	// `schema_sources { ... }` wrapper is still accepted (with a
	// deprecation warning) so files committed before the 0.4.0
	// flattening keep loading.
	for _, entry := range doc.Entries {
		switch n := entry.(type) {
		case *pxf.Block:
			handleTopBlock(n, cfg, &warnings, getenv)
		case *pxf.Assignment:
			// Top-level scalar assignments (log_level, etc.) — future.
			warnings = append(warnings, ParseWarning{
				Line:    n.Pos.Line,
				Column:  n.Pos.Column,
				Message: fmt.Sprintf("ignoring unrecognized top-level assignment %q", n.Key),
			})
		}
	}

	cfg.HasContent = !cfg.Registry.IsEmpty() || !cfg.ProtoFiles.IsEmpty() || !cfg.Security.IsEmpty() || !cfg.Chameleon.IsEmpty()
	return cfg, warnings, nil
}

// handleTopBlock dispatches a top-level Block by name. Canonical
// blocks: `registry`, `proto_files`, `security`, `chameleon`. The
// legacy `schema_sources` wrapper is shimmed for back-compat.
func handleTopBlock(b *pxf.Block, cfg *Config, warnings *[]ParseWarning, getenv func(string) string) {
	switch b.Name {
	case "registry":
		handleRegistry(b, cfg, warnings, getenv)
	case "proto_files":
		handleProtoFiles(b, cfg, warnings, getenv)
	case "schema_sources":
		handleLegacySchemaSources(b, cfg, warnings, getenv)
	case "security":
		handleSecurity(b, cfg, warnings, getenv)
	case "chameleon":
		handleChameleon(b, cfg, warnings, getenv)
	default:
		*warnings = append(*warnings, ParseWarning{
			Line:    b.Pos.Line,
			Column:  b.Pos.Column,
			Message: fmt.Sprintf("ignoring unrecognized top-level block %q", b.Name),
		})
	}
}

// handleSecurity walks the `security { ... }` block, reading the
// inline_secrets policy + any future security knobs. Unrecognized
// fields warn but don't fail — keeps additive evolution painless.
//
// inline_secrets allowed values: "allow" | "warn" | "refuse".
// Other strings get a warning and the field stays unset so the
// consumer (Server's effective-policy lookup) falls back to the
// default.
func handleSecurity(b *pxf.Block, cfg *Config, warnings *[]ParseWarning, getenv func(string) string) {
	for _, entry := range b.Entries {
		assign, ok := entry.(*pxf.Assignment)
		if !ok {
			continue
		}
		val, ok := stringValue(assign.Value)
		if !ok {
			*warnings = append(*warnings, ParseWarning{
				Line:    assign.Pos.Line,
				Column:  assign.Pos.Column,
				Message: fmt.Sprintf("security.%s expects a string value", assign.Key),
			})
			continue
		}
		val = interpolate(val, getenv)
		switch assign.Key {
		case "inline_secrets":
			if !isValidInlineSecretsPolicy(val) {
				*warnings = append(*warnings, ParseWarning{
					Line:    assign.Pos.Line,
					Column:  assign.Pos.Column,
					Message: fmt.Sprintf("security.inline_secrets must be one of allow|warn|refuse; got %q", val),
				})
				continue
			}
			cfg.Security.InlineSecrets = val
		default:
			*warnings = append(*warnings, ParseWarning{
				Line:    assign.Pos.Line,
				Column:  assign.Pos.Column,
				Message: fmt.Sprintf("ignoring unrecognized security field %q", assign.Key),
			})
		}
	}
}

// handleChameleon walks the `chameleon { ... }` block from the
// chameleon-integration design (C1). Reads chain_spec (required to
// activate chain mode), schema_fqn (optional default @type for
// untyped layers), and enabled (defaults to true when chain_spec is
// set; only an explicit `enabled = false` flips it off).
//
// Sub-blocks aren't expected today; surface them as warnings so
// future additions (e.g. context inference defaults) don't silently
// disappear.
func handleChameleon(b *pxf.Block, cfg *Config, warnings *[]ParseWarning, getenv func(string) string) {
	sawChainSpec := false
	enabledExplicit := false
	for _, entry := range b.Entries {
		switch n := entry.(type) {
		case *pxf.Block:
			*warnings = append(*warnings, ParseWarning{
				Line:    n.Pos.Line,
				Column:  n.Pos.Column,
				Message: fmt.Sprintf("ignoring unrecognized chameleon sub-block %q", n.Name),
			})
		case *pxf.Assignment:
			switch n.Key {
			case "chain_spec":
				val, ok := stringValue(n.Value)
				if !ok {
					*warnings = append(*warnings, ParseWarning{
						Line:    n.Pos.Line,
						Column:  n.Pos.Column,
						Message: "chameleon.chain_spec expects a string value",
					})
					continue
				}
				cfg.Chameleon.ChainSpec = interpolate(val, getenv)
				sawChainSpec = true
			case "schema_fqn":
				val, ok := stringValue(n.Value)
				if !ok {
					*warnings = append(*warnings, ParseWarning{
						Line:    n.Pos.Line,
						Column:  n.Pos.Column,
						Message: "chameleon.schema_fqn expects a string value",
					})
					continue
				}
				cfg.Chameleon.SchemaFQN = interpolate(val, getenv)
			case "enabled":
				bv, ok := n.Value.(*pxf.BoolVal)
				if !ok {
					*warnings = append(*warnings, ParseWarning{
						Line:    n.Pos.Line,
						Column:  n.Pos.Column,
						Message: "chameleon.enabled expects a boolean value",
					})
					continue
				}
				cfg.Chameleon.Enabled = bv.Value
				enabledExplicit = true
			default:
				*warnings = append(*warnings, ParseWarning{
					Line:    n.Pos.Line,
					Column:  n.Pos.Column,
					Message: fmt.Sprintf("ignoring unrecognized chameleon field %q", n.Key),
				})
			}
		}
	}
	// Default enabled to true when chain_spec is set and the user
	// didn't write an explicit `enabled = ...`. Without chain_spec,
	// the block is inert; Enabled stays false.
	if sawChainSpec && !enabledExplicit {
		cfg.Chameleon.Enabled = true
	}
}

// isValidInlineSecretsPolicy is the canonical enum check for the
// inline_secrets policy value. Used by the parser (warn on bad
// file content) and by the server's setOverrides validator
// (reject bad Tier 4 push values).
func isValidInlineSecretsPolicy(v string) bool {
	switch v {
	case "allow", "warn", "refuse":
		return true
	}
	return false
}

// handleLegacySchemaSources walks the deprecated `schema_sources {
// registry { ... } }` / `schema_sources { proto_files { ... } }`
// wrapper. Each occurrence emits a deprecation warning, then
// dispatches to the canonical top-level handlers. Duplicate
// detection (multiple registry blocks, multiple proto_files
// blocks) is handled inside those handlers — no need for the
// "first wins" warning the legacy wrapper used to emit.
//
// Removed once the Set RPC has been in the wild long enough that
// every .protolsp.pxf out there has been rewritten to the flat
// shape by an Set call.
func handleLegacySchemaSources(b *pxf.Block, cfg *Config, warnings *[]ParseWarning, getenv func(string) string) {
	*warnings = append(*warnings, ParseWarning{
		Line:    b.Pos.Line,
		Column:  b.Pos.Column,
		Message: "`schema_sources { ... }` block is deprecated; hoist `registry` / `proto_files` to the top level",
	})
	for _, entry := range b.Entries {
		inner, ok := entry.(*pxf.Block)
		if !ok {
			continue
		}
		switch inner.Name {
		case "registry":
			handleRegistry(inner, cfg, warnings, getenv)
		case "proto_files":
			handleProtoFiles(inner, cfg, warnings, getenv)
		default:
			*warnings = append(*warnings, ParseWarning{
				Line:    inner.Pos.Line,
				Column:  inner.Pos.Column,
				Message: fmt.Sprintf("unknown schema source kind %q", inner.Name),
			})
		}
	}
}

// handleProtoFiles pulls the `paths = [...]` list out of a
// `proto_files { ... }` block. The block's position is recorded
// on the resulting ProtoFilesSource so compile errors raised
// during protocompile can anchor their diagnostics on the right
// region of .protolsp.pxf.
//
// Only one proto_files block applies per workspace: a second
// occurrence (whether via the legacy `schema_sources { ... }`
// wrapper or two top-level blocks) warns and is dropped — the
// first one wins.
//
// Empty paths skip the source entirely — a `proto_files { }`
// without any paths is effectively a no-op.
func handleProtoFiles(b *pxf.Block, cfg *Config, warnings *[]ParseWarning, getenv func(string) string) {
	if !cfg.ProtoFiles.IsEmpty() {
		*warnings = append(*warnings, ParseWarning{
			Line:    b.Pos.Line,
			Column:  b.Pos.Column,
			Message: "duplicate proto_files block; keeping the first one",
		})
		return
	}
	src := ProtoFilesSource{Line: b.Pos.Line, Column: b.Pos.Column}
	for _, entry := range b.Entries {
		assign, ok := entry.(*pxf.Assignment)
		if !ok {
			continue
		}
		switch assign.Key {
		case "paths":
			list, ok := assign.Value.(*pxf.ListVal)
			if !ok {
				*warnings = append(*warnings, ParseWarning{
					Line:    assign.Pos.Line,
					Column:  assign.Pos.Column,
					Message: "proto_files.paths expects a list of strings",
				})
				continue
			}
			for _, elem := range list.Elements {
				sv, ok := stringValue(elem)
				if !ok {
					*warnings = append(*warnings, ParseWarning{
						Line:    list.Pos.Line,
						Column:  list.Pos.Column,
						Message: "proto_files.paths entries must be strings",
					})
					continue
				}
				src.Paths = append(src.Paths, interpolate(sv, getenv))
			}
		default:
			*warnings = append(*warnings, ParseWarning{
				Line:    assign.Pos.Line,
				Column:  assign.Pos.Column,
				Message: fmt.Sprintf("ignoring unrecognized proto_files field %q", assign.Key),
			})
		}
	}
	if src.IsEmpty() {
		return
	}
	cfg.ProtoFiles = src
}

// handleRegistry pulls the four registry fields out of a registry
// block. Each field is a string assignment; missing fields are
// fine (zero value is the "default" tier in the config-protocol
// model). Non-string values warn.
//
// Only one registry block applies per workspace; a second
// occurrence warns and is dropped (mirrors handleProtoFiles).
func handleRegistry(b *pxf.Block, cfg *Config, warnings *[]ParseWarning, getenv func(string) string) {
	if !cfg.Registry.IsEmpty() {
		*warnings = append(*warnings, ParseWarning{
			Line:    b.Pos.Line,
			Column:  b.Pos.Column,
			Message: "duplicate registry block; keeping the first one",
		})
		return
	}
	for _, entry := range b.Entries {
		assign, ok := entry.(*pxf.Assignment)
		if !ok {
			continue
		}
		val, ok := stringValue(assign.Value)
		if !ok {
			*warnings = append(*warnings, ParseWarning{
				Line:    assign.Pos.Line,
				Column:  assign.Pos.Column,
				Message: fmt.Sprintf("registry.%s expects a string value", assign.Key),
			})
			continue
		}
		val = interpolate(val, getenv)
		switch assign.Key {
		case "address":
			cfg.Registry.Address = val
		case "namespace":
			cfg.Registry.Namespace = val
		case "token":
			cfg.Registry.Token = val
		case "cache_dir":
			cfg.Registry.CacheDir = val
		default:
			*warnings = append(*warnings, ParseWarning{
				Line:    assign.Pos.Line,
				Column:  assign.Pos.Column,
				Message: fmt.Sprintf("ignoring unrecognized registry field %q", assign.Key),
			})
		}
	}
}

// stringValue extracts the underlying string from a pxf.Value when
// it's a StringVal. Returns ok=false for any other value type.
func stringValue(v pxf.Value) (string, bool) {
	if sv, ok := v.(*pxf.StringVal); ok {
		return sv.Value, true
	}
	return "", false
}

// lookupEnv is the env-variable resolver interpolate uses for
// `${env.X}` references. Defaults to os.Getenv; the LSP server
// swaps it via SetEnvLookup so renderer-side state (e.g. a
// pasted BW_SESSION token from pxfed's in-memory Bitwarden
// session field) can shadow the process env without requiring
// a protolsp restart.
//
// Mutex-protected so the LSP notification that updates the
// lookup can race safely against in-flight Parse calls — the
// reads in interpolate take the RLock once per Parse, the
// writes in SetEnvLookup are rare (once per pxfed session
// refresh).
var (
	lookupEnvMu sync.RWMutex
	lookupEnv   func(name string) string = os.Getenv
)

// SetEnvLookup installs fn as the process-global resolver for
// `${env.X}` references. Passing nil restores the default
// os.Getenv. Returns the previously-installed function so callers
// can chain or restore.
//
// Deprecated-ish: the global affects every Parse call in the
// process, so two independent embedders fight over it (#180).
// Prefer ParseWithEnv, which scopes the resolver to one call;
// this stays for single-instance CLI-style callers.
func SetEnvLookup(fn func(name string) string) func(name string) string {
	lookupEnvMu.Lock()
	defer lookupEnvMu.Unlock()
	prev := lookupEnv
	if fn == nil {
		lookupEnv = os.Getenv
	} else {
		lookupEnv = fn
	}
	return prev
}

// globalEnvLookup snapshots the process-global resolver under the
// read lock. ParseWithEnv falls back to it when the caller passes a
// nil getenv (i.e. the legacy Parse path).
func globalEnvLookup() func(name string) string {
	lookupEnvMu.RLock()
	defer lookupEnvMu.RUnlock()
	return lookupEnv
}

// interpolate resolves ${env.NAME} references in a string value
// against the caller-provided env lookup. Unset env vars expand to
// the empty string
// (matching chameleon's runtime behavior). References to
// anything other than single-segment `env.X` are left literal —
// future interpolation sources (vault, file, etc.) can grow here
// without breaking existing configs, and field references
// (`${a.b.c}`) flow through to the analyze pass untouched so
// chameleon-aware diagnostics + go-to-definition see them.
//
// Uses the shared scanner in internal/interp; the env-specific
// filter lives here so this package owns the env-substitution
// policy.
func interpolate(s string, getenv func(name string) string) string {
	return interp.ReplaceAll(s, func(path string) (string, bool) {
		if !strings.HasPrefix(path, "env.") {
			return "", false
		}
		name := strings.TrimPrefix(path, "env.")
		// Reject multi-segment env paths (`env.A.B`) so they get
		// left literal rather than silently substituted with the
		// empty string. The old envRefPattern's
		// `[A-Za-z_][A-Za-z0-9_]*` segment match did this
		// implicitly; the shared scanner accepts dotted paths so
		// we filter explicitly.
		if strings.Contains(name, ".") {
			return "", false
		}
		return getenv(name), true
	})
}
