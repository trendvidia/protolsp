// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Config-tier ownership. T1's second extraction (PR B) from
// Server's god-object: the Tier 3 (editor settings) + Tier 4
// (overrides) state from the config-protocol model plus the
// client-capability snapshot.
//
// Splits out of the shared Server.stateMu: this concern gets
// its own configMu (RWMutex). PR C will move the remaining
// stateMu callers (fileConfig + chain) into the WorkspaceManager,
// at which point Server.stateMu disappears entirely.
//
// Methods that resolve VALUES across multiple tiers
// (effectiveRegistryConfig, ResolveSection, SnapshotSectionsLocked)
// stay on Server — they need to reach into both this concern
// (EditorConfig, overrides) and the workspace concern
// (fileConfig). Moving them here or into WorkspaceManager would
// force a bidirectional reference; keeping them on Server is
// simpler.

package analysis

import (
	"sync"
)

// ConfigState owns the Tier 3 editor-settings, the Tier 4
// session-override map, and the per-session client-capability
// snapshot. Constructed once at server construction time; the
// zero value is NOT usable — use newConfigState.
//
// Concurrency:
//   - configMu (RWMutex) guards EditorConfig +
//     seededRegistryFromInitOptions. Readers RLock; writers
//     Lock.
//   - overridesMu (Mutex) guards the overrides map. Held across
//     the dispatchSetOverrides snapshot→apply→snapshot window
//     so concurrent readers can't see a half-applied state
//     across multi-section calls (C5 of the audit roadmap).
//   - clientCaps is set once at initialize before any concurrent
//     handler can race with it; no mutex needed.
//
// Lock-order: configMu before overridesMu (overrides resolution
// reads EditorConfig as a fallback); never both held at once
// today, but documenting the order in case a future composite
// op needs both.
type ConfigState struct {
	configMu sync.RWMutex

	// EditorConfig is the merged Tier 3 view of editor-supplied
	// settings. Seeded from InitializeParams.InitializationOptions;
	// replaced / refined by workspace/configuration pulls. Drives
	// schema-lookup rebuilds when registry-related fields change
	// mid-session.
	EditorConfig EditorConfig

	// seededRegistryFromInitOptions records whether the client
	// passed registry config via InitializationOptions. CP5's
	// one-time deprecation nudge at `initialized` fires only when
	// this is true AND the client advertises
	// workspace/configuration capability.
	seededRegistryFromInitOptions bool

	// clientCaps captures the subset of LSP client capabilities
	// the configuration plumbing cares about. Populated at
	// initialize; consulted in initialized + on every change.
	// Effectively immutable after initialize; no synchronization.
	clientCaps ClientCapabilities

	// overrides holds Tier 4 (session) configuration overrides.
	// Keys are section names (configurationSections); values are
	// typed per the section. nil values mean "explicitly cleared"
	// and are removed from the map. In-memory only; cleared on
	// LSP restart by design.
	overrides   map[string]any
	overridesMu sync.Mutex
}

func newConfigState() *ConfigState {
	return &ConfigState{}
}

// SnapshotEditorConfig returns a value-typed copy of the
// current editor-config snapshot.
func (c *ConfigState) SnapshotEditorConfig() EditorConfig {
	if c == nil {
		return EditorConfig{}
	}
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.EditorConfig
}

// SwapEditorConfig replaces EditorConfig atomically and returns
// the prior value. applyEditorConfig uses the returned value to
// decide whether the registry-defining fields changed (and thus
// whether to schedule a rebuild).
func (c *ConfigState) SwapEditorConfig(cfg EditorConfig) EditorConfig {
	if c == nil {
		return EditorConfig{}
	}
	c.configMu.Lock()
	old := c.EditorConfig
	c.EditorConfig = cfg
	c.configMu.Unlock()
	return old
}

// SnapshotSeededFromInitOptions reads the deprecation-nudge
// flag under the RLock.
func (c *ConfigState) SnapshotSeededFromInitOptions() bool {
	if c == nil {
		return false
	}
	c.configMu.RLock()
	defer c.configMu.RUnlock()
	return c.seededRegistryFromInitOptions
}

// SetSeededFromInitOptions writes the flag under the write
// lock. Called once from configureSchemaLookup; goes through the
// lock so the race detector doesn't flag the unguarded initial
// assignment on later reads.
func (c *ConfigState) SetSeededFromInitOptions(v bool) {
	if c == nil {
		return
	}
	c.configMu.Lock()
	c.seededRegistryFromInitOptions = v
	c.configMu.Unlock()
}

// LockOverrides / UnlockOverrides expose the Tier 4 overrides mutex
// so the dispatch/RPC setOverrides paths outside this package can
// hold it across their snapshot → ApplyOverridesLocked → snapshot
// window (C5 of the audit roadmap).
func (c *ConfigState) LockOverrides()   { c.overridesMu.Lock() }
func (c *ConfigState) UnlockOverrides() { c.overridesMu.Unlock() }
