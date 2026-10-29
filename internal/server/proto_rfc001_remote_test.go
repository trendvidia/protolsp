// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// RFC-001 remote resolution (#219 Phase C, second half): references
// into imports the protoindex doesn't hold resolve through the
// schema lookup — as served source when GetSource works, else from
// the compiled descriptor's protowire.schema.v1 carrier options.
// Plus the position-encoding round-trip criterion for navigation
// ranges.

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	schemav1 "github.com/trendvidia/protocompile/gen/protowire/schema/v1"
)

// remoteRFC001Doc references three RFC-001 declarations from
// acme/remote.proto — an import no workspace index holds, so every
// resolution goes through the schema lookup.
//
// Line 6 col layout: `audited` spans 16–23, `is_token` 39–47.
// Line 8: `Token` spans 17–22.
const remoteRFC001Doc = `syntax = "proto3";

package acme.app.v1;

import "acme/remote.proto";

@acme.remote.v1.audited(acme.remote.v1.is_token)
message Foo {
  acme.remote.v1.Token id = 1;
}
`

// remoteRFC001Source is the source form of acme/remote.proto served
// by GetSource in the served-source test.
const remoteRFC001Source = `syntax = "proto3";

package acme.remote.v1;

// Compliance audit marker.
annotation audited(by: string);

// Opaque access token.
type Token = string;

function is_token(s: string);

message Anchor {}
`

// buildRemoteRFC001MessageType constructs the compiled-descriptor
// form of acme/remote.proto: an anchor message (the fakeLookup finds
// files through registered types) whose FileOptions carry the
// RFC-001 declarations + source map the protowire compiler embeds.
// Locations point at the declaration names in remoteRFC001Source so
// the carrier and served-source tests agree about the file layout.
func buildRemoteRFC001MessageType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	opts := &descriptorpb.FileOptions{}
	proto.SetExtension(opts, schemav1.E_AnnotationDecls, &schemav1.FileAnnotationDecls{
		Declarations: []*schemav1.AnnotationDecl{{
			Name:     "audited",
			Params:   []*schemav1.AnnotationParam{{Name: "by", Type: schemav1.ParamType_STRING}},
			Location: &schemav1.SourceLocation{File: "acme/remote.proto", Line: 6, Column: 12},
		}},
	})
	proto.SetExtension(opts, schemav1.E_TypeDecls, &schemav1.FileTypeDecls{
		Declarations: []*schemav1.TypeDecl{{
			Name:        "Token",
			BaseTypeFqn: "string",
			Location:    &schemav1.SourceLocation{File: "acme/remote.proto", Line: 9, Column: 6},
		}},
	})
	proto.SetExtension(opts, schemav1.E_Functions, &schemav1.FileFunctions{
		Declarations: []*schemav1.FunctionDecl{{
			Name:     "is_token",
			Params:   []*schemav1.FunctionParam{{Name: "s", Type: "string"}},
			Location: &schemav1.SourceLocation{File: "acme/remote.proto", Line: 11, Column: 10},
		}},
	})
	proto.SetExtension(opts, schemav1.E_SourceMap, &schemav1.SourceMap{File: "acme/remote.proto"})

	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("acme/remote.proto"),
		Package:     proto.String("acme.remote.v1"),
		Syntax:      proto.String("proto3"),
		Options:     opts,
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Anchor")}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().Get(0))
}

// openRemoteRFC001Doc stages a server whose schema lookup serves the
// compiled remote.proto (optionally with source bytes) and opens the
// referencing document.
func openRemoteRFC001Doc(t *testing.T, withSource bool) (*Server, string) {
	t.Helper()
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.addWithOrigin(buildRemoteRFC001MessageType(t), "remote-ns")
	if withSource {
		lookup.addSource("acme/remote.proto", []byte(remoteRFC001Source))
	}
	srv.Lookups().Swap(lookup, nil)

	const u = "file:///work/app.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, remoteRFC001Doc)))
	return srv, u
}

