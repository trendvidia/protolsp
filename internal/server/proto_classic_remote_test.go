// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Remote fallback for classic message/enum references (#229): a
// field type naming a message or enum in an import the protoindex
// doesn't hold resolves through the schema lookup — as served source
// when GetSource works, else straight off the compiled descriptor
// (fd.Messages()/fd.Enums() + SourceCodeInfo declaration sites).
// Mirrors the RFC-001 remote suite (proto_rfc001_remote_test.go);
// the two share the resolveRFC001RemoteDecl plumbing.

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
)

// remoteClassicDoc references a classic message and a classic enum
// from acme/classic.proto — an import no workspace index holds, so
// both references resolve through the schema lookup.
//
// Line 7: `Anchor` spans cols 18–24. Line 8: `Color` spans 18–23.
const remoteClassicDoc = `syntax = "proto3";

package acme.app.v1;

import "acme/classic.proto";

message Profile {
  acme.classic.v1.Anchor anchor = 1;
  acme.classic.v1.Color color = 2;
}
`

// remoteClassicSource is the source form of acme/classic.proto
// served by GetSource in the served-source test. The descriptor's
// SourceCodeInfo spans (buildRemoteClassicMessageType) point at the
// same layout so both layers agree about the file.
const remoteClassicSource = `syntax = "proto3";

package acme.classic.v1;

// Remote anchor message.
message Anchor {
  string id = 1;
}

// Remote color enum.
enum Color {
  COLOR_UNSPECIFIED = 0;
}
`

// buildRemoteClassicMessageType constructs the compiled-descriptor
// form of acme/classic.proto. Unlike the RFC-001 fixture nothing
// rides in carrier options — messages and enums are real descriptors
// — and the declaration sites come from ordinary SourceCodeInfo.
func buildRemoteClassicMessageType(t *testing.T, withSourceInfo bool) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("acme/classic.proto"),
		Package: proto.String("acme.classic.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Anchor"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("id"), Number: proto.Int32(1),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				JsonName: proto.String("id"),
			}},
		}},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Color"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("COLOR_UNSPECIFIED"), Number: proto.Int32(0)},
			},
		}},
	}
	if withSourceInfo {
		fdp.SourceCodeInfo = &descriptorpb.SourceCodeInfo{
			Location: []*descriptorpb.SourceCodeInfo_Location{
				// Whole-declaration spans matching remoteClassicSource.
				{Path: []int32{4, 0}, Span: []int32{5, 0, 7, 1}},   // message Anchor
				{Path: []int32{5, 0}, Span: []int32{10, 0, 12, 1}}, // enum Color
			},
		}
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().Get(0))
}

// openRemoteClassicDoc stages a server whose schema lookup serves the
// compiled classic.proto (optionally with source bytes / source info)
// and opens the referencing document.
func openRemoteClassicDoc(t *testing.T, withSource, withSourceInfo bool) (*Server, string) {
	t.Helper()
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.addWithOrigin(buildRemoteClassicMessageType(t, withSourceInfo), "classic-ns")
	if withSource {
		lookup.addSource("acme/classic.proto", []byte(remoteClassicSource))
	}
	srv.Lookups().Swap(lookup, nil)

	const u = "file:///work/app.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, remoteClassicDoc)))
	return srv, u
}

// Descriptor-only (layer 2): GetSource misses, so hover + definition
// come from the descriptor itself — message/enum cards without doc
// comments, and declaration-site ranges from SourceCodeInfo.
func TestProtoNavigation_ClassicDescriptorOnlyFallback(t *testing.T) {
	srv, u := openRemoteClassicDoc(t, false, true)

	cases := []struct {
		name       string
		line, char uint32
		wantMD     string
		wantRng    protocol.Range
	}{
		{"message field type", 7, 20, "**message Anchor**", rng(5, 0, 7, 1)},
		{"enum field type", 8, 20, "**enum Color**", rng(10, 0, 12, 1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hover := hoverFromHandle(t, srv, u, c.line, c.char)
			require.NotNil(t, hover, "descriptor-only reference yields a hover")
			assert.Contains(t, hover.Contents.Value, c.wantMD)
			assert.Contains(t, hover.Contents.Value, "Defined in `acme/classic.proto`")

			locs := definitionFromHandle(t, srv, u, c.line, c.char)
			require.Len(t, locs, 1, "definition resolves via the descriptor")
			assert.Equal(t, "registry://classic-ns/acme/classic.proto", string(locs[0].URI))
			assert.Equal(t, c.wantRng, locs[0].Range)
		})
	}
}

