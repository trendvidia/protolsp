// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"os"
	"path/filepath"
	"testing"

	"go.lsp.dev/uri"
)

// TestEnvLookupIsPerWorkspace is the regression test for #180: New
// used to install the Workspace's env resolver into workspaceconfig
// via the process-global SetEnvLookup, so the LAST-constructed
// Workspace's bwSession override backed `${env.BW_SESSION}` for
// every Workspace in the process. Since #180 the resolver rides
// along each ParseWithEnv call instead.
//
// The failure shape this pins down: construct A, construct B (which
// pre-#180 would steal the global), then parse A's config — the
// `${env.BW_SESSION}` reference must resolve through A's override,
// not B's.
func TestEnvLookupIsPerWorkspace(t *testing.T) {
	writeConfig := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		cfg := `@type protolsp.v1.WorkspaceConfig

registry {
  address = "registry.example:443"
  token = "${env.BW_SESSION}"
}
`
		if err := os.WriteFile(filepath.Join(dir, ".protolsp.pxf"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	newWorkspaceAt := func(dir, bwSession string) *Workspace {
		w := New(nil)
		w.ws.SetWorkspaceRoot(uri.File(dir))
		// Set the override directly rather than via SetBwSession —
		// this test targets resolver routing, not the reload cascade
		// SetBwSession also fires.
		w.bwSessionMu.Lock()
		w.bwSession = bwSession
		w.bwSessionMu.Unlock()
		return w
	}

	dirA, dirB := writeConfig(t), writeConfig(t)
	a := newWorkspaceAt(dirA, "token-a")
	// B is constructed AFTER A — the pre-#180 global would now be
	// B's resolver, and A's parse below would resolve through it.
	b := newWorkspaceAt(dirB, "token-b")

	a.LoadWorkspaceFileConfig()
	b.LoadWorkspaceFileConfig()

	fcA := a.ws.SnapshotFileConfig()
	if fcA == nil {
		t.Fatal("workspace A: .protolsp.pxf did not load")
	}
	if got := fcA.Registry.Token; got != "token-a" {
		t.Errorf("workspace A resolved ${env.BW_SESSION} to %q; want %q (per-instance resolver)", got, "token-a")
	}
	fcB := b.ws.SnapshotFileConfig()
	if fcB == nil {
		t.Fatal("workspace B: .protolsp.pxf did not load")
	}
	if got := fcB.Registry.Token; got != "token-b" {
		t.Errorf("workspace B resolved ${env.BW_SESSION} to %q; want %q (per-instance resolver)", got, "token-b")
	}
}
