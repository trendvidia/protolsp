// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// buildTradeMessageType constructs a MessageType for the row type:
//
//	package acme.demo.v1;
//	message Trade { string id = 1; int32 qty = 2; }
//
// Used by R2 tests that need a message the resolver recognizes as
// the @dataset's row type.
func buildTradeMessageType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("acme/demo/v1/trade.proto"),
		Package: proto.String("acme.demo.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Trade"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("id"), Number: proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("id")},
					{Name: proto.String("qty"), Number: proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("qty")},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().Get(0))
}

const tradeDatasetSrc = `@dataset acme.demo.v1.Trade(id, qty)
("T-1", 10)
("T-2", 20)
("T-3", 30)
`

const tradeDatasetUnknownSrc = `@dataset acme.totally.Unknown(id, qty)
("T-1", 10)
("T-2", 20)
`

func TestEnrichAST_FillsRowCountAndResolvable(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))

	doc := srv.Documents().Get("file:///work/t.pxf")
	require.NotNil(t, doc)
	require.Len(t, doc.DatasetRegions, 1)
	r := doc.DatasetRegions[0]
	assert.Equal(t, "acme.demo.v1.Trade", r.TypeFQN)
	assert.Equal(t, 3, r.RowCount, "AST pass fills RowCount from len(d.Rows)")
	assert.True(t, r.Resolvable, "type lookup succeeded → Resolvable true")
}

func TestEnrichAST_ResolvableFalseForUnknownType(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t)) // Trade exists; Unknown does not
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetUnknownSrc)))

	doc := srv.Documents().Get("file:///work/t.pxf")
	require.NotNil(t, doc)
	require.Len(t, doc.DatasetRegions, 1)
	r := doc.DatasetRegions[0]
	assert.Equal(t, 2, r.RowCount)
	assert.False(t, r.Resolvable,
		"unknown type → Resolvable stays false; UI flags this")
}

func TestEnrichAST_RowCountTriggersHuge(t *testing.T) {
	// With a low row threshold, even a 3-row dataset trips Huge.
	// Byte size of this small fixture is well under any default.
	srv, _ := newServerWithFakeConn(t)
	srv.SetHugeRegionRows(2)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))

	doc := srv.Documents().Get("file:///work/t.pxf")
	require.NotNil(t, doc)
	require.Len(t, doc.DatasetRegions, 1)
	r := doc.DatasetRegions[0]
	assert.True(t, r.Huge,
		"row count exceeds threshold → Huge regardless of byte size")
}

// TestEnrichAST_ShrinkBelowThresholdClearsHuge pairs with
// TestEnrichAST_RowCountTriggersHuge — the becomes-huge direction
// was covered, the shrink-below direction was not. A region that
// drops from above-threshold to below-threshold must clear the
// Huge flag so the editor stops rendering the huge-region
// affordance (collapsed view, "click to expand" hint, etc.).
func TestEnrichAST_ShrinkBelowThresholdClearsHuge(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	srv.SetHugeRegionRows(2) // 2 rows trips Huge; below that is normal
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	// Open at 3 rows — above threshold → Huge=true.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))

	doc := srv.Documents().Get("file:///work/t.pxf")
	require.NotNil(t, doc)
	require.Len(t, doc.DatasetRegions, 1)
	require.True(t, doc.DatasetRegions[0].Huge,
		"precondition: initial open with 3 rows must trip Huge under the lowered threshold")

	// Shrink to 1 row — below threshold → Huge must clear.
	const shrunkSrc = `@dataset acme.demo.v1.Trade(id, qty)
("T-1", 10)
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "file:///work/t.pxf", 2, shrunkSrc)))

	doc = srv.Documents().Get("file:///work/t.pxf")
	require.NotNil(t, doc)
	require.Len(t, doc.DatasetRegions, 1)
	assert.Equal(t, 1, doc.DatasetRegions[0].RowCount,
		"didChange shrinks the region to one row")
	assert.False(t, doc.DatasetRegions[0].Huge,
		"row count dropped below threshold → Huge must clear (editor needs both directions to render correctly)")
}

func TestEnrichAST_DegradedWhenParseFails(t *testing.T) {
	// Unparseable PXF → line-based scanner still emits a region;
	// AST pass is a no-op; RowCount stays -1, Resolvable false.
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@dataset acme.demo.v1.Trade(id, qty)
not valid pxf at all !!!
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	doc := srv.Documents().Get("file:///work/t.pxf")
	require.NotNil(t, doc)
	require.Len(t, doc.DatasetRegions, 1,
		"line-based scanner emits a region even when parse fails")
	r := doc.DatasetRegions[0]
	assert.Equal(t, -1, r.RowCount,
		"degraded state: AST pass didn't run; RowCount stays at the line-pass default")
	assert.False(t, r.Resolvable)
}

// enableLegacyDatasetRegions flips the legacy-client gate so
// tests that pre-date #113 can exercise the LSP notification
// path without the publish-gate suppressing it.
func enableLegacyDatasetRegions(srv *Server) {
	srv.legacyLspClients.datasetRegions.Store(true)
}

// TestDatasetRegionsChanged_GatedUntilLegacyDispatchCalled pins
// the #113 gate: didOpen with a @dataset block doesn't push the
// LSP notification when no client this session has called the
// legacy protolsp/datasetRegions request. RPC subscribers still
// see the publish.
func TestDatasetRegionsChanged_GatedUntilLegacyDispatchCalled(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))
	assert.Empty(t, fc.datasetRegionsChangedNotifications(),
		"no notification until the legacy datasetRegions request is invoked")
}

func TestDatasetRegionsChanged_FiresOnDidOpenWithRegion(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	enableLegacyDatasetRegions(srv)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))

	notes := fc.datasetRegionsChangedNotifications()
	require.Len(t, notes, 1,
		"didOpen on a document with a @dataset → one notification")
	assert.Equal(t, int32(1), notes[0].Version)
	require.Len(t, notes[0].Regions, 1)
	assert.Equal(t, "acme.demo.v1.Trade", notes[0].Regions[0].TypeFQN)
	assert.Equal(t, 3, notes[0].Regions[0].RowCount,
		"notification carries the AST-enriched RowCount")
}

func TestDatasetRegionsChanged_SilentForDocumentWithoutRegions(t *testing.T) {
	srv, fc, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))
	assert.Empty(t, fc.datasetRegionsChangedNotifications(),
		"document with no @dataset block fires no notification")
}

func TestDatasetRegionsChanged_FiresOnDidChange_RegionAdded(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	enableLegacyDatasetRegions(srv)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	// Open WITHOUT a dataset, then add one via didChange.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, "")))
	require.Empty(t, fc.datasetRegionsChangedNotifications(),
		"empty initial doc → no notification")

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "file:///work/t.pxf", 2, tradeDatasetSrc)))

	notes := fc.datasetRegionsChangedNotifications()
	require.Len(t, notes, 1,
		"didChange that adds a @dataset fires one notification")
	assert.Equal(t, int32(2), notes[0].Version,
		"notification carries the post-edit document version")
}

func TestDatasetRegionsChanged_SilentOnIdenticalReopen(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	enableLegacyDatasetRegions(srv)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))
	require.Len(t, fc.datasetRegionsChangedNotifications(), 1,
		"first open fires once")

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "file:///work/t.pxf", 2, tradeDatasetSrc)))

	assert.Len(t, fc.datasetRegionsChangedNotifications(), 1,
		"didChange with identical content adds no further notification")
}
