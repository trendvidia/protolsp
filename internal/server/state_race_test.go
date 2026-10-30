// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/chain"
	"github.com/trendvidia/protolsp/workspaceconfig"
)

// TestStateMu_ConcurrentReadsDuringSwapsAreSafe is the race-
// detector test for Server.stateMu: readers (snapshot*) and
// writers (swap*) hammer the workspace-and-config-tier fields
// in parallel for a short window. With -race, an unguarded
// reads-vs-writes pattern would flag immediately.
func TestStateMu_ConcurrentReadsDuringSwapsAreSafe(t *testing.T) {
	srv := New(discardLogger())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers exercising each snapshot helper + the composite
	// readers (effectiveRegistryConfig, ChainStatusSnapshot).
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = srv.SnapshotFileConfig()
					_ = srv.SnapshotChain()
					_ = srv.SnapshotEditorConfig()
					_ = srv.SnapshotSeededFromInitOptions()
					_ = srv.EffectiveRegistryConfig()
					_ = srv.ChainStatusSnapshot()
				}
			}
		}()
	}

	// Writers swapping each field. Loop bounded so the test
	// always terminates; the readers stop via the channel.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			select {
			case <-stop:
				return
			default:
				srv.SwapFileConfig(&workspaceconfig.Config{HasContent: true})
				srv.SwapChain(&chain.ActiveChain{Path: "/tmp/chain.pxf"})
				cfg := srv.SnapshotEditorConfig()
				cfg.Registry.Address = "addr-x"
				srv.SwapEditorConfig(cfg)
				srv.SetSeededFromInitOptions(i%2 == 0)
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestSubWorkspace_LazyLoadRaceWithConcurrentReaders is the
// race-detector test for C6: ensureSubWorkspace used to publish
// a placeholder WorkspaceState into the SubWorkspaces map and
// THEN run loadSubWorkspaceState on the published pointer,
// leaving a window where a concurrent subWorkspaceForDoc reader
// observed a half-loaded struct (FileConfig set, Chain not yet,
// etc.). After the fix, the load runs while WorkspacesMu is
// still held — the map entry never goes through a half-loaded
// state.
func TestSubWorkspace_LazyLoadRaceWithConcurrentReaders(t *testing.T) {
	root, svcA, _ := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	// Build a *Document whose WorkspaceDir is svcA's path. We
	// don't open it through the LSP dispatcher because that
	// triggers the initial EnsureWorkspaceForDoc itself — we
	// want concurrent ensureSubWorkspace + lookups against a
	// FRESH server.
	doc := &Document{
		URI: uri.File(filepath.Join(svcA, "env", "dev.pxf")),
	}
	doc.SetWorkspaceDir(svcA)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers fetching the sub-workspace pointer and reading its
	// fields. Before C6's fix, a half-loaded FileConfig (e.g.
	// non-nil but with Chain still zero) would race against
	// loadSubWorkspaceState's writes.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if sw := srv.SubWorkspaceForDoc(doc); sw != nil {
						_ = sw.FileConfig
						_ = sw.Chain
					}
				}
			}
		}()
	}

	// Loader — ensureSubWorkspace is idempotent after the first
	// successful load; the first call drives loadSubWorkspaceState,
	// subsequent calls hit the map and short-circuit. Either
	// shape must be race-clean.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			select {
			case <-stop:
				return
			default:
				_ = srv.EnsureSubWorkspace(svcA)
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestSubWorkspace_Close_RaceWithConcurrentReads is the race-
// detector test for the cascade teardown:
// while Close walks the SubWorkspaces map and drains each
// lookup, concurrent lookupForDoc calls on those workspaces
// must not race.
func TestSubWorkspace_Close_RaceWithConcurrentReads(t *testing.T) {
	root, svcA, svcB := writeMonorepoWithDifferentRegistries(t)
	srv, _ := initializeWithWorkspace(t, root, nil)

	devURI := uri.File(filepath.Join(svcA, "env", "dev.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, "@type c4test.v1.AppConfig\n")))
	prodURI := uri.File(filepath.Join(svcB, "env", "prod.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, "@type c4test.v1.AppConfig\n")))

	docA := srv.Documents().Get(devURI)
	docB := srv.Documents().Get(prodURI)
	require.NotNil(t, docA)
	require.NotNil(t, docB)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers continuously route lookups through the sub-workspace
	// path. Close() racing with these must not produce a race
	// report.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = srv.LookupForDoc(docA)
					_ = srv.LookupForDoc(docB)
					_ = srv.ChainForDoc(docA)
				}
			}
		}()
	}

	// Give readers a moment to start hammering.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, srv.Close())
	close(stop)
	wg.Wait()
}
