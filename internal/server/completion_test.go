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
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// completionFromHandle drives the completion dispatcher and returns
// the deserialized CompletionList. Mirrors what a real client sees.
func completionFromHandle(t *testing.T, srv *Server, u string, line, char uint32) *protocol.CompletionList {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentCompletion,
		&protocol.CompletionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
				Position:     protocol.Position{Line: line, Character: char},
			},
		})
	require.NoError(t, err)

	var captured *protocol.CompletionList
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result == nil {
			return nil
		}
		b, err := json.Marshal(result)
		require.NoError(t, err)
		var list protocol.CompletionList
		require.NoError(t, json.Unmarshal(b, &list))
		captured = &list
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return captured
}

func labelsFromList(list *protocol.CompletionList) []string {
	if list == nil {
		return nil
	}
	out := make([]string, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, it.Label)
	}
	return out
}

// ---------------------------------------------------------------------------
// Field-name completion
// ---------------------------------------------------------------------------

func TestCompletion_TopLevelFieldsOnRoot(t *testing.T) {
	// On a blank line at the top of a Greeting doc, the resolver
	// should return the single field "name" defined on Greeting.
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n\n\n")))

	// Cursor on the empty line 2 (0-indexed), col 0.
	list := completionFromHandle(t, srv, "file:///work/g.pxf", 2, 0)
	require.NotNil(t, list)
	assert.False(t, list.IsIncomplete)
	assert.Equal(t, []string{"name"}, labelsFromList(list))
}

func TestCompletion_ItemShape(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n\n")))

	list := completionFromHandle(t, srv, "file:///work/g.pxf", 1, 0)
	require.NotNil(t, list)
	require.Len(t, list.Items, 1)
	item := list.Items[0]

	assert.Equal(t, "name", item.Label)
	assert.Equal(t, protocol.CompletionItemKindField, item.Kind)
	assert.Equal(t, "string", detailText(item.Detail), "Detail labels the protobuf kind")
	assert.Equal(t, "name = ", detailText(item.InsertText),
		"InsertText prefills the assignment operator so the user types the value next")
}

func TestCompletion_ReturnsNilWhenNoLookup(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "@type acme.demo.v1.Greeting\n")))

	list := completionFromHandle(t, srv, "file:///work/g.pxf", 1, 0)
	assert.Nil(t, list, "no Lookup → null completion list (LSP correct 'no completions')")
}

func TestCompletion_ReturnsNilWhenNoTypeDirective(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, "name = \"x\"\n")))

	list := completionFromHandle(t, srv, "file:///work/g.pxf", 0, 0)
	assert.Nil(t, list, "no @type → no schema bound → null")
}

func TestCompletion_NestedBlockUsesInnerMessageContext(t *testing.T) {
	// Build a parent message with a nested field whose own field set
	// differs from the parent's. Completion inside the nested block
	// should return the nested field's children, not the parent's.
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildOrderMessageType(t))
	srv.Lookups().Swap(lookup, nil)
	_ = fc

	const src = `@type acme.demo.v1.Order

shipping {

}
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/o.pxf", 1, src)))

	// Source lines (0-indexed):
	//   0: @type acme.demo.v1.Order
	//   1: (blank)
	//   2: shipping {
	//   3: (blank — cursor here, inside the block)
	//   4: }
	list := completionFromHandle(t, srv, "file:///work/o.pxf", 3, 0)
	require.NotNil(t, list)
	labels := labelsFromList(list)
	assert.Contains(t, labels, "street", "nested context should expose Address fields")
	assert.Contains(t, labels, "city")
	assert.NotContains(t, labels, "shipping", "Order's own field 'shipping' should not appear inside Address scope")
}

func TestCompletion_AfterClosedBlockReturnsToOuterScope(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildOrderMessageType(t))
	srv.Lookups().Swap(lookup, nil)
	_ = fc

	const src = `@type acme.demo.v1.Order

shipping {
}

