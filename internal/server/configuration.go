// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// This file implements the standard LSP configuration plumbing from
// the config-protocol design (CP1): server pulls Tier 3 (editor
// settings) via workspace/configuration; client notifies the server
// of changes via workspace/didChangeConfiguration; server registers
// for that notification via client/registerCapability.
//
// initializationOptions continues to seed Tier 3 — see
// configureSchemaLookup in server.go. When the client returns values
// for the same sections via workspace/configuration, those values
// override the seed for the duration of the session. Editors that
// don't implement workspace/configuration (rare in practice) get
// today's seed-only behavior.

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// configurationSections enumerates everything the server pulls in one
// workspace/configuration round-trip. Order matters: the client
// returns values in the same order; we map back by index. Section
// names live in the analysis package (aliased in aliases.go) so the
// effective-config resolver can share them.
var configurationSections = []string{
	sectionRegistryAddress,
	sectionRegistryNamespace,
	sectionRegistryToken,
	sectionRegistryCacheDir,
	sectionSecurityInlineSecrets,
}

// registerDynamicCapabilities sends client/registerCapability for the
// dynamic registrations protolsp needs — currently just
// workspace/didChangeConfiguration. The call is fire-and-check-error;
// clients that lack support return MethodNotFound and we log + move
// on. Editors that ALREADY send didChangeConfiguration unsolicited
// (most do — VS Code among them) keep working even when registration
// fails.
//
// Future registrations (workspace/didChangeWatchedFiles for
// .protolsp.pxf) land here too as their features come online.
func (s *Server) registerDynamicCapabilities(ctx context.Context) {
	if s.conn == nil {
		return
	}
	var regs []protocol.Registration
	if s.ClientCapabilities().DidChangeConfigurationDynamic {
		regs = append(regs, protocol.Registration{
			ID:     "protolsp.workspace.didChangeConfiguration",
			Method: protocol.MethodWorkspaceDidChangeConfiguration,
		})
	}
	// W5: register file watchers for .protolsp.pxf + the chain spec
	// path (if a chain is configured). Without dynamic registration,
	// config / chain reloads require an LSP restart — log + continue.
	if s.ClientCapabilities().DidChangeWatchedFilesDynamic {
		regs = append(regs, watchedFilesRegistration(s.ResolvedChainSpecPath()))
	}
	if len(regs) == 0 {
		return
	}
	params := &protocol.RegistrationParams{Registrations: regs}
	if _, err := s.conn.Call(ctx, protocol.MethodClientRegisterCapability, params, nil); err != nil {
		s.Logger().Warn("client/registerCapability failed; relying on unsolicited notifications",
			slog.String("err", err.Error()),
		)
	}
}

// watchedFilesRegistration builds the FileSystemWatcher set
// protolsp needs to keep its workspace state current (W5):
//
//   - `**/.protolsp.pxf` — the workspace config file. Watched
//     workspace-wide so a future multi-root walk (W4) picks up
//     additions without needing re-registration.
//   - `**/chain.pxf` — defensive catch-all for the conventional
//     chain spec filename. Most workspaces follow the convention;
//     this watcher fires for those even before fileConfig loads.
//   - The active chain spec's absolute path, when distinct from
//     the conventional name. Belt-and-braces: a workspace that
//     names its chain spec something else (e.g.
//     `myapp-chain.pxf`) still gets reload events.
//   - `**/*.proto` — proto sources used by both the schema
//     lookup (W2/W3 proto_files) and the proto-authoring phase 3
//     index. A single watch covers both consumers; the classifier
//     in watched_files.go decides which one(s) actually needs to
//     reload.
//   - `**/buf.yaml` + `**/buf.lock` — proto-authoring phase 3
//     step 5 (live invalidation). An edit to either rewrites the
//     proto index's include paths and triggers a full rebuild.
//   - `**/protowire.config.textproto` — the §9.4 engine config. An
//     edit re-derives the protocheck validator (function_libraries).
//     A config above the workspace root or named via
//     PROTOWIRE_CONFIG isn't watchable here; the other rebuild
//     triggers' fingerprint re-check picks those up eventually.
//
// All watchers default to all change types (created / changed /
// deleted) by leaving Kind unset — the LSP spec defaults to 7
// (Create | Change | Delete), which matches what we want here.
func watchedFilesRegistration(chainSpecAbsPath string) protocol.Registration {
	watchers := []protocol.FileSystemWatcher{
		{GlobPattern: "**/.protolsp.pxf"},
		{GlobPattern: "**/chain.pxf"},
		{GlobPattern: "**/*.proto"},
		{GlobPattern: "**/buf.yaml"},
		{GlobPattern: "**/buf.lock"},
		// Covers both the §9.4 engine config and §7 catalog library
		// files; the classifier matches catalogs against the loaded
		// set precisely, so unrelated textproto edits are dropped.
		{GlobPattern: "**/*.textproto"},
	}
	if chainSpecAbsPath != "" && filepath.Base(chainSpecAbsPath) != "chain.pxf" {
		watchers = append(watchers, protocol.FileSystemWatcher{
			GlobPattern: chainSpecAbsPath,
		})
	}
	return protocol.Registration{
		ID:     "protolsp.workspace.didChangeWatchedFiles",
		Method: protocol.MethodWorkspaceDidChangeWatchedFiles,
		RegisterOptions: protocol.DidChangeWatchedFilesRegistrationOptions{
			Watchers: watchers,
		},
	}
}

