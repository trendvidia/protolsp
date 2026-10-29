// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Lookup-state ownership. T1's first extraction from the Server
// god-object: the registry-backed schema.Lookup + the async
// rebuild bookkeeping (cancel + last-built-cfg diff) live here.
//
// Server keeps a *LookupManager field and delegates Snapshot /
// Swap / BeginRebuild / Close to it. The rebuild ORCHESTRATION
// (RebuildLookupAsync — read effective cfg, decide teardown vs.
// dial, kick off goroutine) stays on Server because the dial
// path needs config-tier reads (effectiveRegistryConfig) and
// post-install document re-analysis (reanalyzeOpenDocs) — both
// cross-cutting concerns that don't belong inside this
// component.

package analysis

import (
	"context"
	"io"
	"sync"

	"github.com/trendvidia/protolsp/schema"
)

// LookupManager owns the user-registry-backed schema.Lookup plus
// the rebuild-coalescing state. Constructed once at server
// construction time; the zero value is NOT usable — use
// newLookupManager.
//
// Concurrency:
//   - lookupMu guards lookup + lookupCloser. Snapshot / Swap take
//     it; readers RLock briefly, writers Lock.
//   - rebuildMu guards lastRebuiltCfg + rebuildCancel. Distinct
//     from lookupMu so the rebuild scheduler doesn't contend with
//     hot-path lookup reads.
//   - Lock-order between the two: never both at the same time.
//     Snapshot/Swap don't touch rebuildMu; BeginRebuild doesn't
//     touch lookupMu (the actual install happens via a separate
//     Swap call from the goroutine the caller spawned).
type LookupManager struct {
	lookup       schema.Lookup
	lookupCloser io.Closer

	// onChanged fires after Swap / swapProtoFiles lands an actual
	// install or teardown (a nil→nil swap is silent), outside
	// lookupMu so the callback can read Snapshot freely. Wired by
	// New to the Publisher's LookupChanged seam; nil in
	// managers that don't publish.
	onChanged func()

	// protoFiles is the W2 proto_files-backed lookup for the root
	// workspace, populated when `.protolsp.pxf` declares a
	// top-level `proto_files { ... }` block. Independent of the
	// registry-backed `lookup`: a workspace can declare proto_files
	// only (no registry) or both (W3 composes them — for W2 alone
	// they're picked between by Snapshot).
	protoFiles       schema.Lookup
	protoFilesCloser io.Closer

	lookupMu sync.RWMutex

	rebuildMu      sync.Mutex
	lastRebuiltCfg schema.Config
	rebuildCancel  context.CancelFunc
}

func newLookupManager(onChanged func()) *LookupManager {
	return &LookupManager{onChanged: onChanged}
}

// Snapshot returns the current lookup (may be nil). Concurrent-
// safe; callers can hold the returned interface without re-
// locking — schema.Lookup implementations are documented as
// concurrent-safe internally.
//
// W2 fork: when no registry lookup is installed but a proto_files
// lookup is, return the proto_files one. W3 will compose the two
// at construction time; today's caller gets the source that's
// actually configured.
func (m *LookupManager) Snapshot() schema.Lookup {
	if m == nil {
		return nil
	}
	m.lookupMu.RLock()
	defer m.lookupMu.RUnlock()
	if m.lookup != nil {
		return m.lookup
	}
	return m.protoFiles
}

// SnapshotProtoFiles returns the W2 proto_files-backed lookup, or
// nil when none is configured. Exposed separately from Snapshot so
// W3's composition path can read it without going through the
// W2 fallback ordering.
func (m *LookupManager) SnapshotProtoFiles() schema.Lookup {
	if m == nil {
		return nil
	}
	m.lookupMu.RLock()
	defer m.lookupMu.RUnlock()
	return m.protoFiles
}

// swapProtoFiles installs newLookup + newCloser as the current
// proto_files-backed lookup and returns the prior closer for the
// caller to close outside the lock. Mirrors Swap but for the W2
// slot, including the onChanged fire — proto_files swaps change
// what Snapshot's W2 fallback serves.
func (m *LookupManager) swapProtoFiles(newLookup schema.Lookup, newCloser io.Closer) (oldCloser io.Closer) {
	if m == nil {
		return nil
	}
	m.lookupMu.Lock()
	oldLookup := m.protoFiles
	oldCloser = m.protoFilesCloser
	m.protoFiles = newLookup
	m.protoFilesCloser = newCloser
	m.lookupMu.Unlock()
	m.notifyChanged(oldLookup, newLookup)
	return oldCloser
}