`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/o.pxf", 1, src)))

	// Cursor on line 4 (0-indexed) — blank line after the closed block.
	// We're back at top level, so Order fields should be visible.
	list := completionFromHandle(t, srv, "file:///work/o.pxf", 4, 0)
	require.NotNil(t, list)
	labels := labelsFromList(list)
	assert.Contains(t, labels, "shipping", "after the block closes, top-level Order fields are in scope again")
	assert.NotContains(t, labels, "street", "Address fields must not leak past the closing brace")
}

// ---------------------------------------------------------------------------
// Message-type completion (`@type` directive)
// ---------------------------------------------------------------------------

func TestCompletion_TypeDirectiveListsAllKnownMessages(t *testing.T) {
	// Two types registered in the fake lookup; completion on an
	// `@type` line should surface both regardless of which one (if
	// any) the document is currently bound to.
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildGreetingMessageType(t))
	lookup.add(buildOrderMessageType(t)) // adds Order; nested Address too
	srv.Lookups().Swap(lookup, nil)
	_ = fc

	// User typed `@type ` and is at the end of the line. The document
	// likely fails to parse (no value yet) — type completion is
	// supposed to work regardless.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/new.pxf", 1, "@type \n")))

	list := completionFromHandle(t, srv, "file:///work/new.pxf", 0, 6)
	require.NotNil(t, list)
	labels := labelsFromList(list)
	assert.Contains(t, labels, "acme.demo.v1.Greeting")
	assert.Contains(t, labels, "acme.demo.v1.Order")
	// (The real registry client surfaces sibling types in the same
	// file too — e.g. Address from the Order fixture — via the
	// underlying NamespacedTypes registry. The fake only knows
	// about the top-level types we explicitly add() to it, which is
	// enough to prove the completion plumbing works end-to-end.)
}

func TestCompletion_TypeDirectiveItemShape(t *testing.T) {
	// The user's lookup has just Greeting; protolsp's internal
	// lookup also surfaces protolsp.v1.* + chameleon.v1.* on every
	// @type completion. This test pins the SHAPE of the user-
	// registered item rather than the exact count.
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/new.pxf", 1, "@type \n")))

	list := completionFromHandle(t, srv, "file:///work/new.pxf", 0, 6)
	require.NotNil(t, list)
	require.NotEmpty(t, list.Items)
	var greeting *protocol.CompletionItem
	for i := range list.Items {
		if list.Items[i].Label == "acme.demo.v1.Greeting" {
			greeting = &list.Items[i]
			break
		}
	}
	require.NotNil(t, greeting, "user-registered Greeting must appear in the suggestions")
	assert.Equal(t, protocol.CompletionItemKindClass, greeting.Kind)
	assert.Equal(t, "message", detailText(greeting.Detail))
	assert.Equal(t, "acme.demo.v1.Greeting", detailText(greeting.InsertText),
		"InsertText is the bare FQN — no trailing punctuation needed")
}

func TestCompletion_TypeDirectivePartialKeyword(t *testing.T) {
	// Completion is invoked while the user is still typing the
	// directive keyword (`@t`, `@ty`, …). The handler must recognize
	// these as in-progress `@type` and still suggest types.
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/new.pxf", 1, "@t")))

	list := completionFromHandle(t, srv, "file:///work/new.pxf", 0, 2)
	require.NotNil(t, list, "partial `@t` should trigger type completion")
	assert.NotEmpty(t, list.Items)
}

func TestCompletion_OtherAtDirectiveNotTreatedAsType(t *testing.T) {
	// `@dataset`, `@proto`, etc. are separate directive families;
	// completion must NOT offer message FQNs there.
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/new.pxf", 1, "@dataset \n")))

	list := completionFromHandle(t, srv, "file:///work/new.pxf", 0, 9)
	// No `@type` context AND no parse-success-with-bound-type either,
	// so we fall through to the field-name path which also returns nil.
	assert.Nil(t, list, "@dataset must not trigger type completion")
}

func TestCompletion_NotTypeDirectiveWhenMidEditOfDifferentWord(t *testing.T) {
	// A line like `@typewriter` looks like `@type` for the first five
	// chars but isn't the directive. Past `@type` we require a
	// whitespace separator before treating it as the directive.
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/new.pxf", 1, "@typewriter\n")))

	list := completionFromHandle(t, srv, "file:///work/new.pxf", 0, 11)
	assert.Nil(t, list, "@typewriter is not @type — must not list message types")
}

// ---------------------------------------------------------------------------
// Enum-value completion
// ---------------------------------------------------------------------------

// buildTicketMessageType constructs a fixture with an enum field:
//
//	enum Priority {
//	  PRIORITY_UNKNOWN = 0;
//	  PRIORITY_LOW     = 1;
//	  PRIORITY_HIGH    = 2;
//	}
//	message Ticket {
//	  string title = 1;
//	  Priority priority = 2;
//	}
func buildTicketMessageType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("acme/demo/v1/ticket.proto"),
		Package: proto.String("acme.demo.v1"),
		Syntax:  proto.String("proto3"),
		EnumType: []*descriptorpb.EnumDescriptorProto{
			{
				Name: proto.String("Priority"),
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: proto.String("PRIORITY_UNKNOWN"), Number: proto.Int32(0)},
					{Name: proto.String("PRIORITY_LOW"), Number: proto.Int32(1)},
					{Name: proto.String("PRIORITY_HIGH"), Number: proto.Int32(2)},
				},
			},
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Ticket"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("title"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("title")},
					{Name: proto.String("priority"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(), TypeName: proto.String(".acme.demo.v1.Priority"), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("priority")},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().Get(0))
}

func newServerWithTicket(t *testing.T) *Server {
	t.Helper()
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTicketMessageType(t))
	srv.Lookups().Swap(lookup, nil)
	return srv
}

func TestCompletion_EnumValuesOnRHS(t *testing.T) {
	// Cursor lands on the RHS of `priority = ` — should list every
	// Priority enum value. The source above the cursor parses
	// cleanly (only the trailing `=` makes the document invalid),
	// so LastGoodParse handles the mid-edit moment.
	srv := newServerWithTicket(t)

	const src = `@type acme.demo.v1.Ticket

