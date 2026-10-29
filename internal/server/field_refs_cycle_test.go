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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// buildSelfRefSchema constructs an AppConfig with five string
// fields named a, b, c, d, e — enough to test cycle topologies
// without bringing in a sub-message. The cycle graph is purely
// over top-level field names.
//
//	package cycletest.v1;
//	message AppConfig {
//	  string a = 1;
//	  string b = 2;
//	  string c = 3;
//	  string d = 4;
//	  string e = 5;
//	}
func buildSelfRefSchema(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fields := []*descriptorpb.FieldDescriptorProto{}
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		fields = append(fields, &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(name),
			Number:   proto.Int32(int32(i + 1)),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			JsonName: proto.String(name),
		})
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("cycletest/v1/app.proto"),
		Package: proto.String("cycletest.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("AppConfig"), Field: fields},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().Get(0))
}

// newCycleServer is a tiny helper mirroring newAppConfigServer
// but with the cycle-only schema (no nested message).
func newCycleServer(t *testing.T) (*Server, *fakeConn) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildSelfRefSchema(t))
	srv.Lookups().Swap(lookup, nil)
	return srv, fc
}

// cycleDiags filters diagnostics down to those produced by the
// cycleReferenceDiagnostics path: source = "protolsp/schema",
// message contains "reference cycle".
func cycleDiags(diags []protocol.Diagnostic) []protocol.Diagnostic {
	var out []protocol.Diagnostic
	for _, d := range diags {
		if d.Source == "protolsp/schema" &&
			strings.Contains(d.Message, "reference cycle") {
			out = append(out, d)
		}
	}
	return out
}

func TestCycleRefs_TwoNodeCycle(t *testing.T) {
	srv, fc := newCycleServer(t)
	const src = `@type cycletest.v1.AppConfig

a = "${b}"
b = "${a}"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	diags := cycleDiags(fc.calls()[0].params.Diagnostics)
	require.Len(t, diags, 2, "both members of an a↔b cycle get a diagnostic")
	for _, d := range diags {
		assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity)
		// The path mentions both a and b in order — exact ordering
		// depends on which DFS branch found the cycle first, but
		// both names appear.
		assert.Contains(t, d.Message, "a")
		assert.Contains(t, d.Message, "b")
		assert.Contains(t, d.Message, "→",
			"diagnostic renders the cycle as a → b → a")
	}
}

func TestCycleRefs_ThreeNodeCycle(t *testing.T) {
	srv, fc := newCycleServer(t)
	const src = `@type cycletest.v1.AppConfig

a = "${b}"
b = "${c}"
c = "${a}"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	diags := cycleDiags(fc.calls()[0].params.Diagnostics)
	require.Len(t, diags, 3, "every member of a 3-node cycle gets a diagnostic")
}

func TestCycleRefs_SelfReference(t *testing.T) {
	srv, fc := newCycleServer(t)
	const src = `@type cycletest.v1.AppConfig

a = "${a}"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	diags := cycleDiags(fc.calls()[0].params.Diagnostics)
	require.Len(t, diags, 1, "a self-reference produces exactly one diagnostic")
	assert.Contains(t, diags[0].Message, "a")
}

func TestCycleRefs_NoCycleProducesNoDiagnostic(t *testing.T) {
	srv, fc := newCycleServer(t)
	const src = `@type cycletest.v1.AppConfig

a = "static"
b = "${a}"
c = "${a} ${b}"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	diags := cycleDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags, "linear dependency graph has no cycles")
}

func TestCycleRefs_PartialCycleAffectsOnlyMembers(t *testing.T) {
	// d has no cycle; a, b, c do. Diagnostics fire only on a, b, c.
	srv, fc := newCycleServer(t)
	const src = `@type cycletest.v1.AppConfig

a = "${b}"
b = "${c}"
c = "${a}"
d = "${a}"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	diags := cycleDiags(fc.calls()[0].params.Diagnostics)
	require.Len(t, diags, 3,
		"node `d` depends on a but isn't in a cycle — no diagnostic on it")
}

func TestCycleRefs_EnvCtxReferencesDontCount(t *testing.T) {
	// a and b reference each other indirectly via env interpolation,
	// but env.* doesn't go through the field graph. No cycle.
	srv, fc := newCycleServer(t)
	const src = `@type cycletest.v1.AppConfig

a = "${env.A}"
b = "${env.B}"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	diags := cycleDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags,
		"env.* refs are runtime sources, not field edges → no cycle")
}

func TestCycleRefs_UnresolvedReferenceSkippedFromGraph(t *testing.T) {
	// `b` doesn't exist on the schema. The reference is reported by
	// the C4 unresolvable path, but it can't form a cycle (since
	// there's no `b` field). a's "cycle through b" doesn't fire.
	srv, fc := newCycleServer(t)
	const src = `@type cycletest.v1.AppConfig

a = "${nonexistent}"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	// Cycle path silent; the unresolvable diagnostic from C4 fires.
	diags := cycleDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags,
		"unresolvable references don't compound into a cycle diagnostic")
}

func TestCycleRefs_NoSchemaSkipsCheck(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	// No lookup.
	const src = `@type cycletest.v1.AppConfig

a = "${b}"
b = "${a}"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	diags := cycleDiags(fc.calls()[0].params.Diagnostics)
	assert.Empty(t, diags,
		"no schema bound → no descriptor to validate paths against, no cycle check")
}

// ---------------------------------------------------------------------------
// Unit-level checks of the graph helpers
// ---------------------------------------------------------------------------
