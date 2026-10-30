// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// fieldOptionsWithDefault returns a FieldOptions with the
// (pxf.default) extension (field 1315, string type) embedded in
// the unknown-bytes fallback path that pxf.Default reads when
// descriptors weren't built against a registered extension.
// Mirrors fieldOptionsWithRequired for pxf.required (chain_required_test.go).
func fieldOptionsWithDefault(t *testing.T, literal string) *descriptorpb.FieldOptions {
	t.Helper()
	var buf []byte
	buf = protowire.AppendTag(buf, 1315, protowire.BytesType)
	buf = protowire.AppendString(buf, literal)
	opts := &descriptorpb.FieldOptions{}
	require.NoError(t, proto.Unmarshal(buf, opts))
	return opts
}

// buildAppConfigWithDefaults constructs an AppConfig variant with
// three fields carrying pxf.default annotations:
//
//	package c7test.v1;
//	message Inner { string label = 1 [(pxf.default) = "\"default-label\""]; }
//	message AppConfig {
//	  string api_url = 1 [(pxf.default) = "\"https://example.com\""];
//	  int32  port    = 2 [(pxf.default) = "8080"];
//	  bool   debug   = 3 [(pxf.default) = "true"];
//	  string nodef   = 4;  // no annotation
//	  Inner  inner   = 5;
//	}
func buildAppConfigWithDefaults(t *testing.T) protoreflect.MessageType {
	t.Helper()
	innerFile := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("c7test/v1/app_config.proto"),
		Package: proto.String("c7test.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Inner"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("label"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("label"),
						Options:  fieldOptionsWithDefault(t, `"default-label"`),
					},
				},
			},
			{
				Name: proto.String("AppConfig"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("api_url"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("apiUrl"),
						Options:  fieldOptionsWithDefault(t, `"https://example.com"`),
					},
					{
						Name:     proto.String("port"),
						Number:   proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("port"),
						Options:  fieldOptionsWithDefault(t, "8080"),
					},
					{
						Name:     proto.String("debug"),
						Number:   proto.Int32(3),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("debug"),
						Options:  fieldOptionsWithDefault(t, "true"),
					},
					{
						Name:     proto.String("nodef"),
						Number:   proto.Int32(4),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("nodef"),
					},
					{
						Name:     proto.String("inner"),
						Number:   proto.Int32(5),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						TypeName: proto.String(".c7test.v1.Inner"),
						JsonName: proto.String("inner"),
					},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(innerFile, nil)
	require.NoError(t, err)
	for i := 0; i < fd.Messages().Len(); i++ {
		m := fd.Messages().Get(i)
		if m.Name() == "AppConfig" {
			return dynamicpb.NewMessageType(m)
		}
	}
	t.Fatal("AppConfig not found")
	return nil
}

func newDefaultsServer(t *testing.T) (*Server, *fakeConn) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildAppConfigWithDefaults(t))
	srv.Lookups().Swap(lookup, nil)
	return srv, fc
}

// defaultHints filters published diagnostics to the C7
// "matches default" hints — Source=protolsp/schema +
// Hint severity + the canonical message prefix.
func defaultHints(diags []protocol.Diagnostic) []protocol.Diagnostic {
	var out []protocol.Diagnostic
	for _, d := range diags {
		if optValue(d.Source) == "protolsp/schema" &&
			d.Severity == protocol.DiagnosticSeverityHint &&
			strings.Contains(messageText(d.Message), "matches default") {
			out = append(out, d)
		}
	}
	return out
}

func TestDefaultRedundancy_StringMatchEmitsHint(t *testing.T) {
	srv, fc := newDefaultsServer(t)
	const src = `@type c7test.v1.AppConfig

api_url = "https://example.com"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	hints := defaultHints(fc.calls()[0].params.Diagnostics)
	require.Len(t, hints, 1)
	assert.Equal(t, protocol.DiagnosticSeverityHint, hints[0].Severity)
	assert.Contains(t, messageText(hints[0].Message), "api_url")
	assert.Contains(t, messageText(hints[0].Message), `"https://example.com"`,
		"hint quotes the default literal so users see what the schema would provide")
}

func TestDefaultRedundancy_IntMatchEmitsHint(t *testing.T) {
	srv, fc := newDefaultsServer(t)
	const src = `@type c7test.v1.AppConfig

port = 8080
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	hints := defaultHints(fc.calls()[0].params.Diagnostics)
	require.Len(t, hints, 1)
	assert.Contains(t, messageText(hints[0].Message), "port")
}

