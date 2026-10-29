// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestDocument_AnalyzeRaceWithConcurrentHover is the race-
// detector test for doc.mu: analyze (write lock) and hover
// (read lock) on the SAME document run in parallel. With
// -race, unsynchronized doc-field access would flag at the
// first iteration.
func TestDocument_AnalyzeRaceWithConcurrentHover(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	docURI := uri.New("file:///workspace/race.pxf")
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, greetingValid)))

	doc := srv.Documents().Get(docURI)
	require.NotNil(t, doc)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Hover readers — concurrent with the analyzer.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = srv.HoverFor(docURI, protocol.Position{Line: 2, Character: 4})
				}
			}
		}()
	}

	// Analyzer — full pipeline (parse + validate + secret/dataset
	// walks).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			select {
			case <-stop:
				return
			default:
				srv.Analyze(doc)
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestDocuments_UpdateRaceWithConcurrentReads (C4) is the
// race-detector test for Documents.Update racing with
// concurrent reads of doc.Source. Without doc.mu, the
// Source-slice swap races with HoverFor's source-walk paths.
func TestDocuments_UpdateRaceWithConcurrentReads(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	docURI := uri.New("file:///workspace/update.pxf")
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, greetingValid)))

	doc := srv.Documents().Get(docURI)
	require.NotNil(t, doc)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers reading Source through the helper that snapshots
	// it. Also exercising snapshotForPipeline for the
	// secrets/datasets path.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					doc.RLock()
					_ = len(doc.Source)
					doc.RUnlock()
					_, _, _ = doc.SnapshotForPipeline()
				}
			}
		}()
	}

	// Writer rewriting Source through Documents.Update — the
	// hot didChange path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := []byte(greetingValid)
		for i := 0; i < 200; i++ {
			select {
			case <-stop:
				return
			default:
				srv.Documents().Update(docURI, int32(2+i), buf)
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