// Served source (layer 1): GetSource works, so the remote file parses
// and the cards carry doc comments; ranges are exact name-token spans.
func TestProtoNavigation_ClassicServedSourceFallback(t *testing.T) {
	srv, u := openRemoteClassicDoc(t, true, false)

	hover := hoverFromHandle(t, srv, u, 7, 20)
	require.NotNil(t, hover)
	assert.Contains(t, hover.Contents.Value, "**message Anchor**")
	assert.Contains(t, hover.Contents.Value, "Remote anchor message.",
		"served source carries the doc comment the descriptor can't")

	locs := definitionFromHandle(t, srv, u, 7, 20)
	require.Len(t, locs, 1)
	assert.Equal(t, "registry://classic-ns/acme/classic.proto", string(locs[0].URI))
	// `Anchor` in `message Anchor {` — exact token span.
	assert.Equal(t, rng(5, 8, 5, 14), locs[0].Range)

	// Enum reference resolves through the parsed source too.
	hover = hoverFromHandle(t, srv, u, 8, 20)
	require.NotNil(t, hover)
	assert.Contains(t, hover.Contents.Value, "**enum Color**")
	assert.Contains(t, hover.Contents.Value, "Remote color enum.")
	locs = definitionFromHandle(t, srv, u, 8, 20)
	require.Len(t, locs, 1)
	// `Color` in `enum Color {`.
	assert.Equal(t, rng(10, 5, 10, 10), locs[0].Range)
}

// ─── Outer-qualified nested references (#237) ────────────────────

// remoteNestedDoc references a nested message and a nested enum by
// their outer-qualified names — package + enclosing message + leaf.
//
// Line 7: `Inner` spans cols 23–28. Line 8: `Mode` spans 23–27.
const remoteNestedDoc = `syntax = "proto3";

package acme.app.v1;

import "acme/nested.proto";

message Profile {
  acme.nested.v1.Outer.Inner inner = 1;
  acme.nested.v1.Outer.Mode mode = 2;
}
`

// remoteNestedSource is the source form of acme/nested.proto. The
// descriptor's SourceCodeInfo spans point at the same layout.
const remoteNestedSource = `syntax = "proto3";

package acme.nested.v1;

message Outer {
  // Nested anchor message.
  message Inner {
    string id = 1;
  }

  // Nested mode enum.
  enum Mode {
    MODE_UNSPECIFIED = 0;
  }
}
`

// buildRemoteNestedMessageType constructs the compiled-descriptor
// form of acme/nested.proto: nested message + nested enum inside
// Outer, with SourceCodeInfo declaration sites for both.
func buildRemoteNestedMessageType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("acme/nested.proto"),
		Package: proto.String("acme.nested.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Outer"),
			NestedType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("Inner"),
				Field: []*descriptorpb.FieldDescriptorProto{{
					Name: proto.String("id"), Number: proto.Int32(1),
					Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
					Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					JsonName: proto.String("id"),
				}},
			}},
			EnumType: []*descriptorpb.EnumDescriptorProto{{
				Name: proto.String("Mode"),
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: proto.String("MODE_UNSPECIFIED"), Number: proto.Int32(0)},
				},
			}},
		}},
		SourceCodeInfo: &descriptorpb.SourceCodeInfo{
			Location: []*descriptorpb.SourceCodeInfo_Location{
				// Whole-declaration spans matching remoteNestedSource.
				{Path: []int32{4, 0}, Span: []int32{4, 0, 14, 1}},        // message Outer
				{Path: []int32{4, 0, 3, 0}, Span: []int32{6, 2, 8, 3}},   // nested message Inner
				{Path: []int32{4, 0, 4, 0}, Span: []int32{11, 2, 13, 3}}, // nested enum Mode
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().Get(0))
}

// openRemoteNestedDoc mirrors openRemoteClassicDoc for the nested
// fixture.
func openRemoteNestedDoc(t *testing.T, withSource bool) (*Server, string) {
	t.Helper()
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.addWithOrigin(buildRemoteNestedMessageType(t), "nested-ns")
	if withSource {
		lookup.addSource("acme/nested.proto", []byte(remoteNestedSource))
	}
	srv.Lookups().Swap(lookup, nil)

	const u = "file:///work/app.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, remoteNestedDoc)))
	return srv, u
}

