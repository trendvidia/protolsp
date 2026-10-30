// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

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

// buildAppConfigWithNestedDb constructs an AppConfig message with
// scalar fields + a nested DbConfig submessage, exercising the
// dotted-path resolver:
//
//	package c4test.v1;
//	message DbConfig {
//	  string host = 1;
//	  int32  port = 2;
//	}
//	message AppConfig {
//	  string   api_url = 1;
//	  bool     debug   = 2;
//	  DbConfig db      = 3;
//	}
func buildAppConfigWithNestedDb(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("c4test/v1/app_config.proto"),
		Package: proto.String("c4test.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("DbConfig"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("host"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("host"),
					},
					{
						Name:     proto.String("port"),
						Number:   proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("port"),
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
					},
					{
						Name:     proto.String("debug"),
						Number:   proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("debug"),
					},
					{
						Name:     proto.String("db"),
						Number:   proto.Int32(3),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						TypeName: proto.String(".c4test.v1.DbConfig"),
						JsonName: proto.String("db"),
					},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	// AppConfig is the second message (DbConfig is first); pick it
	// explicitly so the test name is descriptive.
	for i := 0; i < fd.Messages().Len(); i++ {
		m := fd.Messages().Get(i)
		if m.Name() == "AppConfig" {
			return dynamicpb.NewMessageType(m)
		}
	}
	t.Fatal("AppConfig not found in file descriptor")
	return nil
}

// newAppConfigServer is a thin wrapper that gives the C4 tests a
// Server with the nested-AppConfig fixture pre-bound — the
// equivalent of newServerWithLookup but for the field-reference
// schema.
func newAppConfigServer(t *testing.T) (*Server, *fakeConn) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildAppConfigWithNestedDb(t))
	srv.Lookups().Swap(lookup, nil)
	return srv, fc
}

// fieldRefDiags filters the published diagnostics down to those
// our C4 path emits (Source = "protolsp/schema" + message
// containing "reference"). Keeps the assertions focused even when
// other paths (chain-required, etc.) co-emit.
func fieldRefDiags(diags []protocol.Diagnostic) []protocol.Diagnostic {
	var out []protocol.Diagnostic
	for _, d := range diags {
		if optValue(d.Source) == "protolsp/schema" &&
			len(messageText(d.Message)) >= len("reference ") && messageText(d.Message)[:len("reference ")] == "reference " {
			out = append(out, d)
		}
	}
	return out
}

func TestFieldRefs_ResolvableReferenceProducesNoDiag(t *testing.T) {
	srv, fc := newAppConfigServer(t)

	const src = `@type c4test.v1.AppConfig

api_url = "https://${db.host}:${db.port}/v1"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, src)))

	diags := fieldRefDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags,
		"references to existing fields surface no diagnostic")
}

func TestFieldRefs_UnknownTopLevelFieldEmitsDiag(t *testing.T) {
	srv, fc := newAppConfigServer(t)

	const src = `@type c4test.v1.AppConfig

api_url = "https://${nonexistent}/v1"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, src)))

	diags := fieldRefDiags(fc.calls()[0].params.Diagnostics)
	require.Len(t, diags, 1)
	d := diags[0]
	assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity)
	assert.Contains(t, messageText(d.Message), "nonexistent")
	assert.Contains(t, messageText(d.Message), "c4test.v1.AppConfig",
		"diagnostic names the bound message FQN")
}

func TestFieldRefs_UnknownNestedSegmentEmitsDiag(t *testing.T) {
	srv, fc := newAppConfigServer(t)

	const src = `@type c4test.v1.AppConfig

api_url = "${db.missing}"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, src)))

	diags := fieldRefDiags(fc.calls()[0].params.Diagnostics)
	require.Len(t, diags, 1,
		"parent exists, child doesn't → one diagnostic")
	assert.Contains(t, messageText(diags[0].Message), "db.missing")
}

func TestFieldRefs_DescendingPastScalarEmitsDiag(t *testing.T) {
	srv, fc := newAppConfigServer(t)

	const src = `@type c4test.v1.AppConfig

api_url = "${db.host.too.deep}"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, src)))

	diags := fieldRefDiags(fc.calls()[0].params.Diagnostics)
	require.Len(t, diags, 1,
		"can't descend into a scalar field — diagnostic fires")
	assert.Contains(t, messageText(diags[0].Message), "db.host.too.deep")
}

func TestFieldRefs_EnvAndCtxPrefixesAreSkipped(t *testing.T) {
	srv, fc := newAppConfigServer(t)

	const src = `@type c4test.v1.AppConfig

api_url = "https://${env.HOST}:${ctx.region}/v1"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, src)))

	diags := fieldRefDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags,
		"env.* and ctx.* are context interpolations, not field references — no diag")
}

func TestFieldRefs_NonStringValuesIgnored(t *testing.T) {
	// `debug = false` is a bool; even if we hand-crafted a literal
	// `${nope}` here it wouldn't be collected (the AST walk
	// inspects only StringVals).
	srv, fc := newAppConfigServer(t)

	const src = `@type c4test.v1.AppConfig

debug = false
api_url = "ok"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, src)))

	diags := fieldRefDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags)
}

func TestFieldRefs_MultipleReferencesEmitOnePerBadPath(t *testing.T) {
	srv, fc := newAppConfigServer(t)

	const src = `@type c4test.v1.AppConfig

api_url = "${one} ${two} ${db.host}"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, src)))

	diags := fieldRefDiags(fc.calls()[0].params.Diagnostics)
	require.Len(t, diags, 2,
		"two bad references → two diagnostics; the resolvable one is silent")
}

func TestFieldRefs_NoTypeDirectiveSkipsCheck(t *testing.T) {
	srv, fc := newAppConfigServer(t)

	// No @type → no schema bound → no field-ref diagnostics.
	const src = `api_url = "${anything}"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, src)))

	diags := fieldRefDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags,
		"no @type → no descriptor to resolve against, skip the check")
}

func TestFieldRefs_NoLookupSkipsCheck(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	// No lookup assigned.

	const src = `@type c4test.v1.AppConfig

api_url = "${anything}"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, src)))

	diags := fieldRefDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags,
		"no Lookup configured → no field-ref check, no diagnostics")
}

func TestFieldRefs_UnresolvedTypeSkipsCheck(t *testing.T) {
	// @type names a message the lookup doesn't have. The existing
	// "unknown type" diagnostic fires; the field-ref check
	// shouldn't compound it.
	srv, fc := newServerWithFakeConn(t)
	srv.Lookups().Swap(newFakeLookup(), nil) // empty

	const src = `@type c4test.v1.AppConfig

api_url = "${anything}"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, src)))

	diags := fieldRefDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags,
		"unknown @type → defer to the schema-unknown diagnostic; no field-ref errors")
}

// ---------------------------------------------------------------------------
// Unit-level checks of the helpers
// ---------------------------------------------------------------------------
