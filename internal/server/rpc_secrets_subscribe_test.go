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

// TestSecretsSubscribe_SnapshotPlusChanges drives the end-to-end
// streaming path: subscribe to a freshly-opened doc, receive the
// initial snapshot, edit the doc to add a secret field, receive
// the change event. The first message is always the current
// snapshot so callers don't need a separate GetRanges round trip.
func TestSecretsSubscribe_SnapshotPlusChanges(t *testing.T) {
	srv, _, _ := newServerWithCredentialsType(t)
	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	// Open a doc that has the secret field set from the start.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	client := rpcpbconnect.NewSecretsServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, connect.NewRequest(&rpcpb.SecretsServiceSubscribeRequest{
		TextDocument: &rpcpb.TextDocumentIdentifier{Uri: "file:///work/c.pxf"},
	}))
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	// First message: the snapshot — should include the password
	// field that was set at didOpen time.
	if !stream.Receive() {
		t.Fatalf("Receive returned false; stream.Err()=%v", stream.Err())
	}
	snapshot := stream.Msg()
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Ranges, 1, "snapshot has the existing password field")
	assert.Equal(t, "password", snapshot.Ranges[0].FieldName)

	// Edit the doc — change a non-secret field. notifySecretRangesChanged
	// short-circuits when the secret table is byte-equal, so we'd
	// expect NO push for this edit. Instead, change the doc to
	// remove the secret entirely.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "file:///work/c.pxf", 2,
			"@type acme.demo.v1.Credentials\n\nusername = \"alice\"\n")))

	// Expect one change event with an empty secret-range list.
	require.True(t, stream.Receive(), "change event after secret removal")
	change := stream.Msg()
	require.NotNil(t, change)
	assert.Equal(t, int32(2), change.Version)
	assert.Empty(t, change.Ranges, "removing the secret line empties the range list")
}

// TestSecretsSubscribe_NoChurnOnByteEqualEdits confirms the
// dedup behavior: editing a non-secret field doesn't fire a Subscribe
// event because the secret-range table is unchanged.
func TestSecretsSubscribe_NoChurnOnByteEqualEdits(t *testing.T) {
	srv, _, _ := newServerWithCredentialsType(t)
	endpoint, err := srv.startRPC()
	require.NoError(t, err)
	t.Cleanup(srv.stopRPC)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, secretsScalarSrc)))

	client := rpcpbconnect.NewSecretsServiceClient(
		http.DefaultClient,
		endpoint.Address,
		connect.WithInterceptors(bearerClientInterceptor(endpoint.Token)),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.Subscribe(ctx, connect.NewRequest(&rpcpb.SecretsServiceSubscribeRequest{
		TextDocument: &rpcpb.TextDocumentIdentifier{Uri: "file:///work/c.pxf"},
	}))
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()

	// Drain the initial snapshot.
	require.True(t, stream.Receive())
	_ = stream.Msg()

	// Edit username (non-secret). Secret table stays the same →
	// no event.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, "file:///work/c.pxf", 2,
			"@type acme.demo.v1.Credentials\n\nusername = \"bob\"\npassword = \"super-secret\"\n")))

	// Race the stream against a short deadline: if a spurious event
	// arrives, it shows up here. Cancel after the wait so the
	// stream cleanly tears down.
	done := make(chan bool, 1)
	go func() { done <- stream.Receive() }()
	select {
	case got := <-done:
		t.Fatalf("expected no event for byte-equal secret table; got Receive()=%v", got)
	case <-time.After(150 * time.Millisecond):
		// Success — no event arrived. Cancel to stop the stream.
		cancel()
	}
	// Drain whatever the Receive call ultimately returns post-cancel
	// so the goroutine doesn't leak across t.Cleanup.
	<-done
}
