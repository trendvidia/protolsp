// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// TestThreadingModel_ConcurrentEmbedderContract hammers the contract
// the package comment documents: feature queries are safe
// concurrently with the analyze pipeline and with each other, and
// the mutating entry points are individually thread-safe. Both the
// unlocked doc.Parsed classification reads and the lazy
// workspaceDir populate were violations of exactly that promise,
// and both were found by an embedder's race suite (goed), not ours.
// TestClassificationDoesNotRaceWatchedFilesReanalyze pins those two
// field pairs specifically; this test exercises the documented model
// broadly so the next engine change can't reintroduce the class
// without CI's -race run noticing.
//
// Shape, mirroring goed's in-process embedding over a proto_files
// workspace: goroutines mix the didOpen flow (Open +
// EnsureWorkspaceForDoc + DetectLayerInfo +
// RunAnalyzeAndMaybeLiveReload), full-text updates +
// ScheduleAnalyzePipeline, and watched-files batches, while readers
// loop the feature queries. Both debounce windows are nonzero so the
// timer goroutines — pipeline goroutines per the Publisher contract —
// participate instead of being flattened into synchronous calls.
// There are no result assertions: the assertion is -race staying
// quiet.
func TestThreadingModel_ConcurrentEmbedderContract(t *testing.T) {
	dir := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	configPath := filepath.Join(dir, ".protolsp.pxf")
	write(configPath, "@type protolsp.v1.WorkspaceConfig\n\nproto_files {\n  paths = [\"proto/\"]\n}\n")
	protoPath := filepath.Join(dir, "proto", "myapp", "v1", "greeting.proto")
	write(protoPath, "syntax = \"proto3\";\npackage myapp.v1;\n\nmessage Greeting {\n  string name = 1;\n  string locale = 2;\n}\n")

	openPath := filepath.Join(dir, "opened.pxf")
	openSrc := []byte("@type myapp.v1.Greeting\nname = \"alice\"\n")
	write(openPath, string(openSrc))
	editPath := filepath.Join(dir, "edited.pxf")
	editSrcs := [][]byte{
		[]byte("@type myapp.v1.Greeting\nname = \"bob\"\n"),
		[]byte("@type myapp.v1.Greeting\nname = \"bob\"\nlocale = \"fr\"\n"),
	}
	write(editPath, string(editSrcs[0]))

	w := New(nil)
	t.Cleanup(w.CancelBackground)
	w.ws.SetWorkspaceRoot(uri.File(dir))
	w.SetBufLint(false)
	w.SetDebounceWindow(time.Millisecond)
	w.SetWatchedFilesWindow(time.Millisecond)

	// Seed the proto_files lookup the way the shell's initialize
	// does, so the feature queries resolve myapp.v1.Greeting from
	// disk rather than short-circuiting on a nil lookup.
	w.LoadWorkspaceFileConfig()
	w.RebuildRootProtoFilesLookup()

	openURI := uri.File(openPath)
	editURI := uri.File(editPath)
	w.docs.Open(&Document{URI: editURI, Source: editSrcs[0]})

	// A config event and a proto event per batch: the drain path
	// re-runs the loaders (LoadWorkspaceFileConfig +
	// RebuildRootProtoFilesLookup + chain reload), which is the
	// state the queries and analyze passes snapshot.
	events := []*protocol.FileEvent{
		{URI: uri.File(configPath), Type: protocol.FileChangeTypeChanged},
		{URI: uri.File(protoPath), Type: protocol.FileChangeTypeChanged},
	}

	ctx := context.Background()
	stop := make(chan struct{})
	var mutators, readers sync.WaitGroup
	const iterations = 50

	// didOpen flow. A fresh Document each round makes readers race
	// the store swap and the lazy workspaceDir populate.
	for range 2 {
		mutators.Add(1)
		go func() {
			defer mutators.Done()
			for range iterations {
				doc := &Document{URI: openURI, Source: openSrc}
				w.docs.Open(doc)
				w.EnsureWorkspaceForDoc(doc)
				w.DetectLayerInfo(doc)
				w.RunAnalyzeAndMaybeLiveReload(ctx, openURI, nil, nil)
			}
		}()
	}

	// Full-text updates + debounced analyze. Sleeping past the
	// window lets armed per-doc timers fire mid-run instead of
	// being rearmed into one trailing drain.
	var version atomic.Int32
	for range 2 {
		mutators.Add(1)
		go func() {
			defer mutators.Done()
			for i := range iterations {
				w.docs.Update(editURI, version.Add(1), editSrcs[i%len(editSrcs)])
				if doc := w.docs.Get(editURI); doc != nil {
					w.ScheduleAnalyzePipeline(ctx, doc, nil, nil)
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}

	// Watched-files batches, same pacing rationale as above.
	mutators.Add(1)
	go func() {
		defer mutators.Done()
		for range iterations / 2 {
			w.ScheduleWatchedFilesProcess(ctx, events)
			time.Sleep(2 * time.Millisecond)
		}
	}()

	// Feature-query readers, running until every mutator is done.
	// All four are called bare — the documented contract is
	// that feature queries lock internally.
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, u := range []uri.URI{openURI, editURI} {
					_ = w.HoverFor(u, protocol.Position{Line: 0, Character: 10})
					_ = w.CompletionFor(u, protocol.Position{Line: 1, Character: 0})
					_ = w.DefinitionFor(u, protocol.Position{Line: 0, Character: 10})
					if doc := w.docs.Get(u); doc != nil {
						_ = w.DiagnosticsFor(doc)
					}
				}
			}
		}()
	}

	mutators.Wait()
	close(stop)
	readers.Wait()

	// Drain the trailing watched-files window before TempDir
	// cleanup — its drain re-reads config and protos from disk.
	deadline := time.Now().Add(5 * time.Second)
	for {
		armed, pending := w.WatchedFilesDebounce()
		if !armed && pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("watched-files debounce never drained: armed=%v pending=%d", armed, pending)
		}
		time.Sleep(time.Millisecond)
	}
}
