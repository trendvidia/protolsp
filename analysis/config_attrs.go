// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Three custom LSP requests that let an editor / pxfed / the
// hosted UI maintain `.protolsp.pxf` as a flat key/value store
// without parsing PXF themselves:
//
//   - protolsp/configList — enumerate the settable attributes
//     (name, description, kind).
//   - protolsp/configGet  — read one attribute's literal value
//     from the file.
//   - protolsp/configSet  — write one attribute's value back to
//     the file, preserving comments + structure via an AST-edit
//     round-trip.
//
// Workspace scoping: each call carries an optional workspaceDir.
// Empty means "the root workspace"; otherwise it must name one
// of the known sub-workspaces (W4). Cross-workspace calls are
// rejected — pxfed never sees `.protolsp.pxf` (it filters the
// file from listings) so callers must already know which
// workspace they're editing.
//
// "Literal value" means no ${env.X} interpolation: Get returns
// the raw string that's stored in the file, Set writes the raw
// string verbatim. Interpolation is a render-time concern the
// LSP applies on its own when assembling the effective config.
// Without that property Get→Set round-trips would corrupt
// `${env.TOKEN}` style values into their resolved form.

package analysis

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"errors"

	"github.com/trendvidia/protolsp/workspaceconfig"
)

const (
	// MethodConfigList enumerates settable workspace attributes.
	// Request: no params (or null). Response: ConfigListResult.
	MethodConfigList = "protolsp/configList"

	// MethodConfigGet reads one attribute's literal value.
	// Request: ConfigGetParams. Response: ConfigGetResult.
	MethodConfigGet = "protolsp/configGet"

	// MethodConfigSet writes one attribute's value back to disk.
	// Request: ConfigSetParams. Response: ConfigSetResult.
	MethodConfigSet = "protolsp/configSet"
)

// ConfigAttribute describes one settable attribute in a
// configList response. Kind values are stable across releases:
// "string", "string[]", "bool" — the client uses them to pick
// an input widget. displayName is the human-friendly label clients
// should render next to that widget.
type ConfigAttribute struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
}

// ConfigListResult is the response shape for [MethodConfigList].
// Attributes appear in the canonical declaration order from
// workspaceconfig.AttributeRegistry, which UIs render top-to-bottom.
type ConfigListResult struct {
	Attributes []ConfigAttribute `json:"attributes"`
}

// ConfigGetParams is the request shape for [MethodConfigGet].
// WorkspaceDir is optional; empty selects the root workspace.
type ConfigGetParams struct {
	WorkspaceDir string `json:"workspaceDir,omitempty"`
	Name         string `json:"name"`
}

// ConfigGetResult is the response shape for [MethodConfigGet].
// Value is the literal JSON encoding of the attribute's stored
// value: a string, []string, or bool. Present=false when the
// attribute is not set in the file — Value is the zero value
// for its Kind in that case, so clients can blind-render
// without an extra nil check.
type ConfigGetResult struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Value   any    `json:"value"`
	Present bool   `json:"present"`
}

// ConfigSetParams is the request shape for [MethodConfigSet].
// Value's JSON type must match the attribute's Kind (string,
// list of strings, or bool). RawMessage lets us decode lazily
// against the kind we look up in the registry.
type ConfigSetParams struct {
	WorkspaceDir string          `json:"workspaceDir,omitempty"`
	Name         string          `json:"name"`
	Value        json.RawMessage `json:"value"`
}

// ConfigSetResult is the response shape for [MethodConfigSet].
// OK=true means the file was rewritten and the reload cascade
// fired. Errors flow through the JSON-RPC error channel, not as
// a false OK — the bool is here for client ergonomics.
type ConfigSetResult struct {
	OK bool `json:"ok"`
}

// ErrNoRootWorkspace reports that ConfigPathForWorkspace was asked
// for the root workspace's config but no root is configured. Server-
// side condition — transports map it to their internal/precondition
// error code.
var ErrNoRootWorkspace = errors.New("no root workspace configured")

