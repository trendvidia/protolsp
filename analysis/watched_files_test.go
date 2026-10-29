// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/chain"
)

// chameleonTemplateChainSpec mirrors pxfed's chameleon template: a
// single "default" fs source and layer entries that omit `source =`.
// The shape the stale-diagnostics repro (goed's
// TestChainRequiredDiagnostics) uses.
const chameleonTemplateChainSpec = `@type chameleon.v1.ChainSpec
version = "1.0.0"
default_environment = "dev"
environments = [ { name = "prod" } { name = "dev" extends = "prod" } ]
sources = {
  "default": { kind = "fs" options = { "root": "./layers" } }
}
layers = [
  { id = "base" path = "base.pxf"       for_each = ONCE }
  { id = "env"  path = "env/${env}.pxf" for_each = PER_ENV }
]
`

// newWorkspaceWithChain builds a Workspace rooted at a temp dir with
// the given chain spec loaded as the ROOT chain (installed directly
// via SwapChain — the classifier only consults SnapshotChain, so the
// .protolsp.pxf indirection isn't needed here).
func newWorkspaceWithChain(t *testing.T, spec string) (*Workspace, string) {
	t.Helper()
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o600))
	w := New(nil)
	w.ws.SetWorkspaceRoot(uri.File(dir))
	ac, err := chain.Load(specPath)
	require.NoError(t, err)
	w.SwapChain(ac)
	return w, dir
}

func changeEvent(path string) []*protocol.FileEvent {
	return []*protocol.FileEvent{
		{URI: uri.File(path), Type: protocol.FileChangeTypeChanged},
	}
}

// TestClassifyWatchedEvents_LayerFileUnderFsSourceRoot is the
// regression test for stale chain diagnostics: an on-disk change to a chain layer file must flag
// the batch for an open-doc reanalyze, because chain-aware required-
// field diagnostics on open leaves are computed from the union of
// fields set across resolve-order siblings/ancestors read from disk.
func TestClassifyWatchedEvents_LayerFileUnderFsSourceRoot(t *testing.T) {
	w, dir := newWorkspaceWithChain(t, chameleonTemplateChainSpec)

	cls := w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "layers", "env", "dev.pxf")))
	assert.True(t, cls.reanalyzeChainDocs,
		"layer file under the declared fs source root must trigger the chain-doc reanalyze")
	assert.False(t, cls.reloadChain, "layer edits don't require a chain reload")
	assert.False(t, cls.reloadConfig)

	cls = w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "layers", "base.pxf")))
	assert.True(t, cls.reanalyzeChainDocs,
		"ancestor/sibling layers count too — the union is what goes stale")
}

// TestClassifyWatchedEvents_LayerFileByExactResolvedPath covers
// chains with NO sources block: layer paths resolve bare against the
// workspace dir, so only the exact ResolvedPath match can fire.
func TestClassifyWatchedEvents_LayerFileByExactResolvedPath(t *testing.T) {
	spec := `@type chameleon.v1.ChainSpec
version = "1.0.0"
default_environment = "dev"
environments = [ { name = "dev" } ]
layers = [
  { id = "base" path = "base.pxf"      for_each = ONCE }
  { id = "env"  path = "env/${env}.pxf" for_each = PER_ENV }
]
`
	w, dir := newWorkspaceWithChain(t, spec)

	cls := w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "base.pxf")))
	assert.True(t, cls.reanalyzeChainDocs,
		"exact ResolvedPath match must fire without any sources block")

	cls = w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "env", "dev.pxf")))
	assert.True(t, cls.reanalyzeChainDocs,
		"expanded PER_ENV instance paths match too")

	cls = w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "unrelated.pxf")))
	assert.False(t, cls.reanalyzeChainDocs,
		"a .pxf that is neither an expanded layer nor under a source root stays dropped")
}

func TestClassifyWatchedEvents_UnrelatedEventsStayDropped(t *testing.T) {
	w, dir := newWorkspaceWithChain(t, chameleonTemplateChainSpec)

	cls := w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "notes.pxf")))
	assert.False(t, cls.reanalyzeChainDocs,
		".pxf outside every layer path and source root is not a layer file")

	cls = w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "layers", "README.md")))
	assert.False(t, cls.reanalyzeChainDocs,
		"non-.pxf files under a source root don't feed the field union")

	cls = w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "..", "elsewhere", "dev.pxf")))
	assert.False(t, cls.reanalyzeChainDocs,
		"files outside the workspace dir never match")
}

