// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// Publisher receives every server→client push the analysis pipeline
// produces. It is the seam between the engine (analyze, validate,
// secret/dataset detection, effective-config resolution) and the
// transports that deliver results:
//
//   - the LSP + Connect-RPC server installs serverPublisher, which
//     fans out to jsonrpc2 notifications (gated per legacy
//     client) and to the sidecar's Subscribe streams;
//   - in-process embedders (goed) install their own
//     implementation and receive direct callbacks — no transport.
//
// Contract notes:
//
//   - Calls are best-effort fire-and-forget: implementations must
//     not return errors and must not block the analyze pipeline on
//     slow consumers (the RPC fan-out uses non-blocking buffered
//     channels for exactly this reason).
//   - Change detection has already happened: the engine only calls
//     the *Changed methods when the new snapshot differs from the
//     previous one.
//   - EffectiveConfigChanged receives changes with sensitive
//     sections already redacted (same shape both wire transports
//     historically sent); unredacted values are only reachable via
//     the pull path's explicit revealSecrets flag.
//   - Slices and maps are shared with engine state snapshots;
//     implementations must not mutate them.
type Publisher interface {
	// PublishDiagnostics delivers the full diagnostics set for one
	// document (empty slice clears). Secrets are already scrubbed.
	PublishDiagnostics(ctx context.Context, docURI uri.URI, version uint32, diags []protocol.Diagnostic)
	// ShowMessage surfaces a user-facing notice (stale schema,
	// truncated references walk, deprecation nudges).
	ShowMessage(ctx context.Context, typ protocol.MessageType, message string)
	// SecretRangesChanged fires when a re-analyze produced a
	// different pxf.Secret range table for the document.
	SecretRangesChanged(ctx context.Context, docURI uri.URI, version int32, ranges []SecretRange)
	// DatasetRegionsChanged fires when a re-analyze produced a
	// different @dataset region table for the document.
	DatasetRegionsChanged(ctx context.Context, docURI uri.URI, version int32, regions []DatasetRegion)
	// EffectiveConfigChanged fires when a setOverrides call (LSP or
	// RPC) changed any effective config value. Values in changes are
	// pre-redacted for sensitive sections.
	EffectiveConfigChanged(ctx context.Context, workspaceDir string, changes map[string]ConfigValue)
	// LookupChanged fires after a schema-lookup install or teardown
	// completes — async rebuild install, proto_files rebuild,
	// config-cleared teardown, shutdown Close — and after a rebuild
	// dial fails. On the failed-dial path the observable state is
	// unchanged (the previous lookup stays installed), so unlike the
	// *Changed methods above this can fire without a snapshot
	// difference: it means "a lookup transition finished; status
	// surfaces should re-read RegistryStatus / Snapshot / the
	// per-doc LookupForDoc". workspaceDir identifies whose lookup
	// transitioned: "" for the root workspace's (the
	// Lookups()/RegistryStatus surface), the sub-workspace directory
	// for a per-workspace lookup — consumers with
	// per-workspace surfaces refresh selectively on it. No further
	// payload, deliberately: it is a refresh signal, not a state
	// delta. Fires on the mutating goroutine (rebuild-dial and
	// watched-files debounce goroutines included); embedders
	// replacing a poll loop must not block in it.
	LookupChanged(ctx context.Context, workspaceDir string)
}
