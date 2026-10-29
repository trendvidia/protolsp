// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Server-level shims over the WorkspaceManager (T1 PR C) and
// ConfigState (T1 PR B). The state and synchronization live in
// those concerns; Server methods are kept as a stable dispatcher
// surface so handlers don't have to spell out `s.ws.snapshotX`
// / `s.cfg.snapshotY` at every call site.

package analysis

import (
	"github.com/trendvidia/protolsp/chain"
	"github.com/trendvidia/protolsp/workspaceconfig"
)

func (s *Workspace) SnapshotFileConfig() *workspaceconfig.Config {
	if s == nil {
		return nil
	}
	return s.ws.SnapshotFileConfig()
}

func (s *Workspace) SwapFileConfig(cfg *workspaceconfig.Config) *workspaceconfig.Config {
	if s == nil {
		return nil
	}
	return s.ws.SwapFileConfig(cfg)
}

func (s *Workspace) SnapshotChain() *chain.ActiveChain {
	if s == nil {
		return nil
	}
	return s.ws.SnapshotChain()
}

func (s *Workspace) SwapChain(c *chain.ActiveChain) *chain.ActiveChain {
	if s == nil {
		return nil
	}
	return s.ws.SwapChain(c)
}

// SnapshotEditorConfig / SwapEditorConfig / SnapshotSeededFromInitOptions
// / SetSeededFromInitOptions are thin shims over the ConfigState
// concern (T1 PR B). Callers continue to use Server methods so
// the dispatcher surface stays stable; the actual state +
// synchronization live in ConfigState.
func (s *Workspace) SnapshotEditorConfig() EditorConfig {
	if s == nil {
		return EditorConfig{}
	}
	return s.cfg.SnapshotEditorConfig()
}

func (s *Workspace) SwapEditorConfig(cfg EditorConfig) EditorConfig {
	if s == nil {
		return EditorConfig{}
	}
	return s.cfg.SwapEditorConfig(cfg)
}

func (s *Workspace) SnapshotSeededFromInitOptions() bool {
	if s == nil {
		return false
	}
	return s.cfg.SnapshotSeededFromInitOptions()
}

func (s *Workspace) SetSeededFromInitOptions(v bool) {
	if s == nil {
		return
	}
	s.cfg.SetSeededFromInitOptions(v)
}
