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
	"go.lsp.dev/uri"
)

func TestBuildAndParseRegistryURI_Roundtrip(t *testing.T) {
	cases := []struct {
		ns, path string
		want     uri.URI
	}{
		{"acme-billing", "billing.proto", "registry://acme-billing/billing.proto"},
		{"acme-demo", "acme/demo/v1/greeting.proto", "registry://acme-demo/acme/demo/v1/greeting.proto"},
		{"shared", "shared/money.proto", "registry://shared/shared/money.proto"},
	}
	for _, c := range cases {
		t.Run(c.ns+"/"+c.path, func(t *testing.T) {
			got := BuildRegistryURI(c.ns, c.path)
			assert.Equal(t, c.want, got, "BuildRegistryURI shape")

			ns, p, ok := ParseRegistryURI(got)
			require.True(t, ok)
			assert.Equal(t, c.ns, ns)
			assert.Equal(t, c.path, p)
		})
	}
}

func TestParseRegistryURI_RejectsMalformed(t *testing.T) {
	bad := []uri.URI{
		"",
		"file:///foo/bar",            // wrong scheme
		"registry:///foo",            // missing namespace
		"registry://ns",              // missing trailing slash + path
		"registry://ns/",             // empty path component
		"registry:relative/path",     // no //
		"http://registry/ns/p.proto", // wrong scheme
	}
	for _, b := range bad {
		t.Run(string(b), func(t *testing.T) {
			_, _, ok := ParseRegistryURI(b)
			assert.False(t, ok, "should reject %q", string(b))
		})
	}
}

// sourceContentReply drives the protolsp/sourceContent dispatcher and
// returns either the decoded result or the JSON-RPC error.
func sourceContentReply(t *testing.T, srv *Server, u string) (*SourceContentResult, *jsonrpc2.Error) {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1), MethodSourceContent,
		&SourceContentParams{URI: uri.URI(u)})
	require.NoError(t, err)

	var got *SourceContentResult
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
		var r SourceContentResult
		require.NoError(t, json.Unmarshal(b, &r))
		got = &r
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	return got, gotErr
}

func TestSourceContent_FetchesFromLookup(t *testing.T) {
	srv, _, lookup := newServerWithLookup(t)
	const protoSrc = `syntax = "proto3"; package acme.demo.v1; message Greeting { string name = 1; }`
	lookup.addSource("acme/demo/v1/greeting.proto", []byte(protoSrc))

	got, errResp := sourceContentReply(t, srv, "registry://acme-demo/acme/demo/v1/greeting.proto")
	require.Nil(t, errResp)
	require.NotNil(t, got)
	assert.Equal(t, protoSrc, got.Content)
}

func TestSourceContent_MalformedURIReturnsInvalidParams(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)

	got, errResp := sourceContentReply(t, srv, "file:///not/a/registry/uri")
	assert.Nil(t, got)
	require.NotNil(t, errResp)
	assert.Equal(t, jsonrpc2.InvalidParams, errResp.Code,
		"malformed URI must surface as InvalidParams, not InternalError")
}

func TestSourceContent_UnknownPathReturnsInternalError(t *testing.T) {
	// fakeLookup.GetSource returns protoregistry.NotFound for paths
	// not added via addSource. The handler wraps that as
	// InternalError — the editor displays the error message in the
	// placeholder document so the user sees what went wrong.
	srv, _, _ := newServerWithLookup(t)
	// No addSource calls — every path is unknown.

	got, errResp := sourceContentReply(t, srv, "registry://acme-demo/missing.proto")
	assert.Nil(t, got)
	require.NotNil(t, errResp)
	assert.Equal(t, jsonrpc2.InternalError, errResp.Code)
	assert.Contains(t, errResp.Message, "missing.proto")
}

func TestSourceContent_NoLookupConfiguredReturnsError(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	// No s.lookup assignment.

	got, errResp := sourceContentReply(t, srv, "registry://acme-demo/x.proto")
	assert.Nil(t, got)
	require.NotNil(t, errResp)
	assert.Equal(t, jsonrpc2.InternalError, errResp.Code,
		"sourceContent has nothing to fetch through without a Lookup")
}

// TestSourceContent_InternalNamespaceServesEmbeddedSource pins the
// go-to-definition-for-internal-types contract: a registry:// URI
// whose authority is the InternalOrigin sentinel returns the .proto
// source bytes protolsp ships in its binary. Reached when an editor
// runs go-to-def on `chameleon.v1.ChainSpec` or
// `protolsp.v1.WorkspaceConfig` in a workspace with no registry
// configured.
func TestSourceContent_InternalNamespaceServesEmbeddedSource(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	// Deliberately do NOT configure a user lookup — the internal
	// path must work standalone.

	got, errResp := sourceContentReply(t, srv,
		"registry://_internal_/chameleon/v1/chain.proto")
	require.Nil(t, errResp, "internal namespace must not require a user lookup")
	require.NotNil(t, got)
	// Don't pin the full content — just confirm it's the right file
	// by looking for the package declaration the .proto opens with.
	assert.Contains(t, got.Content, "package chameleon.v1;",
		"served bytes should be the actual chain.proto source")
}
