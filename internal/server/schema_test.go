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
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/trendvidia/protolsp/schema"
)

// fakeLookup is an in-memory schema.Lookup. Test fixtures populate it
// with one or more MessageTypes; missing names return
// protoregistry.NotFound which the server should surface as an
// "unknown type" diagnostic.
//
// origins lets tests attribute a type to a specific namespace ID;
// types added via plain add() default to origin "". addWithOrigin
// sets a non-empty origin for tests exercising hover provenance.
//
// sources lets tests register source bytes for a file path so the
// protolsp/sourceContent handler has something to return. Empty by
// default — tests for that handler populate it explicitly.
type fakeLookup struct {
	types     map[protoreflect.FullName]protoreflect.MessageType
	origins   map[protoreflect.FullName]string
	sources   map[string][]byte
	staleFlag bool
}

func newFakeLookup() *fakeLookup {
	return &fakeLookup{
		types:   map[protoreflect.FullName]protoreflect.MessageType{},
		origins: map[protoreflect.FullName]string{},
		sources: map[string][]byte{},
	}
}

func (f *fakeLookup) add(t protoreflect.MessageType) {
	f.types[t.Descriptor().FullName()] = t
}

func (f *fakeLookup) addWithOrigin(t protoreflect.MessageType, origin string) {
	f.types[t.Descriptor().FullName()] = t
	f.origins[t.Descriptor().FullName()] = origin
}

func (f *fakeLookup) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	if t, ok := f.types[name]; ok {
		return t, nil
	}
	return nil, protoregistry.NotFound
}

// RangeMessages iterates the test fixture's registered types. Map
// iteration order is deterministic enough for completion (clients sort
// the list before display anyway); tests that care about ordering
// should sort the resulting labels themselves.
func (f *fakeLookup) RangeMessages(fn func(protoreflect.MessageType) bool) {
	for _, t := range f.types {
		if !fn(t) {
			return
		}
	}
}

// FindFileByPathWithOrigin scans the fixture's registered types for one
// whose ParentFile().Path() matches, and returns its origin. Tests set
// per-type origins via addWithOrigin; types added via plain add()
// resolve with origin "" (matching what client.Resolver returns for
// ad-hoc parents).
func (f *fakeLookup) FindFileByPathWithOrigin(path string) (protoreflect.FileDescriptor, string, error) {
	for _, t := range f.types {
		fd := t.Descriptor().ParentFile()
		if fd.Path() == path {
			return fd, f.origins[t.Descriptor().FullName()], nil
		}
	}
	return nil, "", protoregistry.NotFound
}

// GetSource returns the source bytes registered for a path via
// addSource(). Returns NotFound for unregistered paths — matches
// client.Resolver semantics so tests can exercise the
// sourceContent error path.
func (f *fakeLookup) GetSource(_ context.Context, filePath string) ([]byte, error) {
	if b, ok := f.sources[filePath]; ok {
		return b, nil
	}
	return nil, protoregistry.NotFound
}

// addSource registers bytes for a path. The fakeLookup's GetSource
// implementation returns whatever's stored here, so this is how
// tests stage source content for the protolsp/sourceContent handler.
func (f *fakeLookup) addSource(path string, content []byte) {
	f.sources[path] = content
}

// IsStale satisfies the schema.Lookup interface; the fake is always
// "live" unless a specific test toggles staleFlag. Tests covering the
// stale-startup notification path set staleFlag explicitly.
func (f *fakeLookup) IsStale() bool { return f.staleFlag }

// _ ensures fakeLookup keeps satisfying schema.Lookup as the interface
// evolves; the unused assignment fails compilation on drift.
var _ schema.Lookup = (*fakeLookup)(nil)

// buildGreetingMessageType constructs a MessageType for the message
//
//	package acme.demo.v1;
//	message Greeting { string name = 1; }
//
// using protodesc to skip pulling in a full .proto compile path.
func buildGreetingMessageType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("acme/demo/v1/greeting.proto"),
		Package: proto.String("acme.demo.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Greeting"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("name"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("name"),
					},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().Get(0))
}

func newServerWithLookup(t *testing.T) (*Server, *fakeConn, *fakeLookup) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildGreetingMessageType(t))
	srv.Lookups().Swap(lookup, nil)
	return srv, fc, lookup
}

// ---------------------------------------------------------------------------
// Schema-aware diagnostics (M3)
// ---------------------------------------------------------------------------