// Descriptor-only (layer 2): outer-qualified nested references walk
// the descriptor's enclosing-message chain (#237).
func TestProtoNavigation_NestedDescriptorOnlyFallback(t *testing.T) {
	srv, u := openRemoteNestedDoc(t, false)

	cases := []struct {
		name       string
		line, char uint32
		wantMD     string
		wantRng    protocol.Range
	}{
		{"nested message field type", 7, 24, "**message Inner**", rng(6, 2, 8, 3)},
		{"nested enum field type", 8, 24, "**enum Mode**", rng(11, 2, 13, 3)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hover := hoverFromHandle(t, srv, u, c.line, c.char)
			require.NotNil(t, hover, "descriptor-only nested reference yields a hover")
			assert.Contains(t, hover.Contents.Value, c.wantMD)
			assert.Contains(t, hover.Contents.Value, "Defined in `acme/nested.proto`")

			locs := definitionFromHandle(t, srv, u, c.line, c.char)
			require.Len(t, locs, 1, "definition resolves via the descriptor")
			assert.Equal(t, "registry://nested-ns/acme/nested.proto", string(locs[0].URI))
			assert.Equal(t, c.wantRng, locs[0].Range)
		})
	}
}

// Served source (layer 1): the parsed remote file resolves the
// outer-qualified nested path with doc comments and exact name-token
// spans (#237).
func TestProtoNavigation_NestedServedSourceFallback(t *testing.T) {
	srv, u := openRemoteNestedDoc(t, true)

	hover := hoverFromHandle(t, srv, u, 7, 24)
	require.NotNil(t, hover)
	assert.Contains(t, hover.Contents.Value, "**message Inner**")
	assert.Contains(t, hover.Contents.Value, "Nested anchor message.",
		"served source carries the doc comment the descriptor can't")

	locs := definitionFromHandle(t, srv, u, 7, 24)
	require.Len(t, locs, 1)
	assert.Equal(t, "registry://nested-ns/acme/nested.proto", string(locs[0].URI))
	// `Inner` in `  message Inner {` — exact token span.
	assert.Equal(t, rng(6, 10, 6, 15), locs[0].Range)

	// The nested enum resolves through the parsed source too.
	hover = hoverFromHandle(t, srv, u, 8, 24)
	require.NotNil(t, hover)
	assert.Contains(t, hover.Contents.Value, "**enum Mode**")
	assert.Contains(t, hover.Contents.Value, "Nested mode enum.")
	locs = definitionFromHandle(t, srv, u, 8, 24)
	require.Len(t, locs, 1)
	// `Mode` in `  enum Mode {`.
	assert.Equal(t, rng(11, 7, 11, 11), locs[0].Range)
}

// A nested path whose leaf neither layer holds keeps the graceful
// nil (#237 acceptance).
func TestProtoNavigation_NestedRemoteFallback_MissDegrades(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.addWithOrigin(buildRemoteNestedMessageType(t), "nested-ns")
	srv.Lookups().Swap(lookup, nil)

	const doc = `syntax = "proto3";

package acme.app.v1;

import "acme/nested.proto";

message Profile {
  acme.nested.v1.Outer.Missing m = 1;
}
`
	const u = "file:///work/app.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, doc)))

	assert.Nil(t, hoverFromHandle(t, srv, u, 7, 24),
		"unknown nested remote type → no hover")
	assert.Empty(t, definitionFromHandle(t, srv, u, 7, 24),
		"unknown nested remote type → no definition")
}

// A name neither layer holds degrades to "no hover / no definition"
// rather than erroring — the pre-existing graceful nil.
func TestProtoNavigation_ClassicRemoteFallback_MissDegrades(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.addWithOrigin(buildRemoteClassicMessageType(t, false), "classic-ns")
	srv.Lookups().Swap(lookup, nil)

	const doc = `syntax = "proto3";

package acme.app.v1;

import "acme/classic.proto";

message Profile {
  acme.classic.v1.Missing m = 1;
}
`
	const u = "file:///work/app.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, doc)))

	assert.Nil(t, hoverFromHandle(t, srv, u, 7, 20),
		"unknown remote type → no hover")
	assert.Empty(t, definitionFromHandle(t, srv, u, 7, 20),
		"unknown remote type → no definition")
}
