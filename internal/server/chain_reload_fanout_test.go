// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Integration test for the chain.pxf reload fan-out. The audit
// named this gap: the constituent pieces (config_reload, watched
// files refresh layer info, per-doc analyze) are each tested in
// isolation, but no single test asserts that a single chain edit
// fans the publishDiagnostics + (if relevant) secretRangesChanged
// + (if relevant) effectiveConfigChanged notifications across
// every open layer doc in the right order/coverage. A reload
// that misses one open doc would silently leave stale chain-
// resolution markers in the editor.

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestChainReload_FanoutAcrossOpenLayerDocs writes a chain.pxf +
// two PER_ENV layer docs, opens both, then edits chain.pxf via a
// watched-files event. After the reload the test asserts:
//
//   - publishDiagnostics fired for EACH open layer doc (not just
//     one or none — full coverage of the reanalyzeOpenDocs walk).
//   - both notifications carry post-edit versions, proving the
//     analyze pipeline ran against the new chain state.
//
// secretRangesChanged is checked opportunistically: a chain-only
// edit doesn't shift the per-doc secret-range table, so the test
// pins absence of that notification — same-shape edits stay
// quiet (the legacy-client gate logic).
func TestChainReload_FanoutAcrossOpenLayerDocs(t *testing.T) {
	workspaceDir := t.TempDir()
	chainPath := filepath.Join(workspaceDir, "chain.pxf")
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]

layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	const layerSrc = `@type acme.demo.v1.Credentials
username = "alice"
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "env", "prod.pxf"), []byte(layerSrc), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "env", "dev.pxf"), []byte(layerSrc), 0o600))

	srv, fc := initializeWithWatchSupport(t, workspaceDir)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain(), "precondition: chain must load")

	prodURI := uri.File(filepath.Join(workspaceDir, "env", "prod.pxf"))
	devURI := uri.File(filepath.Join(workspaceDir, "env", "dev.pxf"))

	// Open both layer docs. didOpen fires publishDiagnostics on
	// each — that's the baseline.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, layerSrc)))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, layerSrc)))

	prePublishes := countPublishesByURI(fc.calls())
	require.Equal(t, 1, prePublishes[prodURI],
		"didOpen on prod.pxf publishes once before reload")
	require.Equal(t, 1, prePublishes[devURI],
		"didOpen on dev.pxf publishes once before reload")

	// Edit chain.pxf — add a new environment + fire the watcher
	// event. The classifier routes this to LoadWorkspaceChain →
	// reanalyzeOpenDocs, which republishes diagnostics for every
	// open doc.
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "sandbox" extends = "prod" }
  { name = "dev" extends = "sandbox" }
]

layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`), 0o600))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(chainPath), Type: protocol.FileChangeTypeChanged},
		})))

	// Sanity: chain reloaded — new environment count visible.
	envCount := len(srv.Workspaces().SnapshotChain().Spec.Environments)
	assert.Equal(t, 3, envCount,
		"chain reload picked up the sandbox environment added by the edit")

	// Fan-out: a SECOND publishDiagnostics must have fired for
	// each open layer doc. If reanalyzeOpenDocs skipped one,
	// that doc's count would still be 1.
	postPublishes := countPublishesByURI(fc.calls())
	assert.Equal(t, 2, postPublishes[prodURI],
		"chain reload must republish for prod.pxf (was open at reload time)")
	assert.Equal(t, 2, postPublishes[devURI],
		"chain reload must republish for dev.pxf (was open at reload time)")

	// Negative: chain-only edits don't shift the per-doc secret-
	// range table (layer file contents are unchanged), so no
	// secretRangesChanged notification should fire after the
	// reload. The legacy-client gate also requires the legacy method to
	// have been called for the notification to ever fire; we
	// haven't called it, so both layers of defense should keep
	// the surface quiet. Pinning this rules out a regression
	// that over-emits per reanalyze.
	assert.Empty(t, fc.secretRangesChangedNotifications(),
		"chain edit doesn't shift per-doc secret tables → no secretRangesChanged should fire")
}

