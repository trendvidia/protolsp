// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

// waitForAnalyzeCount blocks until Server.analyzeCount reaches the
// target value, or the timeout expires. Returns true on success.
// Replaces sleep-based waits in debounce tests — the atomic counter
// is the canonical "pipeline has completed" signal.
func waitForAnalyzeCount(t *testing.T, srv *Server, target int64, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if srv.AnalyzeCount() >= target {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return srv.AnalyzeCount() >= target
}

func TestDebounce_ZeroWindowIsSynchronous(t *testing.T) {
	// Default debounce (0) → pipeline runs inside didChange.
	// analyzeCount jumps on every event with no delay.
	srv, _, _ := newServerWithLookup(t)
	require.Equal(t, time.Duration(0), srv.DebounceWindow())

	const src = "@type acme.demo.v1.Greeting\n\nname = \"a\"\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, src)))
	// didOpen runs the pipeline directly (not through schedule).
	// Subsequent didChange events with window=0 ALSO run inline.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/g.pxf", 2, src)))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/g.pxf", 3, src)))

	assert.Equal(t, int64(3), srv.AnalyzeCount(),
		"window=0 → each didOpen + didChange invokes the pipeline synchronously (1+2=3)")
}

func TestDebounce_NonZeroWindowCoalescesRapidEdits(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	srv.SetDebounceWindow(25 * time.Millisecond)

	const src = "@type acme.demo.v1.Greeting\n\nname = \"a\"\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, src)))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/g.pxf", 2, src)))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/g.pxf", 3, src)))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/g.pxf", 4, src)))

	// At this point: 1 analyze from didOpen (synchronous). The
	// three didChange events reset the timer each time; none
	// have fired yet (we're well under 25 ms).
	assert.Equal(t, int64(1), srv.AnalyzeCount(),
		"three rapid didChange events coalesce — no pipeline runs yet")

	// Wait for the timer to fire.
	require.True(t, waitForAnalyzeCount(t, srv, 2, 500*time.Millisecond),
		"timer eventually fires; analyzeCount reaches 2")
	// One more analyze (from the timer), not three. Total = 2.
	assert.Equal(t, int64(2), srv.AnalyzeCount(),
		"rapid edits produce exactly one deferred analyze")
}

func TestDebounce_PipelineRunsWithFinalSource(t *testing.T) {
	// After coalescing, the analyze sees the source from the LAST
	// applied edit — not from any intermediate state. Verifies
	// the timer reads doc state at fire time, not at scheduling.
	srv, fc, _ := newServerWithLookup(t)
	srv.SetDebounceWindow(25 * time.Millisecond)

	const initial = "@type acme.demo.v1.Greeting\n\nname = \"world\"\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, initial)))

	// Two edits in quick succession. Each replaces "world" / "Mars"
	// with the next target. The timer fires once with the FINAL
	// source visible to analyze.
	r := protocol.Range{
		Start: protocol.Position{Line: 2, Character: 8},
		End:   protocol.Position{Line: 2, Character: 13},
	}
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeIncrementalCall(t, "file:///work/g.pxf", 2, r, "Mars")))
	r2 := protocol.Range{
		Start: protocol.Position{Line: 2, Character: 8},
		End:   protocol.Position{Line: 2, Character: 12},
	}
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeIncrementalCall(t, "file:///work/g.pxf", 3, r2, "Earth")))

	require.True(t, waitForAnalyzeCount(t, srv, 2, 500*time.Millisecond))

	doc := srv.Documents().Get("file:///work/g.pxf")
	require.NotNil(t, doc)
	expected := "@type acme.demo.v1.Greeting\n\nname = \"Earth\"\n"
	assert.Equal(t, expected, string(doc.Source),
		"deferred analyze sees the final post-edit source")

	// PublishDiagnostics fired exactly once for the post-edit
	// state (plus the initial didOpen publish).
	notes := fc.calls()
	assert.GreaterOrEqual(t, len(notes), 2,
		"at least two publishDiagnostics calls: didOpen + final analyze")
	// Latest publish carries the FINAL version.
	last := notes[len(notes)-1]
	assert.Equal(t, protocol.NewOptional(int32(3)), last.params.Version,
		"latest publish tags the final post-edit version")
}

func TestDebounce_DidCloseCancelsPendingTimer(t *testing.T) {
	// If a didChange schedules analyze and then didClose arrives
	// before the timer fires, the timer must not fire on the
	// closed doc. We assert by counting analyzes after a wait
	// longer than the debounce window — must NOT advance past
	// the count from didOpen alone.
	srv, _, _ := newServerWithLookup(t)
	srv.SetDebounceWindow(25 * time.Millisecond)

	const src = "@type acme.demo.v1.Greeting\n\nname = \"a\"\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, src)))
	require.Equal(t, int64(1), srv.AnalyzeCount(),
		"didOpen runs the pipeline synchronously (count=1)")

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/g.pxf", 2, src)))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didCloseCall(t, "file:///work/g.pxf")))

	// analyzeCount must STAY at 1 — the close stopped the timer.
	// assert.Never polls for the full window; a faster CI now
	// finishes the test faster (no fixed sleep) and a slower CI
	// still has time to observe a regression where the deferred
	// Analyze fires.
	assert.Never(t, func() bool {
		return srv.AnalyzeCount() > 1
	}, 60*time.Millisecond, 5*time.Millisecond,
		"didClose cancels the pending timer; deferred analyze never runs")
}

func TestDebounce_DidOpenAlwaysSynchronous(t *testing.T) {
	// didOpen runs synchronously even with a debounce window set
	// — users expect immediate diagnostics on file open.
	srv, _, _ := newServerWithLookup(t)
	srv.SetDebounceWindow(500 * time.Millisecond)

	const src = "@type acme.demo.v1.Greeting\n\nname = \"a\"\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, src)))

	// No wait — count must be 1 immediately.
	assert.Equal(t, int64(1), srv.AnalyzeCount(),
		"didOpen runs the pipeline without waiting on the debounce window")
}
