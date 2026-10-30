// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Tier 3 (editor settings) value types + the client-capability
// snapshot. Moved here from internal/server so the ConfigState the
// Workspace owns can reference them without an import cycle; the
// LSP shell keeps the workspace/configuration pull/apply plumbing
// (configuration.go) and reads these via the analysis import.

package analysis

import (
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/schema"
)

// EditorConfig is the per-session view of Tier 3 settings the server
// consumes. Mirrors schema.Config plus any non-registry fields
// (security policy, future log-level overrides). Values are merged
// from initializationOptions and workspace/configuration pulls.
type EditorConfig struct {
	Registry schema.Config

	// InlineSecrets is the Tier 3 view of the security
	// inline-secrets policy from the secret-handling design (S4).
	// Empty when the editor hasn't supplied a value; lower tiers
	// (file, default) take over via ResolveSection.
	InlineSecrets string
}

// Section names the LSP shell pulls from the client. Each is a Tier 3
// override for the corresponding workspace setting.
//
// Names match the config-protocol design's dotted-path convention
// (rooted at `protolsp.`). Future sections — chameleon, dataset
// thresholds — will be added here as those features land.
const (
	SectionRegistryAddress       = "protolsp.registry.address"
	SectionRegistryNamespace     = "protolsp.registry.namespace"
	SectionRegistryToken         = "protolsp.registry.token"
	SectionRegistryCacheDir      = "protolsp.registry.cacheDir"
	SectionSecurityInlineSecrets = "protolsp.security.inline_secrets"
)

// ClientCapabilities captures the subset of LSP client capabilities
// protolsp's configuration plumbing cares about. Populated at
// initialize time and consulted in initialized + on every change.
//
// We intentionally don't track every flag — only the ones that gate
// our own behavior. Adding more later is a one-line change to read
// the flag plus the code path that uses it.
type ClientCapabilities struct {
	// WorkspaceConfiguration: client implements workspace/configuration.
	// When false, the server doesn't pull; the initializationOptions
	// seed sticks for the session.
	WorkspaceConfiguration bool

	// DidChangeConfigurationDynamic: client supports dynamic
	// registration of workspace/didChangeConfiguration via
	// client/registerCapability. When true we register and react to
	// notifications; when false we still accept the notification if
	// the client sends it unsolicited (most clients do).
	DidChangeConfigurationDynamic bool

	// DidChangeWatchedFilesDynamic: client supports dynamic
	// registration of workspace/didChangeWatchedFiles via
	// client/registerCapability. When false the server can't ask the
	// client to watch files at all — config / chain reloads then
	// require an LSP restart. W5 of workspace-config; degrades
	// gracefully on clients that don't support it.
	DidChangeWatchedFilesDynamic bool
}

// ReadClientCapabilities translates the relevant subset of
// InitializeParams.Capabilities into our internal struct.
func ReadClientCapabilities(p *protocol.InitializeParams) ClientCapabilities {
	var caps ClientCapabilities
	w := p.Capabilities.Workspace
	if w == nil {
		return caps
	}
	caps.WorkspaceConfiguration = isTrue(w.Configuration)
	if dc := w.DidChangeConfiguration; dc != nil {
		caps.DidChangeConfigurationDynamic = isTrue(dc.DynamicRegistration)
	}
	if dw := w.DidChangeWatchedFiles; dw != nil {
		caps.DidChangeWatchedFilesDynamic = isTrue(dw.DynamicRegistration)
	}
	return caps
}