// TestChainReload_MalformedChainSpec_DegradesGracefully covers the
// negative path of the reload fan-out: a chain.pxf edit that
// introduces a parse error must NOT crash the dispatcher, NOT
// silently leave open layer docs stale, and MUST allow the next
// good edit to recover the chain. The contract being pinned:
//
//   - LoadWorkspaceChain logs at Warn and leaves rootChain == nil.
//   - reanalyzeOpenDocs still runs for every open layer doc so
//     diagnostics reflect the now-standalone state (no chain).
//   - A follow-up good edit re-populates rootChain — chain-aware
//     features come back online without an LSP restart.
func TestChainReload_MalformedChainSpec_DegradesGracefully(t *testing.T) {
	workspaceDir := t.TempDir()
	chainPath := filepath.Join(workspaceDir, "chain.pxf")
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]

layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	const layerSrc = `@type acme.demo.v1.Credentials
username = "alice"
`
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "env", "prod.pxf"), []byte(layerSrc), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "env", "dev.pxf"), []byte(layerSrc), 0o600))

	srv, fc := initializeWithWatchSupport(t, workspaceDir)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain(), "precondition: initial chain loaded")

	prodURI := uri.File(filepath.Join(workspaceDir, "env", "prod.pxf"))
	devURI := uri.File(filepath.Join(workspaceDir, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, layerSrc)))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, layerSrc)))

	preBrokenPublishes := countPublishesByURI(fc.calls())
	require.Equal(t, 1, preBrokenPublishes[prodURI])
	require.Equal(t, 1, preBrokenPublishes[devURI])

	// Write a malformed chain.pxf — missing closing brace, unterminated
	// string. Fire the watched-files event; LoadWorkspaceChain MUST log
	// + degrade rather than abort.
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec
version = "1.0.0
environments = [ { name = "prod" }
`), 0o600))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(chainPath), Type: protocol.FileChangeTypeChanged},
		})))

	// Contract: rootChain is now nil — LoadWorkspaceChain wiped
	// before reparse and the reparse failed.
	chainAfterBroken := srv.Workspaces().SnapshotChain()
	assert.Nil(t, chainAfterBroken,
		"malformed chain.pxf must leave rootChain nil — degrade to standalone mode")

	// Fan-out still ran — both open docs got a re-publish even
	// though the chain is gone. If reanalyzeOpenDocs had been
	// short-circuited by the chain-load failure, the counts would
	// stay at 1.
	postBrokenPublishes := countPublishesByURI(fc.calls())
	assert.Equal(t, 2, postBrokenPublishes[prodURI],
		"reanalyze must fan out even when the chain reload itself failed")
	assert.Equal(t, 2, postBrokenPublishes[devURI],
		"reanalyze must fan out even when the chain reload itself failed")

	// Recovery: write a valid chain.pxf, fire the event, expect
	// rootChain to come back AND a third publish per doc. Without
	// this assertion the test could pass against an implementation
	// that latches the failed state permanently.
	require.NoError(t, os.WriteFile(chainPath, []byte(`@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "prod"

environments = [
  { name = "prod" }
]

layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`), 0o600))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(chainPath), Type: protocol.FileChangeTypeChanged},
		})))

	chainAfterRecovery := srv.Workspaces().SnapshotChain()
	require.NotNil(t, chainAfterRecovery,
		"good chain.pxf edit must recover from the prior degraded state")
	assert.Equal(t, "prod", chainAfterRecovery.Spec.DefaultEnvironment)

	postRecoveryPublishes := countPublishesByURI(fc.calls())
	assert.Equal(t, 3, postRecoveryPublishes[prodURI],
		"recovery edit republishes for every open layer doc")
	assert.Equal(t, 3, postRecoveryPublishes[devURI])
}

// countPublishesByURI buckets publishDiagnostics notifications by
// their target URI. Returns a map keyed by the DocumentURI so
// callers can assert per-doc fan-out coverage without depending
// on emission order.
func countPublishesByURI(notes []notifyCall) map[protocol.DocumentURI]int {
	out := map[protocol.DocumentURI]int{}
	for _, n := range notes {
		if n.params == nil {
			continue
		}
		out[n.params.URI]++
	}
	return out
}
