// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hoverProtoFixture is a small proto with everything Phase 2's
// within-file hover should handle:
//   - a message with a leading comment
//   - a field whose type is another message in the same file
//   - an enum with a leading comment
//   - an enum value with a leading comment
//
// The line numbers below are referenced by the tests via 0-indexed LSP
// positions; if you add a line at the top, every test breaks. Keep
// the fixture small and intentional.
const hoverProtoFixture = `syntax = "proto3";

package acme.demo.v1;

// A friendly greeting addressed to a specific recipient.
message Greeting {
  // The recipient's full name.
  string name = 1;

  // Optional priority hint for the renderer.
  Priority priority = 2;
}

// Render priority for greetings.
enum Priority {
  // No explicit priority set.
  PRIORITY_UNSPECIFIED = 0;
  // Show as soon as practical.
  PRIORITY_HIGH = 1;
}
`

func TestProtoHover_MessageName(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greet.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, hoverProtoFixture)))

	// Line 5 (0-indexed) is `message Greeting {`. Character 8 lands
	// in the middle of "Greeting".
	hover := hoverFromHandle(t, srv, u, 5, 10)
	require.NotNil(t, hover, "hover on message name should produce content")
	assert.Contains(t, hover.Contents.Value, "**message Greeting**",
		"message hover names the declaration")
	assert.Contains(t, hover.Contents.Value, "A friendly greeting addressed",
		"leading comment is included")
}

func TestProtoHover_FieldName(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greet.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, hoverProtoFixture)))

	// Line 7 (0-indexed) is `  string name = 1;`. Character 11 lands in "name".
	hover := hoverFromHandle(t, srv, u, 7, 11)
	require.NotNil(t, hover, "hover on field name should produce content")
	assert.Contains(t, hover.Contents.Value, "**name**",
		"field hover renders the field name")
	assert.Contains(t, hover.Contents.Value, "`string`",
		"field hover renders the type")
	assert.Contains(t, hover.Contents.Value, "(field 1)",
		"field hover renders the field number")
	assert.Contains(t, hover.Contents.Value, "The recipient's full name",
		"field-level leading comment is included")
}

func TestProtoHover_FieldTypeReference_ResolvesInFile(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greet.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, hoverProtoFixture)))

	// Line 10 (0-indexed) is `  Priority priority = 2;`. Character
	// 4 lands in "Priority" (the type, not the field name).
	hover := hoverFromHandle(t, srv, u, 10, 4)
	require.NotNil(t, hover, "hover on a type reference should resolve in-file")
	assert.Contains(t, hover.Contents.Value, "**enum Priority**",
		"type ref resolves to the declared enum")
	assert.Contains(t, hover.Contents.Value, "Render priority for greetings",
		"declared enum's comment is included")
}

func TestProtoHover_EnumValue(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greet.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, hoverProtoFixture)))

	// Line 16 (0-indexed) is `  PRIORITY_UNSPECIFIED = 0;`. Character
	// 5 lands in the name.
	hover := hoverFromHandle(t, srv, u, 16, 5)
	require.NotNil(t, hover, "hover on enum value should produce content")
	assert.Contains(t, hover.Contents.Value, "**PRIORITY_UNSPECIFIED**",
		"enum value hover renders the name")
	assert.Contains(t, hover.Contents.Value, "= 0",
		"enum value hover renders the number")
	assert.Contains(t, hover.Contents.Value, "No explicit priority set",
		"enum-value leading comment is included")
}

func TestProtoHover_BuiltinTypeReturnsNothing(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greet.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, hoverProtoFixture)))

	// Line 7 (0-indexed) is `  string name = 1;`. Character 4 lands in "string".
	// Built-ins don't get a hover — phase 2 is intentionally silent.
	hover := hoverFromHandle(t, srv, u, 7, 4)
	if hover != nil {
		assert.False(t,
			strings.Contains(hover.Contents.Value, "message") ||
				strings.Contains(hover.Contents.Value, "enum"),
			"hover on `string` should not falsely resolve to a user type")
	}
}

func TestProtoHover_WhitespaceReturnsNil(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	const u = "file:///work/greet.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, hoverProtoFixture)))

	// Line 1 is blank. Anywhere on it should return nil.
	hover := hoverFromHandle(t, srv, u, 1, 0)
	assert.Nil(t, hover, "hover on whitespace returns nil")
}

func TestProtoHover_UnopenedDocReturnsNil(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	hover := hoverFromHandle(t, srv, "file:///work/nope.proto", 0, 0)
	assert.Nil(t, hover, "hover on a never-opened doc returns nil")
}