// ErrUnknownWorkspaceDir reports that ConfigPathForWorkspace was
// given a directory that isn't a registered sub-workspace. Caller-
// side condition — transports map it to their invalid-argument code.
var ErrUnknownWorkspaceDir = errors.New("not a registered sub-workspace")

// ConfigPathForWorkspace resolves workspaceDir to the absolute
// `.protolsp.pxf` path for the workspace. Empty = root workspace;
// otherwise the dir must match a known sub-workspace (rejecting
// arbitrary paths protects against editor extensions accidentally
// targeting a workspace that protolsp isn't managing).
//
// Errors wrap ErrNoRootWorkspace / ErrUnknownWorkspaceDir so the
// transport shells (JSON-RPC dispatch, Connect-RPC handlers) can map
// them onto their own error-code vocabularies with errors.Is.
func (s *Workspace) ConfigPathForWorkspace(workspaceDir string) (string, error) {
	if workspaceDir == "" {
		rootPath, ok := URIToFilePath(s.ws.workspaceRoot)
		if !ok || rootPath == "" {
			return "", ErrNoRootWorkspace
		}
		return filepath.Join(rootPath, ".protolsp.pxf"), nil
	}
	if _, ok := s.SubWorkspaceByDir(workspaceDir); !ok {
		return "", fmt.Errorf("unknown workspaceDir %q (%w)", workspaceDir, ErrUnknownWorkspaceDir)
	}
	return filepath.Join(workspaceDir, ".protolsp.pxf"), nil
}

// ReloadAfterConfigEdit fires the same cascade dispatchDidChangeWatchedFiles
// runs after a `.protolsp.pxf` edit. Centralised here so configSet's
// effect on lookups, diagnostics, and the chain is identical to what
// an external editor save would do — pxfed shouldn't be able to tell
// whether a config change came from a file watcher or from its own
// Set call.
func (s *Workspace) ReloadAfterConfigEdit(ctx context.Context, workspaceDir string) {
	rootPath, _ := URIToFilePath(s.ws.workspaceRoot)
	if workspaceDir == "" || workspaceDir == rootPath {
		s.LoadWorkspaceFileConfig()
		// proto_files rebuild precedes the registry rebuild so the
		// dial goroutine sees the freshest proto_files snapshot for
		// its WithFallback wiring (matches watched_files.go order).
		s.RebuildRootProtoFilesLookup()
		s.RebuildLookupFromEffectiveConfig(ctx)
		s.LoadWorkspaceChain()
		// Phase 3 proto authoring: refresh the import-path index
		// alongside the schema lookup + chain so a buf.yaml /
		// .protolsp.pxf edit flows through to cross-file proto
		// resolution without an LSP restart.
		s.LoadProtoIndex()
		// Re-derive the protocheck validator: a config edit can
		// move the workspace's effective §9.4 engine config or flip
		// whether its function_libraries resolve. Fingerprint-diffed
		// internally; no-op in the OSS build.
		s.RebuildProtocheckValidator(ctx)
	} else {
		s.reloadSubWorkspace(workspaceDir)
	}
	s.reanalyzeOpenDocs(ctx)
}

// DecodeRawByKind decodes a JSON RawMessage into the Go value
// SetAttribute expects for kind. Centralises the type-tag → Go-type
// mapping so the AttributeKind ↔ wire-shape contract has exactly
// one definition.
func DecodeRawByKind(raw json.RawMessage, kind workspaceconfig.AttributeKind) (any, error) {
	switch kind {
	case workspaceconfig.KindString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("expected JSON string: %w", err)
		}
		return s, nil
	case workspaceconfig.KindStringList:
		var ss []string
		if err := json.Unmarshal(raw, &ss); err != nil {
			return nil, fmt.Errorf("expected JSON array of strings: %w", err)
		}
		return ss, nil
	case workspaceconfig.KindBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, fmt.Errorf("expected JSON bool: %w", err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("unsupported attribute kind %v", kind)
}
