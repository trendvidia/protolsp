// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
)

// listMessagesReply drives the protolsp/listMessages dispatcher and
// returns the decoded result or the JSON-RPC error.
func listMessagesReply(t *testing.T, srv *Server, params interface{}) (*ListMessagesResult, *jsonrpc2.Error) {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodListMessages, params)
	require.NoError(t, err)

	var got *ListMessagesResult
	var gotErr *jsonrpc2.Error
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		if replyErr != nil {
			var jsErr *jsonrpc2.Error
			require.ErrorAs(t, replyErr, &jsErr)
			gotErr = jsErr
			return nil
		}
		if result == nil {
			return nil
		}
		b, err := json.Marshal(result)
		require.NoError(t, err)
		var r ListMessagesResult
		require.NoError(t, json.Unmarshal(b, &r))
		got = &r
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	return got, gotErr
}

func TestListMessages_ReturnsAllVisibleTypes(t *testing.T) {
	// newServerWithLookup pre-adds Greeting; we add Order. Both come
	// back sorted by FullName. (The fake's RangeMessages only yields
	// types explicitly add()ed — it doesn't traverse nested messages
	// the way a real client.Resolver's NamespacedTypes does. That's
	// fine here: this test pins the dispatcher's behavior, not the
	// resolver's enumeration semantics.)
	srv, _, lookup := newServerWithLookup(t)
	lookup.add(buildOrderMessageType(t))

	got, errResp := listMessagesReply(t, srv, &ListMessagesParams{})
	require.Nil(t, errResp)
	require.NotNil(t, got)

	names := make([]string, len(got.Messages))
	for i, m := range got.Messages {
		names[i] = m.FullName
	}
	assert.Equal(t, []string{
		"acme.demo.v1.Greeting",
		"acme.demo.v1.Order",
	}, names, "results sorted by FullName, both registered types present")
}

func TestListMessages_IncludesOriginWhenSet(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.addWithOrigin(buildGreetingMessageType(t), "acme-demo")
	srv.Lookups().Swap(lookup, nil)

	got, errResp := listMessagesReply(t, srv, &ListMessagesParams{})
	require.Nil(t, errResp)
	require.NotNil(t, got)
	require.Len(t, got.Messages, 1)
	assert.Equal(t, "acme.demo.v1.Greeting", got.Messages[0].FullName)
	assert.Equal(t, "acme/demo/v1/greeting.proto", got.Messages[0].File)
	assert.Equal(t, "acme-demo", got.Messages[0].Namespace,
		"namespace from FindFileByPathWithOrigin surfaces in the entry")
}

func TestListMessages_EmptyOriginOmittedFromJSON(t *testing.T) {
	// fakeLookup.add() (no origin) → Namespace is "" → JSON
	// omitempty should drop the field. Important for clients that
	// want to distinguish "no namespace tracked" from "namespace =
	// empty string".
	srv, _, _ := newServerWithLookup(t)

	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodListMessages,
		&ListMessagesParams{})
	require.NoError(t, err)

	var rawResult json.RawMessage
	reply := func(_ context.Context, result interface{}, _ error) error {
		b, err := json.Marshal(result)
		require.NoError(t, err)
		rawResult = b
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	assert.NotContains(t, string(rawResult), `"namespace"`,
		"empty namespace should be omitted from the JSON entirely")
}

func TestListMessages_PrefixFilter(t *testing.T) {
	srv, _, lookup := newServerWithLookup(t)
	lookup.add(buildOrderMessageType(t))
	lookup.add(buildTicketMessageType(t))

	got, errResp := listMessagesReply(t, srv,
		&ListMessagesParams{Prefix: "acme.demo.v1.O"})
	require.Nil(t, errResp)
	require.NotNil(t, got)
	require.Len(t, got.Messages, 1, "prefix filter narrows to types whose FQN starts with the prefix")
	assert.Equal(t, "acme.demo.v1.Order", got.Messages[0].FullName)
}

func TestListMessages_NoLookupReturnsEmpty(t *testing.T) {
	// No registry configured → empty list, not an error. UI renders
	// "no schema connected" rather than popping up a JSON-RPC error.
	srv, _ := newServerWithFakeConn(t)
	// No s.lookup assignment.

	got, errResp := listMessagesReply(t, srv, &ListMessagesParams{})
	require.Nil(t, errResp)
	require.NotNil(t, got)
	assert.Empty(t, got.Messages)
}

func TestListMessages_NilParamsTolerated(t *testing.T) {
	// JSON-RPC params are optional. A literal `null` or missing body
	// shouldn't fail — the handler treats them as "no filter."
	srv, _, _ := newServerWithLookup(t)

	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodListMessages, nil)
	require.NoError(t, err)

	var got *ListMessagesResult
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, err := json.Marshal(result)
		require.NoError(t, err)
		var r ListMessagesResult
		require.NoError(t, json.Unmarshal(b, &r))
		got = &r
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	require.NotNil(t, got)
	assert.NotEmpty(t, got.Messages, "null params should behave like empty filter, not error")
}
