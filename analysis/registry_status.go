// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"

	"go.lsp.dev/protocol"
)

// MethodRegistryStatus is the custom LSP request editors call to
// learn whether the bound resolver is currently serving from a live
// registry connection or from a disk cache. The VS Code extension,
// pxfed, and the future hosted Monaco IDE all read this to render a
// status-bar indicator ("Schema: live / Schema: stale (cached)").
//
// Request:  no params (or null).
// Response: { "stale": bool, "configured": bool }
//
//   - configured: whether any registry was wired at initialize time.
//     When false, the editor should show "no registry" rather than
//     either of the connected states.
//   - stale: true when the resolver fell back to disk cache because
//     the registry was unreachable. Recovering from stale requires
//     a restart (no live reconnection logic at this layer).
const MethodRegistryStatus = "protolsp/registryStatus"

// RegistryStatusResult is the response shape for [MethodRegistryStatus].
type RegistryStatusResult struct {
	Configured bool `json:"configured"`
	Stale      bool `json:"stale"`
}

// RegistryStatus reports whether a schema lookup is installed and
// whether it is serving from a stale disk cache. Never errors —
// this is status-badge surface, and "nothing configured" is a valid
// answer, not a failure. The composed recipe behind
// [MethodRegistryStatus] and the RPC sidecar's GetStatus; exported
// so in-process embedders share it instead of re-deriving status
// from Lookups().Snapshot().
func (s *Workspace) RegistryStatus() RegistryStatusResult {
	lk := s.lookupMgr.Snapshot()
	return RegistryStatusResult{
		Configured: lk != nil,
		Stale:      lk != nil && lk.IsStale(),
	}
}

// NotifyStaleSchemaIfApplicable sends a window/showMessage warning
// to the client when the resolver loaded stale at startup. Called
// from the `initialized` handler so the message lands after the
// LSP handshake has completed — clients (especially VS Code) drop
// notifications received before initialized.
//
// Best-effort: failures log but don't fail anything. Stale state
// is already visible via protolsp/registryStatus, so the message
// is a courtesy nudge, not a hard requirement.
func (s *Workspace) NotifyStaleSchemaIfApplicable(ctx context.Context) {
	lk := s.lookupMgr.Snapshot()
	if lk == nil || !lk.IsStale() {
		return
	}
	s.pub.ShowMessage(ctx, protocol.MessageTypeWarning,
		"protolsp: registry unreachable; serving schema from disk cache. "+
			"Restart the editor when the registry is reachable to refresh.")
}