func TestClassifyWatchedEvents_NoChainPaysNothing(t *testing.T) {
	dir := t.TempDir()
	w := New(nil)
	w.ws.SetWorkspaceRoot(uri.File(dir))

	cls := w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "layers", "env", "dev.pxf")))
	assert.False(t, cls.reanalyzeChainDocs,
		"no loaded chain anywhere → the layer-file check never runs")
}

// TestClassifyWatchedEvents_ChainSpecStillReloads pins the ordering:
// an event for the chain spec itself keeps taking the reload path,
// not the (cheaper) layer-file reanalyze path.
func TestClassifyWatchedEvents_ChainSpecStillReloads(t *testing.T) {
	w, dir := newWorkspaceWithChain(t, chameleonTemplateChainSpec)

	cls := w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "chain.pxf")))
	assert.True(t, cls.reloadChain)
	assert.False(t, cls.reanalyzeChainDocs,
		"the spec is not a layer file; reloadChain already fans out to reanalyze")
}

// TestClassifyWatchedEvents_SubWorkspaceLayerFile routes a layer
// event through a SUB-workspace's chain: layer paths anchor at the
// sub-workspace dir, not the LSP root.
func TestClassifyWatchedEvents_SubWorkspaceLayerFile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "svc")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sub, ".protolsp.pxf"),
		[]byte("@type protolsp.v1.WorkspaceConfig\nchameleon {\n  chain_spec = \"chain.pxf\"\n}\n"),
		0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(sub, "chain.pxf"),
		[]byte(chameleonTemplateChainSpec), 0o600))

	w := New(nil)
	w.ws.SetWorkspaceRoot(uri.File(dir))
	sw := w.EnsureSubWorkspace(sub)
	require.NotNil(t, sw.Chain, "sub-workspace chain must load for this test to mean anything")

	cls := w.classifyWatchedEvents(changeEvent(filepath.Join(sub, "layers", "env", "dev.pxf")))
	assert.True(t, cls.reanalyzeChainDocs,
		"layer files of sub-workspace chains anchor at the sub-workspace dir")

	cls = w.classifyWatchedEvents(changeEvent(filepath.Join(dir, "layers", "env", "dev.pxf")))
	assert.False(t, cls.reanalyzeChainDocs,
		"the same relative path at the ROOT is not a layer of the sub-workspace's chain")
}

// TestProcessWatchedFiles_LayerEventReanalyzesOpenDocs asserts the
// fan-out end to end: a layer-file event must re-run the analyze
// pipeline for open documents (the piece whose absence left leaf
// diagnostics stale).
func TestProcessWatchedFiles_LayerEventReanalyzesOpenDocs(t *testing.T) {
	w, dir := newWorkspaceWithChain(t, chameleonTemplateChainSpec)
	w.SetWatchedFilesWindow(0)

	leafPath := filepath.Join(dir, "layers", "env", "dev.pxf")
	require.NoError(t, os.MkdirAll(filepath.Dir(leafPath), 0o755))
	leafSource := []byte("@type example.v1.Greeting\nname = \"alice\"\n")
	require.NoError(t, os.WriteFile(leafPath, leafSource, 0o600))
	doc := &Document{URI: uri.File(leafPath), Source: leafSource}
	w.docs.Open(doc)

	basePath := filepath.Join(dir, "layers", "base.pxf")
	require.NoError(t, os.WriteFile(basePath, []byte("greeting = \"hi\"\n"), 0o600))

	before := w.AnalyzeCount()
	w.ScheduleWatchedFilesProcess(context.Background(), changeEvent(basePath))
	assert.Greater(t, w.AnalyzeCount(), before,
		"an on-disk sibling-layer edit must re-analyze the open leaf")

	// Control: an unrelated event must NOT re-analyze.
	before = w.AnalyzeCount()
	w.ScheduleWatchedFilesProcess(context.Background(), changeEvent(filepath.Join(dir, "unrelated.txt")))
	assert.Equal(t, before, w.AnalyzeCount(),
		"unrelated events keep being dropped without fan-out")
}