// pullEditorConfig sends workspace/configuration to the client and
// returns the editor's current values for the sections in
// configurationSections. Returns (nil, nil) when the client doesn't
// support workspace/configuration — callers fall back to whatever's
// already in s.Config().editorConfig (typically the initializationOptions
// seed).
//
// The shape: each client response entry is the value for the
// matching section (or null when the client has nothing). Order
// follows configurationSections.
func (s *Server) pullEditorConfig(ctx context.Context) (map[string]any, error) {
	if s.conn == nil || !s.ClientCapabilities().WorkspaceConfiguration {
		return nil, nil
	}
	items := make([]protocol.ConfigurationItem, 0, len(configurationSections))
	for _, sec := range configurationSections {
		items = append(items, protocol.ConfigurationItem{Section: sec})
	}
	var raw json.RawMessage
	if _, err := s.conn.Call(ctx, protocol.MethodWorkspaceConfiguration,
		&protocol.ConfigurationParams{Items: items}, &raw); err != nil {
		return nil, fmt.Errorf("workspace/configuration: %w", err)
	}
	var values []any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("unmarshalling workspace/configuration response: %w", err)
	}
	if len(values) != len(configurationSections) {
		return nil, fmt.Errorf("workspace/configuration returned %d values; want %d",
			len(values), len(configurationSections))
	}
	out := make(map[string]any, len(values))
	for i, sec := range configurationSections {
		out[sec] = values[i]
	}
	return out, nil
}

// applyEditorConfig merges a pulled or notified configuration map
// into the running server, rebuilding the schema lookup when
// registry-related sections changed. Empty / nil entries are treated
// as "client doesn't have a value here" and leave the prior value
// (typically the seed) in place — this matches LSP's optional-value
// convention.
//
// Returns true when the schema lookup was rebuilt (so callers can
// trigger downstream effects like reanalyzing open documents); the
// only error path is from schema.Open's dial / cache-load attempt.
func (s *Server) applyEditorConfig(ctx context.Context, values map[string]any) (rebuilt bool, err error) {
	if values == nil {
		return false, nil
	}
	prior := s.SnapshotEditorConfig()
	merged := prior // value-typed copy
	updateString(&merged.Registry.Address, values[sectionRegistryAddress])
	updateString(&merged.Registry.Namespace, values[sectionRegistryNamespace])
	updateString(&merged.Registry.Token, values[sectionRegistryToken])
	updateString(&merged.Registry.CacheDir, values[sectionRegistryCacheDir])
	updateString(&merged.InlineSecrets, values[sectionSecurityInlineSecrets])

	if merged == prior {
		return false, nil
	}

	// Only rebuild the schema lookup when the registry-defining
	// fields actually changed. The other fields (future: log level,
	// chameleon block) don't require rebuilding the resolver.
	registryChanged := prior.Registry != merged.Registry
	s.SwapEditorConfig(merged)

	if !registryChanged {
		return false, nil
	}

	// Hand off to the shared async-rebuild path. Returns immediately;
	// the actual schema.Open dial runs in a goroutine. Same diff +
	// cancellation logic the live-reload + file-watcher paths use,
	// so a flurry of didChangeConfiguration calls won't pile up
	// stale dials.
	s.RebuildLookupAsync(ctx)
	return true, nil
}

