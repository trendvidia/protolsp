// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFormatProto_RefusesWhenPrinterDropsAComment pins why the
// server returns no edits for this shape: the content guard, not a
// parse error. protocompile's printer drops the comment after `};`.
func TestFormatProto_RefusesWhenPrinterDropsAComment(t *testing.T) {
	src := []byte("syntax = \"proto3\";\n\nmessage Foo {\n  oneof choice {\n    string a = 1;\n  }; // kept-or-refused\n}\n")
	_, err := formatProto("foo.proto", src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `lost ["comment:kept-or-refused"]`)
}

// TestContentOf_SeesADroppedAnnotationUse checks the guard against
// the loss that blocked #9 (protocompile#258): an annotation use
// missing from the output is reported, while the layout changes the
// Legacy preset makes are not.
func TestContentOf_SeesADroppedAnnotationUse(t *testing.T) {
	in, err := parseProtoStrict("a.proto", "syntax = \"proto3\";\nmessage M {\n  M m = 1 @required; // note\n}\n")
	require.NoError(t, err)
	dropped, err := parseProtoStrict("b.proto", "syntax = \"proto3\";\nmessage M {\n  M m = 1; // note\n}\n")
	require.NoError(t, err)
	lost, added := diffContent(contentOf(in.Stream()), contentOf(dropped.Stream()))
	assert.Equal(t, []string{"required"}, lost)
	assert.Empty(t, added)

	relaid, err := parseProtoStrict("c.proto", "syntax=\"proto3\";message M{M m=1 @required;/* note */};")
	require.NoError(t, err)
	lost, added = diffContent(contentOf(in.Stream()), contentOf(relaid.Stream()))
	assert.Empty(t, lost, "whitespace, `;` and comment markers are layout")
	assert.Empty(t, added)
}
