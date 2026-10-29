// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// shadowProtoFixture: a self-contained .proto with an in-file type
// reference (Priority) so hover + definition have something to
// resolve without any import / disk index.
//
// Line map (0-indexed): 0 `syntax`, 2 `message Greeting {`,
// 3 `  string name = 1;`, 4 `  Priority priority = 2;`,
// 5 `}`, 7 `enum Priority {`.
const shadowProtoFixture = `syntax = "proto3";

message Greeting {
  string name = 1;
  Priority priority = 2;
}

enum Priority {
  P_UNSPECIFIED = 0;
  P_HIGH = 1;
}
`

func TestIsRegistryProtoURI(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"registry://acme/greet/v1/greet.proto", true},
		{"registry://_internal_/protolsp/v1/config.proto", true},
		{"file:///work/greet.proto", false},           // on-disk file → analyze pipeline, not shadow
		{"registry://acme/greet/v1/greet.pxf", false}, // not a proto
		{"untitled://foo.proto", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, IsRegistryProtoURI(uri.URI(tc.in)), "IsRegistryProtoURI(%q)", tc.in)
	}
}

func TestOpenShadowProto_HoverResolvesInBuffer(t *testing.T) {
	w := New(nil)
	const u = "registry://acme/greet/v1/greet.proto"
	w.OpenShadowProto(uri.URI(u), shadowProtoFixture)

	// The doc is tracked with a parsed AST but never validated.
	doc := w.Documents().Get(uri.URI(u))
	require.NotNil(t, doc, "shadow doc is tracked in the store")
	require.NotNil(t, doc.protoAST, "shadow parse populated the AST")

	// Line 4 (`  Priority priority = 2;`), char 4 → middle of the
	// "Priority" type reference. Hover routes through the proto AST
	// handler for a registry:// URI.
	h := w.HoverFor(protocol.DocumentURI(u), protocol.Position{Line: 4, Character: 4})
	require.NotNil(t, h, "hover resolves the in-file type reference")
	assert.Contains(t, h.Contents.Value, "Priority", "hover card names the resolved type")
}

func TestOpenShadowProto_DefinitionJumpsWithinBuffer(t *testing.T) {
	w := New(nil)
	const u = "registry://acme/greet/v1/greet.proto"
	w.OpenShadowProto(uri.URI(u), shadowProtoFixture)

	// Cursor on the "Priority" type ref (line 4) jumps to the enum
	// declaration (line 7) — and, crucially, the target URI is the
	// registry:// buffer itself, not a bogus file:// path derived
	// from the relative parse filename.
	loc := w.DefinitionFor(protocol.DocumentURI(u), protocol.Position{Line: 4, Character: 4})
	require.NotNil(t, loc, "in-file type reference resolves to a location")
	assert.Equal(t, uri.URI(u), uri.URI(loc.URI),
		"within-buffer definition stays on the registry:// URI")
	assert.Equal(t, uint32(7), loc.Range.Start.Line,
		"jump lands on the enum declaration line")
}

func TestOpenShadowProto_Idempotent(t *testing.T) {
	w := New(nil)
	const u = "registry://acme/greet/v1/greet.proto"
	w.OpenShadowProto(uri.URI(u), shadowProtoFixture)
	v1 := w.Documents().Get(uri.URI(u)).Version
	// Re-open with fresh bytes (e.g. after a lookup rebuild): refreshes
	// in place, bumps the version, re-parses.
	w.OpenShadowProto(uri.URI(u), shadowProtoFixture)
	doc := w.Documents().Get(uri.URI(u))
	require.NotNil(t, doc)
	assert.Greater(t, doc.Version, v1, "re-open bumps the version")
	assert.NotNil(t, doc.protoAST, "re-open re-parses the AST")
}

func TestOpenShadowProto_IgnoresNonRegistryProto(t *testing.T) {
	w := New(nil)
	// A file:// URI must NOT be tracked by the shadow path — those
	// go through the real analyze pipeline. Passing one is a no-op.
	w.OpenShadowProto(uri.URI("file:///work/greet.proto"), shadowProtoFixture)
	assert.Nil(t, w.Documents().Get(uri.URI("file:///work/greet.proto")),
		"non-registry URIs are not tracked by OpenShadowProto")
}
