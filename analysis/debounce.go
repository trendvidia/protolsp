// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// I3 of the incremental-sync design: analyze debouncing. Rapid
// didChange events coalesce into one analyze + publish + notify
// pipeline run, instead of one per keystroke. The debounce
// window is Server.debounceWindow — 0 (default) runs the
// pipeline synchronously inside the dispatcher (today's
// behavior; existing tests pass unchanged). Non-zero schedules
// the pipeline on a per-document time.Timer that resets on each
// subsequent edit.
//
// didOpen always runs synchronously regardless of the window —
// users expect immediate diagnostics on file open.
//
// Stale-state policy during the debounce window: cursor-driven
// features (hover, completion, code actions) read the current
// (pre-fire) analysis state. Hovers within the window may see
// data ~debounceWindow older than the source. Accepted explicitly
// per the design's §"Position invalidation between debounce
// window and analyze".

package analysis

import (
	"context"
	"time"

	"go.lsp.dev/uri"
)

// runAnalyzePipeline runs the full analyze → publish → notify
// chain against the document currently stored at doc.URI. The
// passed-in previousSecrets diff baseline is captured at
// scheduling time; the function reads the up-to-date Document
// from the store before each step in case further edits arrived
// between scheduling and fire.
//
// Bumps Server.analyzeCount on completion so tests can wait via
// assert.Eventually without sleeping.
//
// Called both synchronously (debounceWindow == 0) and from the
// deferred timer (debounceWindow > 0).
func (s *Workspace) runAnalyzePipeline(ctx context.Context, docURI uri.URI, previousSecrets []SecretRange, previousDatasets []DatasetRegion) {
	doc := s.docs.Get(docURI)
	if doc == nil {
		// Closed in the interim. Nothing to do.
		return
	}
	s.Analyze(doc)
	s.PublishDiagnostics(ctx, doc)
	// Snapshot post-analyze state under RLock so the notify
	// helpers compare against a coherent point-in-time view.
	version, currentSecrets, currentDatasets := doc.SnapshotForPipeline()
	s.notifySecretRangesChanged(ctx, doc.URI, version, previousSecrets, currentSecrets)
	s.notifyDatasetRegionsChanged(ctx, doc.URI, version, previousDatasets, currentDatasets)
	s.analyzeCount.Add(1)
}

// ScheduleAnalyzePipeline either runs the pipeline now or schedules
// it after Server.debounceWindow. Resetting the timer when one is
// already pending is what produces the coalescing behavior — N
// rapid edits within the window produce one analyze, not N.
//
// The deferred goroutine uses context.Background — the request's
// own context is gone by the time the timer fires.
func (s *Workspace) ScheduleAnalyzePipeline(ctx context.Context, doc *Document, previousSecrets []SecretRange, previousDatasets []DatasetRegion) {
	if s.debounceWindow <= 0 {
		// Synchronous mode — today's behavior preserved. Wrapper
		// routes config / chain docs into the live-reload path.
		s.RunAnalyzeAndMaybeLiveReload(ctx, doc.URI, previousSecrets, previousDatasets)
		return
	}
	doc.analyzeMu.Lock()
	defer doc.analyzeMu.Unlock()
	if doc.analyzeTimer != nil {
		doc.analyzeTimer.Stop()
	}
	docURI := doc.URI
	doc.analyzeTimer = time.AfterFunc(s.debounceWindow, func() {
		// Background context — the request's context is dead by
		// the time the timer fires. Pipeline calls that use it
		// (publish, notify) just dial s.conn directly.
		s.RunAnalyzeAndMaybeLiveReload(context.Background(), docURI, previousSecrets, previousDatasets)
	})
}

// CancelPendingAnalyze stops any pending timer on the document.
// Called from didClose so a fired timer doesn't analyze a
// closed document.
func (s *Workspace) CancelPendingAnalyze(doc *Document) {
	if doc == nil {
		return
	}
	doc.analyzeMu.Lock()
	defer doc.analyzeMu.Unlock()
	if doc.analyzeTimer != nil {
		doc.analyzeTimer.Stop()
		doc.analyzeTimer = nil
	}
}