// Swap installs newLookup + newCloser as the current lookup
// state and returns the prior closer for the caller to close
// outside the lock. Closing the old resolver inside the lock
// would risk a deadlock if the close ever called back into the
// server's handlers.
//
// An actual install or teardown fires onChanged after the mutation
// (still before the caller closes the returned oldCloser — by then
// the old lookup is already unreachable via Snapshot, so a callback
// re-reading state sees only the new one).
func (m *LookupManager) Swap(newLookup schema.Lookup, newCloser io.Closer) (oldCloser io.Closer) {
	if m == nil {
		return nil
	}
	m.lookupMu.Lock()
	oldLookup := m.lookup
	oldCloser = m.lookupCloser
	m.lookup = newLookup
	m.lookupCloser = newCloser
	m.lookupMu.Unlock()
	m.notifyChanged(oldLookup, newLookup)
	return oldCloser
}

// notifyChanged fires onChanged unless the swap was a nil→nil
// no-op (e.g. Close on a never-configured slot, or the empty-
// registry teardown path running before anything was installed).
// Interface identity between old and new is deliberately NOT
// compared — re-installing over an existing lookup is a real
// transition (fresh dial result), and comparing schema.Lookup
// interfaces could panic on uncomparable implementations.
func (m *LookupManager) notifyChanged(oldLookup, newLookup schema.Lookup) {
	if m.onChanged == nil {
		return
	}
	if oldLookup == nil && newLookup == nil {
		return
	}
	m.onChanged()
}

// BeginRebuild atomically decides whether a rebuild is needed
// for cfg and prepares its cancellation context. Returns
// (nil, false) when cfg equals the last-built baseline — the
// caller skips the rebuild entirely. Otherwise updates the
// baseline, cancels any in-flight rebuild's context, allocates
// a fresh cancel for the new attempt, and returns (newCtx, true).
//
// The empty-Address path is the caller's concern: BeginRebuild
// still returns (ctx, true) so the caller can do its
// synchronous-teardown branch (close the old closer; don't
// dial). The cancel side-effect on prior rebuilds is the
// shared part.
func (m *LookupManager) BeginRebuild(cfg schema.Config) (context.Context, bool) {
	if m == nil {
		return nil, false
	}
	m.rebuildMu.Lock()
	defer m.rebuildMu.Unlock()
	if cfg == m.lastRebuiltCfg {
		return nil, false
	}
	m.lastRebuiltCfg = cfg
	if m.rebuildCancel != nil {
		m.rebuildCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.rebuildCancel = cancel
	return ctx, true
}

// RecordBaseline records cfg as the last successfully-installed
// rebuild baseline WITHOUT firing a rebuild. Used after the
// synchronous initial install in configureSchemaLookup so the
// next async rebuild's diff check sees the right baseline and
// skips redundant dials.
func (m *LookupManager) RecordBaseline(cfg schema.Config) {
	if m == nil {
		return
	}
	m.rebuildMu.Lock()
	m.lastRebuiltCfg = cfg
	m.rebuildMu.Unlock()
}

// invalidateBaseline clears the diff-check baseline so the next
// BeginRebuild proceeds with a dial even when the caller's cfg
// matches what was previously tried. Used when an out-of-band
// signal — pxfed/setBwSession is the motivating example —
// suggests the previous dial outcome may have changed despite
// an unchanged registry config (e.g., the registry server
// itself was unreachable / unauthenticated at the first dial
// because BW_SESSION wasn't yet in env, and is reachable now).
// Without this, a failed initial dial stays sticky for the
// session — hasUserLookup() returns false, validate() bails,
// and downstream features (secret-typed field detection,
// schema-driven go-to-def) silently fail forever.
func (m *LookupManager) invalidateBaseline() {
	if m == nil {
		return
	}
	m.rebuildMu.Lock()
	m.lastRebuiltCfg = schema.Config{}
	m.rebuildMu.Unlock()
}

// CancelInFlight cancels the in-flight rebuild's context (if
// any) and clears the stored cancel. Used at shutdown so the
// goroutine doesn't try to install into a torn-down server.
func (m *LookupManager) CancelInFlight() {
	if m == nil {
		return
	}
	m.rebuildMu.Lock()
	defer m.rebuildMu.Unlock()
	if m.rebuildCancel != nil {
		m.rebuildCancel()
		m.rebuildCancel = nil
	}
}

// Close cancels any in-flight rebuild + closes both the registry-
// backed and proto_files-backed lookups. Returns the first non-nil
// close error (registry's takes precedence; proto_files' closer is
// a no-op today so the order rarely matters in practice).
// Server.Close cascades here.
func (m *LookupManager) Close() error {
	if m == nil {
		return nil
	}
	m.CancelInFlight()
	var firstErr error
	if old := m.Swap(nil, nil); old != nil {
		if err := old.Close(); err != nil {
			firstErr = err
		}
	}
	if old := m.swapProtoFiles(nil, nil); old != nil {
		if err := old.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
