// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protocompile/ast"
)

// TestFormatProto_RefusesWhenPrinterLosesText checks the guard is
// wired in: with a printer that drops a comment, formatProto returns
// an error naming the loss instead of the shortened text.
func TestFormatProto_RefusesWhenPrinterLosesText(t *testing.T) {
	real := printProto
	t.Cleanup(func() { printProto = real })
	printProto = func(f *ast.File) (string, error) {
		out, err := real(f)
		return strings.Replace(out, " // note", "", 1), err
	}

	_, err := formatProto("foo.proto", []byte("syntax = \"proto3\";\n\nmessage Foo {\n  string a = 1; // note\n}\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `lost ["comment:note"]`)
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
