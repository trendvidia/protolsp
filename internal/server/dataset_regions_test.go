// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/positions"
)

func TestDetectDatasetRegions_SingleWellFormed(t *testing.T) {
	src := []byte(`@dataset acme.demo.v1.Row {
  { col_a = "x"; col_b = 1 }
  { col_a = "y"; col_b = 2 }
}
`)
	regions := DetectDatasetRegions(src, 0, positions.UTF16)
	require.Len(t, regions, 1)
	r := regions[0]
	assert.Equal(t, "acme.demo.v1.Row", r.TypeFQN)
	assert.Equal(t, -1, r.RowCount, "R1: row count stays -1; R2 fills it")
	assert.False(t, r.Resolvable, "R1: resolvable stays false; R2 fills it")
	assert.Equal(t, uint32(0), r.Range.Start.Line)
	assert.Equal(t, uint32(0), r.Range.Start.Character)
	assert.Greater(t, r.ByteSize, 0)
}

func TestDetectDatasetRegions_MultipleInOneDocument(t *testing.T) {
	src := []byte(`@dataset a.A {
  { x = 1 }
}

@dataset b.B {
  { y = 2 }
}
`)
	regions := DetectDatasetRegions(src, 0, positions.UTF16)
	require.Len(t, regions, 2)
	assert.Equal(t, "a.A", regions[0].TypeFQN)
	assert.Equal(t, "b.B", regions[1].TypeFQN)
	// Second region starts on a later line than the first.
	assert.Greater(t, regions[1].Range.Start.Line, regions[0].Range.End.Line,
		"second region is past the first")
}

func TestDetectDatasetRegions_UnclosedExtendsToEOF(t *testing.T) {
	// Mid-edit: user typed `@dataset Foo {` and paused. The
	// closing brace doesn't exist yet. Region's end clamps to
	// end-of-document so the editor can still render a huge-
	// region affordance.
	src := []byte(`@dataset acme.demo.v1.Row {
  { col_a = "x"
`)
	regions := DetectDatasetRegions(src, 0, positions.UTF16)
	require.Len(t, regions, 1)
	r := regions[0]
	assert.Equal(t, "acme.demo.v1.Row", r.TypeFQN)
	assert.Equal(t, r.ByteSize, len(src),
		"unclosed region's byte size reaches EOF")
}

func TestDetectDatasetRegions_HugeFlagFromByteSize(t *testing.T) {
	// Build a region just over a small threshold; the Huge flag
	// flips true.
	body := strings.Repeat("  { x = 1 }\n", 20) // ~200 bytes
	src := []byte("@dataset a.A {\n" + body + "}\n")

	// Threshold 100 bytes → huge.
	regions := DetectDatasetRegions(src, 100, positions.UTF16)
	require.Len(t, regions, 1)
	assert.True(t, regions[0].Huge,
		"region exceeding the byte threshold is marked huge")

	// Threshold 10_000 bytes → not huge.
	regions = DetectDatasetRegions(src, 10_000, positions.UTF16)
	require.Len(t, regions, 1)
	assert.False(t, regions[0].Huge,
		"region under the byte threshold is not huge")
}

func TestDetectDatasetRegions_EmptyDocument(t *testing.T) {
	regions := DetectDatasetRegions([]byte(""), 0, positions.UTF16)
	assert.Empty(t, regions)
}

func TestDetectDatasetRegions_NoDatasetIsEmpty(t *testing.T) {
	src := []byte(`@type acme.demo.v1.Greeting

name = "world"
`)
	regions := DetectDatasetRegions(src, 0, positions.UTF16)
	assert.Empty(t, regions,
		"a non-@dataset document produces no regions")
}

func TestDetectDatasetRegions_LeadingWhitespaceOnDirectiveLine(t *testing.T) {
	// `@dataset` doesn't have to be at column 0 to qualify.
	// Common indentation conventions still produce a region.
	src := []byte(`   @dataset a.A {
  { x = 1 }
}
`)
	regions := DetectDatasetRegions(src, 0, positions.UTF16)
	require.Len(t, regions, 1)
	assert.Equal(t, "a.A", regions[0].TypeFQN)
	assert.Equal(t, uint32(3), regions[0].Range.Start.Character,
		"start column reflects the leading whitespace")
}

// Request dispatch tests.

func datasetRegionsReply(t *testing.T, srv *Server, docURI uri.URI) DatasetRegionsResult {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodDatasetRegions,
		DatasetRegionsParams{TextDocument: protocol.TextDocumentIdentifier{URI: docURI}})
	require.NoError(t, err)
	var got DatasetRegionsResult
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, _ := json.Marshal(result)
		_ = json.Unmarshal(b, &got)
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return got
}

func TestDatasetRegions_RequestReturnsAnalyzedRegions(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	const src = `@dataset acme.demo.v1.Row {
  { col_a = "x" }
}
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/d.pxf", 1, src)))

	got := datasetRegionsReply(t, srv, "file:///work/d.pxf")
	require.Len(t, got.Regions, 1)
	assert.Equal(t, "acme.demo.v1.Row", got.Regions[0].TypeFQN)
}

func TestDatasetRegions_NoDocReturnsEmpty(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	got := datasetRegionsReply(t, srv, "file:///not-open.pxf")
	assert.NotNil(t, got.Regions, "missing doc → empty array, not null")
	assert.Empty(t, got.Regions)
}

func TestDatasetRegions_RegionsSurviveUnparseableDocument(t *testing.T) {
	// Unparseable PXF still produces dataset regions via the
	// line-based scanner — the resilience point of R1.
	srv, _, _ := newServerWithLookup(t)
	const src = `@dataset acme.demo.v1.Row {
  this is not valid pxf
}
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/d.pxf", 1, src)))

	got := datasetRegionsReply(t, srv, "file:///work/d.pxf")
	require.Len(t, got.Regions, 1,
		"line-based scanner detects regions even when parse fails")
	assert.Equal(t, "acme.demo.v1.Row", got.Regions[0].TypeFQN)
}

func TestDatasetRegions_ThresholdConfigurable(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	srv.SetHugeRegionBytes(50) // tiny threshold so a few lines trip it

	const src = `@dataset acme.demo.v1.Row {
  { col_a = "padding to exceed 50 bytes easily" }
  { col_a = "more padding" }
}
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/d.pxf", 1, src)))

	got := datasetRegionsReply(t, srv, "file:///work/d.pxf")
	require.Len(t, got.Regions, 1)
	assert.True(t, got.Regions[0].Huge,
		"Server.hugeRegionBytes threshold controls the Huge flag")
}

func TestDatasetRegions_RegionsClearedOnSourceChange(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/d.pxf", 1,
			`@dataset a.A {
  { x = 1 }
}
`)))

	// First snapshot: one region.
	got := datasetRegionsReply(t, srv, "file:///work/d.pxf")
	require.Len(t, got.Regions, 1)

	// Replace with a non-dataset document.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/d.pxf", 2, `@type Greeting

name = "x"
`)))

	got = datasetRegionsReply(t, srv, "file:///work/d.pxf")
	assert.Empty(t, got.Regions,
		"removing the @dataset directive clears the regions on next analyze")
}

func TestDatasetRegions_NilParamsTolerated(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodDatasetRegions, nil)
	require.NoError(t, err)
	var got DatasetRegionsResult
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, _ := json.Marshal(result)
		_ = json.Unmarshal(b, &got)
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	assert.NotNil(t, got.Regions, "null params → empty result, no error")
}
