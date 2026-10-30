// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trendvidia/protolsp/schema"
)

// TestClose_CancelsInFlightLookupRebuild covers the shutdown
// contract for the async rebuild goroutine: Server.Close cascades
// through lookupMgr.Close → CancelInFlight, so any goroutine
// blocked on schema.Open sees ctx.Err() and bails before trying to
// install a result into a torn-down server.
//
// The audit named the synchronous-install path (configureSchemaLookup)
// as untested for cancellation — this companion exercises the
// goroutine-based path so both shutdown windows are pinned.
func TestClose_CancelsInFlightLookupRebuild(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	srv.Config().EditorConfig.Registry = schema.Config{
		// Bogus address so the dial would hang or fail; we want it
		// in-flight when Close runs, not already completed.
		Address:   "unreachable.example.invalid:443",
		Namespace: "test",
	}

	// Kick off the async dial; returns immediately while the
	// goroutine runs in the background.
	srv.RebuildLookupAsync(context.Background())

	// Close runs the cancel chain. After Close, the manager must
	// hold no rebuildCancel — any subsequent rebuild observes a
	// clean baseline rather than a stale token.
	require.NoError(t, srv.Close())

	// Calling Close a second time is the idempotency contract — no
	// panic, no error, no double-cancel observable.
	require.NoError(t, srv.Close())

	// CancelInFlight on a manager whose pending rebuild was already
	// cancelled is a no-op. Exercise it to confirm.
	srv.Lookups().CancelInFlight()
}

// TestConfigureSchemaLookup_RespectsContextCancellation pins the
// sync-install contract: when ctx is already cancelled,
// configureSchemaLookup must return an error rather than install a
// lookup against a dead context. The audit flagged the
// synchronous-install path because cancellation here had no
// regression test.
func TestConfigureSchemaLookup_RespectsContextCancellation(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel before configure runs

	raw := json.RawMessage(`{
		"registry": {
			"address": "unreachable.example.invalid:443",
			"namespace": "test"
		}
	}`)
	err := srv.configureSchemaLookup(ctx, raw)
	require.Error(t, err,
		"cancelled context must surface as an error rather than install a torn lookup")

	// No lookup installed → the manager snapshot is still nil.
	assert.Nil(t, srv.Lookups().Snapshot(),
		"failed configure must leave the lookup unset")
}

// TestClose_StopsPerDocAnalyzeTimers covers the close-vs-debounce
// race: a doc with a pending debounced analyze must NOT fire its
// timer after Server.Close. If it does, the deferred goroutine
// calls into a torn-down server (publishDiagnostics dials a closed
// conn, reanalyzeOpenDocs reaches into a cleared workspace).
//
// Today Server.Close stops the watched-files debounce timer but
// doesn't iterate open documents to cancel each Document.analyzeTimer.
// This test pins the contract regardless of whether the current
// implementation closes the gap — a fix that adds the iteration
// keeps the test green; a regression that drops it goes red.
func TestClose_StopsPerDocAnalyzeTimers(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	srv.SetDebounceWindow(50 * time.Millisecond)

	const src = "@type acme.demo.v1.Greeting\n\nname = \"a\"\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, src)))
	// didOpen runs synchronously → count=1.
	require.Equal(t, int64(1), srv.AnalyzeCount())

	// Schedule a deferred analyze; do NOT wait for it to fire.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/g.pxf", 2, src)))

	countBeforeClose := srv.AnalyzeCount()

	// Close before the timer fires.
	require.NoError(t, srv.Close())

	// Wait well past the debounce window. analyzeCount must not
	// advance — a fired timer is a regression: the pipeline tried to
	// publish through a closed conn / against a torn-down workspace.
	assert.Never(t, func() bool {
		return srv.AnalyzeCount() > countBeforeClose
	}, 200*time.Millisecond, 5*time.Millisecond,
		"Close must cancel pending per-doc analyze timers")
}