title = "x"
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	// Then user types "priority = " on a new line. didChange with the
	// updated source (which won't parse because of trailing `=`).
	const editedSrc = `@type acme.demo.v1.Ticket

title = "x"
priority = `
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/t.pxf", 2, editedSrc)))

	// Cursor at end of line 3 (the `priority = ` line, 0-indexed),
	// just past the `=`+space.
	list := completionFromHandle(t, srv, "file:///work/t.pxf", 3, uint32(len("priority = ")))
	require.NotNil(t, list, "RHS of enum-typed field must surface enum values")
	labels := labelsFromList(list)
	assert.Equal(t, []string{"PRIORITY_UNKNOWN", "PRIORITY_LOW", "PRIORITY_HIGH"}, labels,
		"enum values should appear in source declaration order")
}

func TestCompletion_EnumItemShape(t *testing.T) {
	srv := newServerWithTicket(t)
	// Open with a clean parseable document so the AST is available
	// when we then didChange to a mid-edit invalid state.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, "@type acme.demo.v1.Ticket\n\ntitle = \"x\"\n")))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/t.pxf", 2, "@type acme.demo.v1.Ticket\n\ntitle = \"x\"\npriority = ")))

	list := completionFromHandle(t, srv, "file:///work/t.pxf", 3, uint32(len("priority = ")))
	require.NotNil(t, list)
	require.NotEmpty(t, list.Items)
	item := list.Items[0]
	assert.Equal(t, "PRIORITY_UNKNOWN", item.Label)
	assert.Equal(t, protocol.CompletionItemKindEnumMember, item.Kind)
	assert.Equal(t, "acme.demo.v1.Priority", detailText(item.Detail), "Detail names the enum type")
	assert.Equal(t, "PRIORITY_UNKNOWN", detailText(item.InsertText))
}

func TestCompletion_NonEnumFieldRHSReturnsNil(t *testing.T) {
	// `title` is a string field. Completion on its RHS should NOT
	// fall through to field-name completion (the cursor is at a
	// value position, not a name position).
	srv := newServerWithTicket(t)
	const src = `@type acme.demo.v1.Ticket

title = `
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	list := completionFromHandle(t, srv, "file:///work/t.pxf", 2, uint32(len("title = ")))
	assert.Nil(t, list, "non-enum field RHS must not produce completions")
}

