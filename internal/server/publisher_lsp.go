// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"log/slog"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// serverPublisher is the transport-side Publisher: it owns every
// jsonrpc2 conn.Notify the engine used to make inline, plus the
// Connect-RPC Subscribe fan-outs, plus the legacy-LSP-client gating
// that keeps RPC-only clients from paying doubled wire
// traffic. Engine code calls s.Publisher().X and stays transport-free.
//
// nil-conn tolerance: tests (and the window between New and
// AttachConn) run without a client connection; every LSP branch
// checks it. The RPC fan-outs run regardless — RPC-only sessions
// have subscribers but no interest in the legacy notifications.
type serverPublisher struct{ s *Server }

func (p serverPublisher) PublishDiagnostics(ctx context.Context, docURI uri.URI, version uint32, diags []protocol.Diagnostic) {
	if p.s.conn == nil {
		// Nowhere to send. The handler still maintains document
		// state correctly — the publish is a side-effect.
		return
	}
	if diags == nil {
		// LSP spec: an absent diagnostics field is illegal; an empty
		// slice clears prior squiggles. Normalize to empty.
		diags = []protocol.Diagnostic{}
	}
	params := &protocol.PublishDiagnosticsParams{
		URI:         docURI,
		Diagnostics: diags,
	}
	if version != 0 {
		params.Version = protocol.NewOptional(int32(version))
	}
	if err := p.s.conn.Notify(ctx, protocol.MethodTextDocumentPublishDiagnostics, params); err != nil {
		p.s.Logger().Warn("publishDiagnostics: notify failed", slog.String("err", err.Error()))
	}
}

func (p serverPublisher) ShowMessage(ctx context.Context, typ protocol.MessageType, message string) {
	if p.s.conn == nil {
		return
	}
	params := &protocol.ShowMessageParams{Type: typ, Message: message}
	if err := p.s.conn.Notify(ctx, protocol.MethodWindowShowMessage, params); err != nil {
		p.s.Logger().Warn("window/showMessage notify failed", slog.String("err", err.Error()))
	}
}

func (p serverPublisher) SecretRangesChanged(ctx context.Context, docURI uri.URI, version int32, ranges []SecretRange) {
	// Fan to every active Connect-RPC SecretsService.Subscribe
	// stream first. The publish step is non-blocking; lost events
	// on a slow consumer don't gate the LSP notification.
	p.s.publishSecretRanges(docURI, version, ranges)
	if p.s.conn == nil {
		return
	}
	// Skip the legacy LSP notification when no client this
	// session has called the matching legacy request method.
	// RPC-only clients (pxfed since pxfed#34, etc.) would otherwise
	// pay doubled wire traffic on every analyze pass.
	if !p.s.legacyLspClients.secretRanges.Load() {
		return
	}
	if err := p.s.conn.Notify(ctx, MethodSecretRangesChanged, &SecretRangesChangedParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
		Version:      version,
		Ranges:       ToSecretRangeEntries(ranges),
	}); err != nil {
		// Notifications are best-effort; log and move on.
		p.s.Logger().Warn("secretRangesChanged: notify failed",
			"err", err.Error())
	}
}

func (p serverPublisher) DatasetRegionsChanged(ctx context.Context, docURI uri.URI, version int32, regions []DatasetRegion) {
	// Fan to every active DatasetsService.Subscribe stream
	// alongside the LSP notification — same pattern as the
	// secrets path.
	p.s.publishDatasetRegions(docURI, version, regions)
	if p.s.conn == nil {
		return
	}
	// Skip the legacy LSP notification when no client this
	// session has called the matching legacy request method.
	if !p.s.legacyLspClients.datasetRegions.Load() {
		return
	}
	// Always send a non-nil slice on the wire.
	if regions == nil {
		regions = []DatasetRegion{}
	}
	if err := p.s.conn.Notify(ctx, MethodDatasetRegionsChanged, &DatasetRegionsChangedParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: docURI},
		Version:      version,
		Regions:      regions,
	}); err != nil {
		p.s.Logger().Warn("datasetRegionsChanged: notify failed",
			"err", err.Error())
	}
}

func (p serverPublisher) EffectiveConfigChanged(ctx context.Context, workspaceDir string, changes map[string]ConfigValue) {
	// Fan to every active EffectiveConfigService.Subscribe stream
	// for this workspace BEFORE the LSP notification path. The
	// changes map arrives pre-redacted from the engine; publish's
	// internal redaction is idempotent on it.
	p.s.publishEffectiveConfig(workspaceDir, changes)
	if p.s.conn == nil {
		return
	}
	// Skip the legacy LSP notification when no client this
	// session has touched the matching legacy request methods
	// (getEffectiveConfig / setOverrides).
	if !p.s.legacyLspClients.effectiveConfig.Load() {
		return
	}
	if err := p.s.conn.Notify(ctx, MethodEffectiveConfigChanged, &EffectiveConfigChangedParams{
		WorkspaceDir: workspaceDir,
		Changes:      changes,
	}); err != nil {
		p.s.Logger().Warn("effectiveConfigChanged notify failed",
			slog.String("err", err.Error()))
	}
}

// LookupChanged is a no-op on the wire transports: LSP clients and
// the RPC sidecar poll protolsp/registryStatus (once per client
// boot — protolsp never reconnects mid-session in that topology),
// and no protolsp/* notification method exists for lookup
// transitions. The seam exists for in-process embedders,
// which live across rebuilds and would otherwise poll.
func (p serverPublisher) LookupChanged(context.Context, string) {}
