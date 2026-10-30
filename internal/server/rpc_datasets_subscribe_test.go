// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
	"github.com/trendvidia/protolsp/proto/protolsp/v1/rpc/rpcpbconnect"
)

// TestDatasetsSubscribe_SnapshotPlusChanges mirrors the secrets
// streaming test for the parallel DatasetsService surface. First
// message is the current snapshot; an edit that shifts the
// DatasetRegion shape (row count, type FQN, etc.) fires a change
// event with the updated regions.
func TestDatasetsSubscribe_SnapshotPlusChanges(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	// Open a doc with one @dataset region containing three rows.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))

	client := rpcpbconnect.NewDatasetsServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, connect.NewRequest(&rpcpb.DatasetsServiceSubscribeRequest{
		TextDocument: &rpcpb.TextDocumentIdentifier{Uri: "file:///work/t.pxf"},
	}))
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	// First message: snapshot — one region for the @dataset block.
	if !stream.Receive() {
		t.Fatalf("Receive returned false; stream.Err()=%v", stream.Err())
	}
	snapshot := stream.Msg()
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Regions, 1, "snapshot carries the existing region")
	assert.Equal(t, "acme.demo.v1.Trade", snapshot.Regions[0].TypeFqn)
	assert.Equal(t, int32(3), snapshot.Regions[0].RowCount,
		"snapshot row count matches the three rows in tradeDatasetSrc")

	// Edit the doc — drop a row. Row count goes 3→2, regions differ.
	const tradeDatasetTwoRowsSrc = `@dataset acme.demo.v1.Trade(id, qty)
("T-1", 10)
("T-2", 20)
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/t.pxf", 2, tradeDatasetTwoRowsSrc)))

	require.True(t, stream.Receive(), "change event after row drop")
	change := stream.Msg()
	require.NotNil(t, change)
	assert.Equal(t, int32(2), change.Version)
	require.Len(t, change.Regions, 1)
	assert.Equal(t, int32(2), change.Regions[0].RowCount,
		"change event reflects the new row count")
}

// TestDatasetsSubscribe_NoChurnOnByteEqualEdits confirms the dedup
// behavior: an edit that doesn't shift the DatasetRegion table
// (here: re-apply the same source verbatim) doesn't fire a
// Subscribe event. Mirrors the secrets-side no-churn test.
func TestDatasetsSubscribe_NoChurnOnByteEqualEdits(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))

	client := rpcpbconnect.NewDatasetsServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, connect.NewRequest(&rpcpb.DatasetsServiceSubscribeRequest{
		TextDocument: &rpcpb.TextDocumentIdentifier{Uri: "file:///work/t.pxf"},
	}))
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	// Drain the initial snapshot.
	require.True(t, stream.Receive())
	_ = stream.Msg()

	// Re-apply identical content. DatasetRegion shape is unchanged →
	// datasetRegionsEqual short-circuits notify → no Subscribe event.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/t.pxf", 2, tradeDatasetSrc)))

	// Race the stream against a short deadline.
	done := make(chan bool, 1)
	go func() { done <- stream.Receive() }()
	select {
	case got := <-done:
		t.Fatalf("expected no event for byte-equal region table; got Receive()=%v", got)
	case <-time.After(150 * time.Millisecond):
		cancel()
	}
	<-done
}

// TestDatasetsSubscribe_RejectsEmptyURI pins the InvalidArgument
// contract for callers that forget to set TextDocument.Uri — same
// guard the secrets-side Subscribe carries.
func TestDatasetsSubscribe_RejectsEmptyURI(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	client := rpcpbconnect.NewDatasetsServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, connect.NewRequest(&rpcpb.DatasetsServiceSubscribeRequest{}))
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	// Receive surfaces the InvalidArgument error from the handler.
	require.False(t, stream.Receive(),
		"empty URI must close the stream without delivering any message")
	streamErr := stream.Err()
	require.Error(t, streamErr)
	connectErr := new(connect.Error)
	require.ErrorAs(t, streamErr, &connectErr)
	assert.Equal(t, connect.CodeInvalidArgument, connectErr.Code())
}

// A @dataset bound to an anonymous @proto is sent with an empty
// type_fqn and resolvable set: the row type is known, but its name is
// protolsp's own. A typeless @dataset with nothing to bind to
// is not resolvable.
func TestDatasetsGetRegions_AnonymousProto(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)
	client := rpcpbconnect.NewDatasetsServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)

	for _, tc := range []struct {
		name, src  string
		resolvable bool
	}{
		{"bound", "@proto { string a = 1; }\n@dataset (a)\n(\"x\")\n", true},
		{"orphan", "@dataset (a)\n(\"x\")\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := "file:///work/" + tc.name + ".pxf"
			require.NoError(t, testHandle(srv, context.Background(), noopReply, didOpenCall(t, uri, 1, tc.src)))
			resp, err := client.GetRegions(context.Background(), connect.NewRequest(&rpcpb.DatasetsServiceGetRegionsRequest{
				TextDocument: &rpcpb.TextDocumentIdentifier{Uri: uri},
			}))
			require.NoError(t, err)
			require.Len(t, resp.Msg.Regions, 1)
			assert.Equal(t, "", resp.Msg.Regions[0].TypeFqn)
			assert.Equal(t, tc.resolvable, resp.Msg.Regions[0].Resolvable)
		})
	}
}