// updateString copies a non-empty string value from raw into dst.
// Nil / wrong-typed / empty values are treated as "client has no
// opinion, keep what we have" — matches LSP's optional-value
// convention for workspace/configuration responses.
func updateString(dst *string, raw any) {
	if raw == nil {
		return
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return
	}
	*dst = s
}

// dispatchDidChangeConfiguration handles
// workspace/didChangeConfiguration. The notification's payload is
// untrusted — different clients send different things, some send
// null. The robust pattern is to ignore the payload and re-pull
// fresh values via workspace/configuration. gopls and several other
// production servers do the same.
//
// The re-pull MUST happen on a goroutine, not inline on this handler.
// go.lsp.dev/jsonrpc2's conn.Go dispatches every incoming message on
// a single reader goroutine (conn.go: `go c.run(ctx, handler)` — no
// AsyncHandler, no separate response router). conn.Call writes the
// outbound request and then blocks on a channel that only the SAME
// reader goroutine can fill (via the `case *Response` branch in
// run()). Calling it inline would write the workspace/configuration
// request out, wait for the response forever, and freeze the
// dispatcher — every subsequent textDocument/didOpen, hover,
// definition, etc., queues in the OS pipe and never gets handled.
//
// The bug manifested in the locked-bw → paste-session → reopen flow:
// pxfed's registry-settings onChange listener fires after the
// in-memory bw session change, sends didChangeConfiguration, the
// handler deadlocks the dispatcher, and the user's subsequent
// reopen-prod.sops.pxf didOpen never gets analyzed. Symptom:
// "secrets not redacted + go-to-def doesn't work" despite the file
// being decrypted and bound in the editor.
//
// Mirrors the dispatchInitialize → initialized follow-up, which
// already wraps its pullEditorConfig in a goroutine for the same
// reason.
func (s *Server) dispatchDidChangeConfiguration(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	s.configChangeFollowup.Add(1)
	go func() {
		defer s.configChangeFollowup.Done()
		// Background context — the request's ctx is gone the moment
		// reply() returns to the dispatcher. pullEditorConfig +
		// applyEditorConfig take seconds at most (one round-trip plus
		// possibly an async lookup rebuild), so an indefinite ctx is
		// fine; cancellation comes from server shutdown via
		// s.Lookups().CancelInFlight on Close.
		bgCtx := context.Background()
		values, err := s.pullEditorConfig(bgCtx)
		if err != nil {
			s.Logger().Warn("workspace/configuration re-pull after didChangeConfiguration failed",
				slog.String("err", err.Error()),
			)
			return
		}
		if _, applyErr := s.applyEditorConfig(bgCtx, values); applyErr != nil {
			s.Logger().Warn("applying configuration change",
				slog.String("err", applyErr.Error()),
			)
		}
	}()
	// Notification — reply with nil unconditionally per LSP spec.
	return reply(ctx, nil, nil)
}

// notifyInitOptionsDeprecation surfaces a one-time
// window/showMessage notice at `initialized` when the client both
// (a) seeded registry config via the legacy
// `initializationOptions.registry.*` channel AND (b) advertises
// workspace/configuration capability — meaning the migration path
// is actually available to them. CP5 of the config-protocol
// design's migration plan.
//
// Skipped when:
//   - No init-options seed: nothing to deprecate. Fresh integrations
//     using workspace/configuration from day one stay quiet.
//   - Client lacks workspace/configuration capability: nudging them
//     toward a method they can't call is noise. Functionality is
//     unchanged regardless; the seed path still works.
//
// One-time-ness comes naturally from `initialized` firing once per
// session.
func (s *Server) notifyInitOptionsDeprecation(ctx context.Context) {
	if !s.SnapshotSeededFromInitOptions() || !s.ClientCapabilities().WorkspaceConfiguration {
		return
	}
	s.Publisher().ShowMessage(ctx, protocol.MessageTypeInfo,
		"protolsp: registry configuration via initializationOptions is deprecated. "+
			"The editor should adopt workspace/configuration; the seed path will be removed in a future release.")
}
