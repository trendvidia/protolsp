// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

func TestRegionAtPosition_InsideAReturnsRegion(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	doc := &Document{
		DatasetRegions: []DatasetRegion{
			{
				Range: protocol.Range{
					Start: protocol.Position{Line: 2, Character: 0},
					End:   protocol.Position{Line: 6, Character: 0},
				},
				TypeFQN: "a.A",
			},
		},
	}
	got := srv.RegionAtPosition(doc, protocol.Position{Line: 3, Character: 5})
	require.NotNil(t, got)
	assert.Equal(t, "a.A", got.TypeFQN)
}

func TestRegionAtPosition_OutsideReturnsNil(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	doc := &Document{
		DatasetRegions: []DatasetRegion{
			{
				Range: protocol.Range{
					Start: protocol.Position{Line: 2, Character: 0},
					End:   protocol.Position{Line: 6, Character: 0},
				},
			},
		},
	}
	// Before
	assert.Nil(t, srv.RegionAtPosition(doc, protocol.Position{Line: 1, Character: 0}))
	// At End (exclusive)
	assert.Nil(t, srv.RegionAtPosition(doc, protocol.Position{Line: 6, Character: 0}))
	// After
	assert.Nil(t, srv.RegionAtPosition(doc, protocol.Position{Line: 9, Character: 0}))
}

func TestRegionAtPosition_MultipleRegionsPicksMatch(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	doc := &Document{
		DatasetRegions: []DatasetRegion{
			{Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 0},
				End:   protocol.Position{Line: 3, Character: 0},
			}, TypeFQN: "a.A"},
			{Range: protocol.Range{
				Start: protocol.Position{Line: 5, Character: 0},
				End:   protocol.Position{Line: 9, Character: 0},
			}, TypeFQN: "b.B"},
		},
	}
	got := srv.RegionAtPosition(doc, protocol.Position{Line: 7, Character: 0})
	require.NotNil(t, got)
	assert.Equal(t, "b.B", got.TypeFQN,
		"position falls inside the second region")
}

func TestHover_DatasetDirectiveLineReturnsTypeInfo(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))

	hover := srv.HoverFor("file:///work/t.pxf", protocol.Position{Line: 0, Character: 10})
	require.NotNil(t, hover, "hover on the @dataset directive line returns content")

	md := hover.Contents.Value
	assert.Contains(t, md, "@dataset")
	assert.Contains(t, md, "acme.demo.v1.Trade")
	assert.Contains(t, md, "3 rows", "AST-enriched row count surfaces in hover")
	assert.Contains(t, md, "acme/demo/v1/trade.proto",
		"defining-file line is rendered")
}

func TestHover_DatasetWithUnknownTypeSurfacesIt(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup() // type intentionally not added
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetUnknownSrc)))

	hover := srv.HoverFor("file:///work/t.pxf", protocol.Position{Line: 0, Character: 10})
	require.NotNil(t, hover)
	md := hover.Contents.Value
	assert.Contains(t, md, "acme.totally.Unknown")
	assert.Contains(t, md, "not known to the bound schema resolver",
		"unknown type → hover names the misconfiguration plainly")
}

func TestHover_OnRowLineFallsThrough(t *testing.T) {
	// Cursor inside the body (a row line) — R3 doesn't render
	// cell-level hover yet; we just don't surface the directive
	// info. Today this returns nil because @dataset documents
	// have no @type to anchor the entry-walk path either.
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))

	// Line 1 is a row tuple `("T-1", 10)`.
	hover := srv.HoverFor("file:///work/t.pxf", protocol.Position{Line: 1, Character: 4})
	assert.Nil(t, hover,
		"R3 returns no hover for row-line cursors; cell-level hover is future scope")
}

func TestHover_StandaloneDocumentStillWorks(t *testing.T) {
	// A regular @type document with no @dataset → hover behavior
	// is unchanged from before R3.
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	hover := srv.HoverFor("file:///work/g.pxf", protocol.Position{Line: 2, Character: 2})
	require.NotNil(t, hover, "field hover on a regular @type doc still works")
	md := hover.Contents.Value
	assert.Contains(t, md, "name")
}

func TestHover_NoLookupBailsCleanly(t *testing.T) {
	// No lookup configured → no hover, even on a region's
	// directive line. (Today's behavior for hover with no
	// schema; pre-R3 short-circuit preserved.)
	srv, _ := newServerWithFakeConn(t)
	// Don't set lookup. didOpen still runs; analyze detects
	// regions via the line-based scan regardless.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))

	hover := srv.HoverFor("file:///work/t.pxf", protocol.Position{Line: 0, Character: 10})
	assert.Nil(t, hover, "no lookup → hover bails (same as for any doc)")
}

func TestHover_DatasetSingleRowSayRow(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@dataset acme.demo.v1.Trade(id, qty)
("T-1", 10)
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	hover := srv.HoverFor("file:///work/t.pxf", protocol.Position{Line: 0, Character: 10})
	require.NotNil(t, hover)
	md := hover.Contents.Value
	assert.Contains(t, md, "1 row",
		"singular 'row' (no plural s) for a one-row dataset")
	assert.NotContains(t, md, "1 rows")
}