func TestCompletion_UnknownFieldOnRHSReturnsNil(t *testing.T) {
	// `nope` doesn't exist on Ticket. The RHS branch shouldn't
	// silently fall through to field-name completion — the user is
	// clearly trying to type a value.
	srv := newServerWithTicket(t)
	const src = `@type acme.demo.v1.Ticket

nope = `
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	list := completionFromHandle(t, srv, "file:///work/t.pxf", 2, uint32(len("nope = ")))
	assert.Nil(t, list)
}

func TestCompletion_LastGoodParseFallback(t *testing.T) {
	// Open with valid source so LastGoodParse populates. Then
	// didChange with an unparseable trailing `priority = ` and
	// confirm completion still works (the LastGoodParse's AST gives
	// us @type and the surrounding structure, even though the
	// current Parsed is nil).
	srv := newServerWithTicket(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, "@type acme.demo.v1.Ticket\n\ntitle = \"x\"\n")))

	doc := srv.Documents().Get("file:///work/t.pxf")
	require.NotNil(t, doc)
	require.NotNil(t, doc.Parsed, "initial open should produce a clean parse")
	require.NotNil(t, doc.LastGoodParse)

	// Now the edited version that won't parse.
	const edited = "@type acme.demo.v1.Ticket\n\ntitle = \"x\"\npriority = "
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/t.pxf", 2, edited)))

	doc = srv.Documents().Get("file:///work/t.pxf")
	require.NotNil(t, doc)
	assert.Nil(t, doc.Parsed, "edited source must fail to parse (regression: if this becomes non-nil, the test is no longer testing the fallback)")
	require.NotNil(t, doc.LastGoodParse, "LastGoodParse must persist through the failed parse")

	list := completionFromHandle(t, srv, "file:///work/t.pxf", 3, uint32(len("priority = ")))
	require.NotNil(t, list, "fallback to LastGoodParse should yield enum values")
	assert.Contains(t, labelsFromList(list), "PRIORITY_HIGH")
}

func TestInitialize_AdvertisesCompletionProvider(t *testing.T) {
	srv := New(discardLogger())
	rep := &captureReplier{}

	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize,
		&protocol.InitializeParams{ClientInfo: protocol.ClientInfo{Name: "test"}})
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), rep.reply, call))

	result := rep.result.(*initializeResult)
	require.NotNil(t, result.Capabilities.CompletionProvider,
		"server must advertise CompletionProvider in initialize result")
}

// ---------------------------------------------------------------------------
// fixture: Order { string id; Address shipping; } / Address { street, city, zip }
// ---------------------------------------------------------------------------

// buildOrderWithAddressesType is buildOrderMessageType plus a
// `repeated Address addresses` field so hover/go-to-def tests can
// exercise array-element resolution (`addresses = [ { street = …
// } ]`). Kept separate from buildOrderMessageType so the
// long-standing completion + nested-block tests aren't affected by
// the extra field.
func buildOrderWithAddressesType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("acme/demo/v1/order.proto"),
		Package: proto.String("acme.demo.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Address"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("street"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("street")},
					{Name: proto.String("city"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("city")},
				},
			},
			{
				Name: proto.String("Order"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("id"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("id")},
					{
						Name: proto.String("addresses"), Number: proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum(),
						TypeName: proto.String(".acme.demo.v1.Address"),
						JsonName: proto.String("addresses"),
					},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().ByName("Order"))
}

func buildOrderMessageType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("acme/demo/v1/order.proto"),
		Package: proto.String("acme.demo.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Address"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("street"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("street")},
					{Name: proto.String("city"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("city")},
					{Name: proto.String("zip"), Number: proto.Int32(3), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("zip")},
				},
			},
			{
				Name: proto.String("Order"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("id"), Number: proto.Int32(1), Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), JsonName: proto.String("id")},
					{Name: proto.String("shipping"), Number: proto.Int32(2), Type: descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(), TypeName: proto.String(".acme.demo.v1.Address"), JsonName: proto.String("shipping")},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	// Order is the second message in MessageType.
	return dynamicpb.NewMessageType(fd.Messages().ByName("Order"))
}
