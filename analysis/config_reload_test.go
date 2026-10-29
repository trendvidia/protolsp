// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestClassificationDoesNotRaceWatchedFilesReanalyze is the
// regression test for an unlocked read: RunAnalyzeAndMaybeLiveReload's tail
// read doc.Parsed through IsWorkspaceConfigDoc / IsChainSpecDoc
// with no doc lock, racing the watched-files reanalyze pass
// (processWatchedFiles → reanalyzeOpenDocs → runAnalyzePipeline →
// Analyze), which resets Parsed under doc.mu.Lock.
//
// The shape mirrors goed's in-process embedding, where the two
// paths run on independent goroutines: one drives the didOpen /
// scheduled-analyze pipeline for an open doc, the other feeds a
// .protolsp.pxf change event through the watched-files pipeline.
// Run under -race; the earlier code trips the detector within a
// handful of iterations.
func TestClassificationDoesNotRaceWatchedFilesReanalyze(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, ".protolsp.pxf")
	if err := os.WriteFile(configPath, []byte("@type protolsp.v1.WorkspaceConfig\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(dir, "greeting.pxf")
	docSource := []byte("@type example.v1.Greeting\nname = \"alice\"\n")
	if err := os.WriteFile(docPath, docSource, 0o600); err != nil {
		t.Fatal(err)
	}

	w := New(nil)
	w.ws.SetWorkspaceRoot(uri.File(dir))
	w.SetWatchedFilesWindow(0) // process inline on the calling goroutine

	docURI := uri.File(docPath)
	doc := &Document{URI: docURI, Source: docSource}
	w.docs.Open(doc)
	// Deliberately NO EnsureWorkspaceForDoc warm-up: leaving
	// WorkspaceDir undiscovered makes both goroutines race through
	// the lazy-populate path (EnsureWorkspaceForDoc's write vs. the
	// unlocked classification / reanalyze reads), covering the lazy
	// workspaceDir populate alongside the doc.Parsed reads.

	events := []*protocol.FileEvent{
		{URI: uri.File(configPath), Type: protocol.FileChangeTypeChanged},
	}

	ctx := context.Background()
	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range iterations {
			w.RunAnalyzeAndMaybeLiveReload(ctx, docURI, nil, nil)
		}
	}()
	go func() {
		defer wg.Done()
		for range iterations {
			w.ScheduleWatchedFilesProcess(ctx, events)
		}
	}()
	wg.Wait()
}