// Descriptor-only: GetSource misses, so hover + definition come from
// the carrier options — signature cards without doc comments, and
// declaration-site ranges from the carried SourceLocations.
func TestProtoNavigation_RFC001DescriptorOnlyFallback(t *testing.T) {
	srv, u := openRemoteRFC001Doc(t, false)

	cases := []struct {
		name       string
		line, char uint32
		wantMD     string
		wantRng    protocol.Range
	}{
		{"annotation use", 6, 18, "**annotation audited(by: string)**", rng(5, 11, 5, 12)},
		{"function ref", 6, 41, "**function is_token(s: string)**", rng(10, 9, 10, 10)},
		{"alias field type", 8, 19, "**type Token = string**", rng(8, 5, 8, 6)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hover := hoverFromHandle(t, srv, u, c.line, c.char)
			require.NotNil(t, hover, "descriptor-only reference yields a hover")
			assert.Contains(t, hover.Contents.Value, c.wantMD)
			assert.Contains(t, hover.Contents.Value, "Defined in `acme/remote.proto`")

			locs := definitionFromHandle(t, srv, u, c.line, c.char)
			require.Len(t, locs, 1, "definition resolves via the carrier")
			assert.Equal(t, "registry://remote-ns/acme/remote.proto", string(locs[0].URI))
			assert.Equal(t, c.wantRng, locs[0].Range)
		})
	}
}

// Served source: GetSource works, so the remote file parses and the
// cards carry doc comments; ranges are exact name-token spans.
func TestProtoNavigation_RFC001ServedSourceFallback(t *testing.T) {
	srv, u := openRemoteRFC001Doc(t, true)

	hover := hoverFromHandle(t, srv, u, 6, 18)
	require.NotNil(t, hover)
	assert.Contains(t, hover.Contents.Value, "**annotation audited(by: string)**")
	assert.Contains(t, hover.Contents.Value, "Compliance audit marker.",
		"served source carries the doc comment the carrier can't")

	locs := definitionFromHandle(t, srv, u, 6, 18)
	require.Len(t, locs, 1)
	assert.Equal(t, "registry://remote-ns/acme/remote.proto", string(locs[0].URI))
	// `audited` in `annotation audited(by: string);` — line 5 of the
	// served source, exact token span.
	assert.Equal(t, rng(5, 11, 5, 18), locs[0].Range)

	// Alias reference resolves through the parsed source too.
	hover = hoverFromHandle(t, srv, u, 8, 19)
	require.NotNil(t, hover)
	assert.Contains(t, hover.Contents.Value, "Opaque access token.")
	locs = definitionFromHandle(t, srv, u, 8, 19)
	require.Len(t, locs, 1)
	assert.Equal(t, rng(8, 5, 8, 10), locs[0].Range)
}

// Pre-v1.2 toolchains emit no carrier extensions: with no source and
// no carrier, references degrade to "no hover / no definition"
// rather than erroring — #219's graceful-degradation criterion.
func TestProtoNavigation_RFC001RemoteFallback_PreV12Degrades(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("acme/remote.proto"),
		Package:     proto.String("acme.remote.v1"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Anchor")}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	lookup.addWithOrigin(dynamicpb.NewMessageType(fd.Messages().Get(0)), "remote-ns")
	srv.Lookups().Swap(lookup, nil)

	const u = "file:///work/app.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, remoteRFC001Doc)))

	assert.Nil(t, hoverFromHandle(t, srv, u, 6, 18),
		"no carrier + no source → no hover")
	assert.Empty(t, definitionFromHandle(t, srv, u, 6, 18),
		"no carrier + no source → no definition")
}

// Navigation ranges must count columns in the negotiated encoding
// (#219 criterion 4). The declaration line carries a non-ASCII
// comment before the name, so UTF-8 (byte) and UTF-16 (code unit)
// columns genuinely diverge: é is 2 bytes / 1 code unit.
func TestProtoDefinition_RFC001_PositionEncodingRoundTrip(t *testing.T) {
	const doc = `syntax = "proto3";

package t;

/* café */ annotation checked(by: string);

@checked("x")
message M {}
`
	// `checked` on line 4: bytes 23–30, UTF-16 units 22–29.
	cases := []struct {
		name      string
		encodings []string
		want      protocol.Range
	}{
		{"utf-16 (default)", []string{"utf-16"}, rng(4, 22, 4, 29)},
		{"utf-8 negotiated", []string{"utf-8"}, rng(4, 23, 4, 30)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _ := initializeWithPositionEncodings(t, c.encodings)
			const u = "file:///work/enc.proto"
			require.NoError(t, srv.Handle(context.Background(), noopReply,
				didOpenCall(t, u, 1, doc)))

			locs := definitionFromHandle(t, srv, u, 6, 3)
			require.Len(t, locs, 1)
			assert.Equal(t, c.want, locs[0].Range,
				"declaration range counts columns in the negotiated encoding")
		})
	}
}