func TestDefaultRedundancy_BoolMatchEmitsHint(t *testing.T) {
	srv, fc := newDefaultsServer(t)
	const src = `@type c7test.v1.AppConfig

debug = true
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	hints := defaultHints(fc.calls()[0].params.Diagnostics)
	require.Len(t, hints, 1)
	assert.Contains(t, messageText(hints[0].Message), "debug")
}

func TestDefaultRedundancy_MismatchEmitsNothing(t *testing.T) {
	srv, fc := newDefaultsServer(t)
	const src = `@type c7test.v1.AppConfig

api_url = "https://different.example"
port = 9090
debug = false
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	hints := defaultHints(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, hints,
		"every assignment differs from the field's default — no hints")
}

func TestDefaultRedundancy_FieldWithoutAnnotationIsSilent(t *testing.T) {
	srv, fc := newDefaultsServer(t)
	const src = `@type c7test.v1.AppConfig

nodef = "anything"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	hints := defaultHints(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, hints,
		"fields without a pxf.default annotation never emit the hint")
}

func TestDefaultRedundancy_NestedFieldEmitsHint(t *testing.T) {
	srv, fc := newDefaultsServer(t)
	const src = `@type c7test.v1.AppConfig

inner {
  label = "default-label"
}
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	hints := defaultHints(fc.calls()[0].params.Diagnostics)
	require.Len(t, hints, 1)
	assert.Contains(t, messageText(hints[0].Message), "label",
		"nested-block fields get the same redundancy treatment as top-level")
}

func TestDefaultRedundancy_MultipleFieldsEmitMultipleHints(t *testing.T) {
	srv, fc := newDefaultsServer(t)
	const src = `@type c7test.v1.AppConfig

api_url = "https://example.com"
port = 8080
debug = true
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	hints := defaultHints(fc.calls()[0].params.Diagnostics)
	assert.Len(t, hints, 3,
		"each redundant assignment gets its own hint")
}

func TestDefaultRedundancy_NoSchemaSkipsCheck(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	// No lookup.
	const src = `@type c7test.v1.AppConfig

api_url = "https://example.com"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	hints := defaultHints(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, hints, "no schema bound → no annotations to read → no hints")
}

func TestDefaultRedundancy_NoTypeDirectiveSkipsCheck(t *testing.T) {
	srv, fc := newDefaultsServer(t)
	const src = `api_url = "https://example.com"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	hints := defaultHints(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, hints, "no @type → no descriptor to walk")
}

// ---------------------------------------------------------------------------
// Hover: pxf.default surfaces in the field's hover markdown.
// ---------------------------------------------------------------------------

func TestHover_SurfacesDefaultAnnotation(t *testing.T) {
	srv, _ := newDefaultsServer(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work")
	const src = `@type c7test.v1.AppConfig

api_url = "anything"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	md := hoverAt(t, srv, "file:///work/c.pxf", 2, 0)
	require.NotEmpty(t, md)
	assert.Contains(t, md, "Default:",
		"hover includes a Default annotation line for fields with pxf.default")
	assert.Contains(t, md, `"https://example.com"`,
		"the default literal renders verbatim as a PXF value")
}

func TestHover_OmitsDefaultLineWhenAnnotationAbsent(t *testing.T) {
	srv, _ := newDefaultsServer(t)
	srv.Workspaces().SetWorkspaceRoot("file:///work")
	const src = `@type c7test.v1.AppConfig

nodef = "anything"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	md := hoverAt(t, srv, "file:///work/c.pxf", 2, 0)
	require.NotEmpty(t, md, "hover still renders the field's type + file info")
	assert.NotContains(t, md, "Default:",
		"hover skips the Default line for fields without pxf.default")
}
