// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Rebuild orchestration for the user-registry-backed schema.Lookup.
// Field-level state ownership (lookup, lookupCloser, lookupMu,
// rebuildMu, lastRebuiltCfg, rebuildCancel) lives in
// LookupManager (lookup_manager.go); this file orchestrates the
// dial / install / re-analyze pipeline on top of that.
//
// Two related concerns:
//
//   1. **Snapshot reads + atomic swaps.** Every production read
//      site goes through lookupMgr.Snapshot (RWMutex.RLock).
//      Writes — initial construction at initialize, config-
//      driven rebuilds at edit time, teardown at close — go
//      through lookupMgr.Swap.
//
//   2. **Diff + async rebuild.** The pre-existing
//      RebuildLookupFromEffectiveConfig dialed the registry
//      synchronously on every live-reload trigger, even when the
//      registry config hadn't changed. That blocked the analyze
//      pipeline on potentially-slow network calls. The current
//      design diff-checks first via lookupMgr.BeginRebuild, dials
//      in a goroutine, and coalesces rapid edits by cancelling
//      any in-flight dial whose result would be superseded.

package analysis

import (
	"context"
	"log/slog"

	"github.com/trendvidia/protolsp/schema"
)

// RebuildLookupAsync (re-)builds the schema lookup from the
// current effective config. Returns immediately; the actual
// schema.Open call runs in a background goroutine.
//
// Diff-before-rebuild: when the effective config equals what we
// last built the lookup from, this is a no-op. Most live-reload
// triggers (a user editing the security or chameleon block of
// .protolsp.pxf) take this path — saving a dial per keystroke
// when only non-registry fields changed.
//
// Coalescing: a fresh rebuild request cancels any in-flight
// dial via lookupMgr.BeginRebuild. The cancelled goroutine sees
// ctx.Err() after schema.Open returns and skips the install;
// only the most recent request gets to install its result.
//
// The empty-registry path is handled synchronously (no async
// teardown needed — closing the old closer is fast).
func (s *Workspace) RebuildLookupAsync(_ context.Context) {
	if s == nil {
		return
	}
	cfg := s.EffectiveRegistryConfig()
	rebuildCtx, proceed := s.lookupMgr.BeginRebuild(cfg)
	if !proceed {
		// Diff: identical config + the lookup already reflects
		// it → nothing to do.
		return
	}
	if cfg.Address == "" {
		// Empty registry — tear down any existing lookup
		// synchronously. The in-flight rebuild (if any) was
		// already cancelled inside BeginRebuild.
		if oldCloser := s.lookupMgr.Swap(nil, nil); oldCloser != nil {
			if err := oldCloser.Close(); err != nil {
				s.log.Warn("closing previous schema lookup",
					slog.String("err", err.Error()))
			}
		}
		return
	}
	go s.dialAndInstallLookup(rebuildCtx, cfg)
}

// dialAndInstallLookup is the background goroutine the async
// rebuild kicks off. Dial + install + post-install reanalyze.
//
// W3: when the root workspace has compiled proto_files on hand,
// they're wired in as a WithFallback parent on the registry
// resolver so unresolved names walk into the local descriptor set
// transparently — no separate-lookup routing dance.
//
// Cancellation check happens AFTER the dial returns so the
// schema.Open call gets terminated via ctx (the dial implementation
// honors ctx). If we got cancelled (a newer rebuild started), we
// drop the dial result on the floor and let the newer one win.
func (s *Workspace) dialAndInstallLookup(ctx context.Context, cfg schema.Config) {
	var openOpts []schema.OpenOption
	if files, types := schema.ProtoFilesParents(s.lookupMgr.SnapshotProtoFiles()); files != nil && types != nil {
		openOpts = append(openOpts, schema.WithFallback(files, types))
	}
	lookup, closer, err := schema.Open(ctx, cfg, openOpts...)
	if err != nil {
		// Non-fatal: a dial failure leaves the previous lookup in
		// place. The user keeps editing against the old state
		// until they fix the registry config or it becomes
		// reachable again. Still fire LookupChanged: the
		// snapshot is unchanged, but a status surface waiting on
		// this rebuild needs to know the attempt resolved.
		s.log.Warn("background schema lookup rebuild failed; keeping previous lookup",
			slog.String("address", cfg.Address),
			slog.String("err", err.Error()))
		s.pub.LookupChanged(ctx, "")
		return
	}
	if ctx.Err() != nil {
		// Cancelled mid-dial. Don't install — a newer rebuild is
		// already in flight (or completed).
		if closer != nil {
			_ = closer.Close()
		}
		return
	}
	if oldCloser := s.lookupMgr.Swap(lookup, closer); oldCloser != nil {
		if err := oldCloser.Close(); err != nil {
			s.log.Warn("closing previous schema lookup after async rebuild",
				slog.String("err", err.Error()))
		}
	}
	s.log.Info("schema lookup rebuilt (async)",
		slog.String("address", cfg.Address),
		slog.String("namespace", cfg.Namespace))
	// Re-analyze open docs so they reflect the new lookup. Bypasses
	// the live-reload wrapper to avoid re-triggering a rebuild
	// from within a rebuild path.
	s.reanalyzeOpenDocs(ctx)
}