const greetingValid = `@type acme.demo.v1.Greeting

name = "world"
`

const greetingUnknownField = `@type acme.demo.v1.Greeting

stranger = "world"
`

const greetingTypeMismatch = `@type acme.demo.v1.Greeting

name = 42
`

const greetingNoTypeDirective = `name = "world"
`

func TestSchema_ValidPxfPublishesEmptyDiagnostics(t *testing.T) {
	srv, fc, _ := newServerWithLookup(t)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Empty(t, notes[0].params.Diagnostics, "valid pfx against registered schema → no diagnostics")

	doc := srv.Documents().Get("file:///work/g.pxf")
	require.NotNil(t, doc)
	assert.NoError(t, doc.SchemaErr, "schema validation succeeded")
}

func TestSchema_UnknownFieldEmitsDiagnostic(t *testing.T) {
	srv, fc, _ := newServerWithLookup(t)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingUnknownField)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := notes[0].params.Diagnostics
	require.Len(t, diags, 1)
	d := diags[0]
	assert.Equal(t, "protolsp/schema", optValue(d.Source), "schema diagnostics carry the schema source tag")
	assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity)
	assert.Contains(t, messageText(d.Message), "stranger", "unknown-field diagnostic should name the offending field")

	doc := srv.Documents().Get("file:///work/g.pxf")
	require.NotNil(t, doc)
	// Unknown fields now route through UnknownFields (per-occurrence)
	// rather than the single SchemaErr (which is reserved for
	// type-mismatch / required / oneof issues from UnmarshalDescriptor).
	require.NotEmpty(t, doc.UnknownFields)
	assert.NoError(t, doc.SchemaErr,
		"unknown-field detection moved out of SchemaErr to UnknownFields with DiscardUnknown on the decoder")
}

func TestSchema_TypeMismatchEmitsDiagnostic(t *testing.T) {
	srv, fc, _ := newServerWithLookup(t)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingTypeMismatch)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	require.Len(t, notes[0].params.Diagnostics, 1)
	assert.Equal(t, "protolsp/schema", optValue(notes[0].params.Diagnostics[0].Source))
}

func TestSchema_UnknownTypeEmitsDiagnostic(t *testing.T) {
	srv, fc, lookup := newServerWithLookup(t)
	// Reset the lookup so the registered Greeting is gone — any FQN
	// resolves to NotFound now.
	lookup.types = map[protoreflect.FullName]protoreflect.MessageType{}

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingValid)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	require.Len(t, notes[0].params.Diagnostics, 1)
	d := notes[0].params.Diagnostics[0]
	assert.Equal(t, "protolsp/schema", optValue(d.Source))
	assert.Contains(t, messageText(d.Message), "unknown type", "diagnostic should call out that the @type FQN didn't resolve")
	assert.Contains(t, messageText(d.Message), "acme.demo.v1.Greeting")
}

func TestSchema_NoTypeDirectiveSkipsValidation(t *testing.T) {
	srv, fc, _ := newServerWithLookup(t)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingNoTypeDirective)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Empty(t, notes[0].params.Diagnostics, "without @type the LSP must not invent a schema diagnostic")

	doc := srv.Documents().Get("file:///work/g.pxf")
	require.NotNil(t, doc)
	assert.NoError(t, doc.SchemaErr)
}

func TestSchema_ParseErrorSuppressesSchemaValidation(t *testing.T) {
	// When the source doesn't parse, the validator should be skipped —
	// running pxf.UnmarshalDescriptor on un-parseable bytes would just
	// produce a duplicate or misleading error.
	srv, fc, _ := newServerWithLookup(t)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, invalidPxf)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := notes[0].params.Diagnostics
	require.Len(t, diags, 1)
	assert.Equal(t, "protolsp/parse", optValue(diags[0].Source), "parse failure must surface as parse diagnostic, not schema")
}

func TestSchema_NoLookupConfiguredSkipsValidation(t *testing.T) {
	// Default Server has lookup=nil. valid + invalid-against-schema
	// pfx both pass; we never reach the validator.
	srv, fc := newServerWithFakeConn(t)
	// No s.lookup assignment.

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingUnknownField)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Empty(t, notes[0].params.Diagnostics, "no Lookup → no schema diagnostics, even when the source would fail validation")
}
