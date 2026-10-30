// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// openInline opens src with no workspace registry: a document that
// carries its own @proto needs none. It returns the document and
// the diagnostics last published for it.
func openInline(t *testing.T, src string) (*Document, []protocol.Diagnostic) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/inline.pxf", 1, src)))
	doc := srv.Documents().Get("file:///work/inline.pxf")
	require.NotNil(t, doc)
	require.NoError(t, doc.ParseErr)
	notes := fc.calls()
	require.NotEmpty(t, notes)
	return doc, notes[len(notes)-1].params.Diagnostics
}

// messages returns the messages of diags from source, in order.
func messages(diags []protocol.Diagnostic, source string) []string {
	var out []string
	for _, d := range diags {
		if optValue(d.Source) == source {
			out = append(out, messageText(d.Message))
		}
	}
	return out
}

const loginSource = `@proto """
syntax = "proto3";
package l.v1;
import "pxf/secret.proto";
message Login {
  string user = 1;
  pxf.Secret pw = 2;
  int32 n = 3;
}
"""
`

// A body whose @type names a source-form @proto type is checked against
// it: an unknown field, a type mismatch, and a Secret to mask.
func TestInlineProto_SourceFormBody(t *testing.T) {
	doc, diags := openInline(t, loginSource+`@type l.v1.Login
user = "a"
pw = "s3cret"
bogus = 1
`)
	assert.Equal(t, []string{`unknown field "bogus" on this message`}, messages(diags, "protolsp/schema"))
	assert.Equal(t, []string{"s3cret"}, doc.SecretValues())

	_, diags = openInline(t, loginSource+"@type l.v1.Login\nn = \"x\"\n")
	assert.Equal(t, []string{`expected integer for field "n"`}, messages(diags, "protolsp/schema"))
	assert.Empty(t, messages(diags, "protolsp/proto"))
}

// A @dataset of a source-form type has its cells checked and its
// Secrets masked.
func TestInlineProto_SourceFormDataset(t *testing.T) {
	doc, diags := openInline(t, loginSource+`@dataset l.v1.Login (user, pw, n)
("a", "s3cret", "x")
`)
	assert.Equal(t, []string{"s3cret"}, doc.SecretValues())
	var cell []string
	for _, m := range messages(diags, "protolsp/dataset") {
		if strings.Contains(m, `column "n"`) {
			cell = append(cell, m)
		}
	}
	assert.Len(t, cell, 1, "the int32 column's string cell is reported: %v", diags)
	require.Len(t, doc.DatasetRegions, 1)
	assert.True(t, doc.DatasetRegions[0].Resolvable)
}

// The named and anonymous forms bind their datasets. The anonymous
// binding keeps the region's TypeFQN empty: its synthetic name is
// protolsp's own.
func TestInlineProto_NamedAndAnonymousDatasets(t *testing.T) {
	for _, tc := range []struct{ name, src, fqn string }{
		{"named", "@proto t.v1.Trade { string sym = 1; double px = 2; }\n@dataset t.v1.Trade (sym, px)\n(\"A\", \"x\")\n", "t.v1.Trade"},
		{"anonymous", "@proto { string sym = 1; double px = 2; }\n@dataset (sym, px)\n(\"A\", \"x\")\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, diags := openInline(t, tc.src)
			assert.Empty(t, messages(diags, "protolsp/proto"))
			require.Len(t, doc.DatasetRegions, 1)
			assert.True(t, doc.DatasetRegions[0].Resolvable)
			assert.Equal(t, tc.fqn, doc.DatasetRegions[0].TypeFQN)
			found := false
			for _, m := range messages(diags, "protolsp/dataset") {
				found = found || strings.Contains(m, `column "px"`)
			}
			assert.True(t, found, "the double column's string cell is reported: %v", diags)
		})
	}
}

// A message about an anonymous @proto's row type does not show the
// synthetic name protolsp gave it.
func TestInlineProto_AnonymousNameNotShown(t *testing.T) {
	_, diags := openInline(t, "@proto { string a = 1; }\n@dataset (nope)\n(\"x\")\n")
	assert.Equal(t, []string{`unknown column "nope" on the anonymous @proto row type`}, messages(diags, "protolsp/dataset"))

	// Nor in a column path into a type nested in it.
	_, diags = openInline(t, "@proto { message In { string x = 1; } In in = 1; }\n@dataset (in.nope)\n(\"x\")\n")
	assert.Equal(t, []string{`column "in.nope": In has no field "nope"`}, messages(diags, "protolsp/dataset"))
	// Nor in the decoder's verdict on what a block holds.
	_, diags = openInline(t, "@proto { message In { string x = 1; } In in = 1; }\n@dataset (in)\n({ nope = 1 })\n")
	assert.Equal(t, []string{`column "in": unknown field "nope" in In`}, messages(diags, "protolsp/dataset"))
}

// Two anonymous @proto directives bind the typeless datasets after them
// in document order, one each.
func TestInlineProto_AnonymousBindsInOrder(t *testing.T) {
	doc, diags := openInline(t, `@proto { string a = 1; }
@proto { int32 b = 1; }
@dataset (a)
("ok")
@dataset (b)
("x")
`)
	assert.Empty(t, messages(diags, "protolsp/proto"))
	// Bound in order, only the second table's string cell in an int32
	// column is wrong. Swapped, each table would name an unknown column.
	assert.Equal(t, []string{`column "b" expects int32; got string`}, messages(diags, "protolsp/dataset"))
	require.Len(t, doc.DatasetRegions, 2)
	assert.True(t, doc.DatasetRegions[0].Resolvable)
	assert.True(t, doc.DatasetRegions[1].Resolvable)
}

// The embedded schema is authoritative over the workspace registry
// (draft § The "@proto" Directive): a registry type of the same name
// with other fields does not decide.
func TestInlineProto_EmbeddedSchemaWins(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t)) // acme.demo.v1.Credentials: username, password
	srv.Lookups().Swap(lookup, nil)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, didOpenCall(t, "file:///work/inline.pxf", 1,
		"@proto acme.demo.v1.Credentials { string login = 1; }\n@type acme.demo.v1.Credentials\nlogin = \"a\"\n")))
	notes := fc.calls()
	require.NotEmpty(t, notes)
	assert.Empty(t, notes[len(notes)-1].params.Diagnostics)
}

// What fails is reported at its @proto, source "protolsp/proto": a body
// that does not compile, an import the draft does not permit, two
// diverging definitions of one name, and an orphan on either side of
// the anonymous binding. Identical definitions are one type.
func TestInlineProto_Errors(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"bad body", "@proto t.v1.T { strin x = 1; }\n@type t.v1.T\n", "@proto does not compile"},
		{"import from disk", "@proto \"\"\"\nsyntax = \"proto3\";\nimport \"acme/other.proto\";\nmessage T {}\n\"\"\"\n@type T\n", "@proto does not compile"},
		{"diverging duplicate", "@proto t.v1.T { string x = 1; }\n@proto t.v1.T { int32 x = 1; }\n@type t.v1.T\n", "@proto does not compile"},
		{"anonymous orphan", "@proto { string x = 1; }\n", "anonymous @proto has no typeless @dataset after it to bind to"},
		{"typeless dataset orphan", "@proto t.v1.T { string x = 1; }\n@dataset (x)\n(\"a\")\n", "@dataset has no type and no anonymous @proto before it to bind to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := openInline(t, tc.src)
			got := messages(diags, "protolsp/proto")
			require.Len(t, got, 1, "diagnostics: %v", diags)
			assert.Contains(t, got[0], tc.want)
			assert.NotContains(t, got[0], "__protolsp_inline_", "the virtual file name is not shown")
		})
	}
	_, diags := openInline(t, "@proto t.v1.T { string x = 1; }\n@proto t.v1.T { string x = 1; }\n@type t.v1.T\nx = \"a\"\n")
	assert.Empty(t, diags, "identical definitions are one type")
}

// A compile error lands where its text is in the document, not at the
// directive: inside a one-line named body, a multi-line one, and a
// source form.
func TestInlineProto_ErrorPositions(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		at        protocol.Position
	}{
		{"one-line body", "@proto t.v1.T { strin x = 1; }\n@type t.v1.T\n", protocol.Position{Line: 0, Character: 16}},
		{"multi-line body", "@proto t.v1.T {\n  string a = 1;\n  strin x = 2;\n}\n@type t.v1.T\n", protocol.Position{Line: 2, Character: 2}},
		{"source form", "@proto \"\"\"\nsyntax = \"proto3\";\nimport \"acme/other.proto\";\nmessage T {}\n\"\"\"\n@type T\n", protocol.Position{Line: 2, Character: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := openInline(t, tc.src)
			var got []protocol.Diagnostic
			for _, d := range diags {
				if optValue(d.Source) == "protolsp/proto" {
					got = append(got, d)
				}
			}
			require.Len(t, got, 1, "diagnostics: %v", diags)
			assert.Equal(t, tc.at, got[0].Range.Start, messageText(got[0].Message))
		})
	}
}

// The descriptor form is a FileDescriptorSet.
func TestInlineProto_DescriptorForm(t *testing.T) {
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("t.proto"),
		Package: proto.String("t.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Trade"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("px"), Number: proto.Int32(1), JsonName: proto.String("px"),
				Type:  descriptorpb.FieldDescriptorProto_TYPE_DOUBLE.Enum(),
				Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			}},
		}},
	}
	_, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	raw, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{fdp}})
	require.NoError(t, err)
	doc, diags := openInline(t, "@proto b\""+base64.StdEncoding.EncodeToString(raw)+"\"\n@dataset t.v1.Trade (px)\n(\"x\")\n")
	assert.Empty(t, messages(diags, "protolsp/proto"))
	require.Len(t, doc.DatasetRegions, 1)
	assert.True(t, doc.DatasetRegions[0].Resolvable)
	found := false
	for _, m := range messages(diags, "protolsp/dataset") {
		found = found || strings.Contains(m, `column "px"`)
	}
	assert.True(t, found, "the double column's string cell is reported: %v", diags)
}

// changeInline opens src, then replaces the whole document with next,
// and returns the document and the diagnostics published for next.
func changeInline(t *testing.T, src, next string) (*Document, []protocol.Diagnostic) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/inline.pxf", 1, src)))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, "file:///work/inline.pxf", 2, next)))
	doc := srv.Documents().Get("file:///work/inline.pxf")
	require.NotNil(t, doc)
	require.NoError(t, doc.ParseErr)
	notes := fc.calls()
	require.NotEmpty(t, notes)
	return doc, notes[len(notes)-1].params.Diagnostics
}

// The hover on a @dataset of an in-document type resolves it, and
// names neither the anonymous binding's synthetic type nor the
// virtual file a @proto is compiled from.
func TestInlineProto_DatasetHover(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"anonymous", "@proto { string sym = 1; }\n@dataset (sym)\n(\"A\")\n", "anonymous `@proto` row type"},
		{"named", "@proto t.v1.Trade { string sym = 1; }\n@dataset t.v1.Trade (sym)\n(\"A\")\n", "t.v1.Trade"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newServerWithFakeConn(t)
			require.NoError(t, testHandle(srv, context.Background(), noopReply,
				didOpenCall(t, "file:///work/inline.pxf", 1, tc.src)))
			hover := srv.HoverFor("file:///work/inline.pxf", protocol.Position{Line: 1, Character: 3})
			require.NotNil(t, hover)
			md := hoverBody(hover)
			assert.Contains(t, md, tc.want)
			assert.Contains(t, md, "this document's `@proto`")
			assert.NotContains(t, md, "not known")
			assert.NotContains(t, md, "__protolsp_")
			assert.NotContains(t, md, "``", "no empty type name")
		})
	}
}

// A typeless @dataset whose binding failed is reported once, by
// protolsp/proto: the row-type check does not add an "unknown row
// type" naming no type, with or without a registry. Without a
// registry, a type the document does not define is not reported
// either, as before.
func TestInlineProto_NoSecondUnknownRowType(t *testing.T) {
	for _, src := range []string{
		"@proto t.v1.T { string x = 1; }\n@dataset (x)\n(\"a\")\n",
		"@dataset (x)\n(\"a\")\n",
		"@proto { strin x = 1; }\n@dataset (x)\n(\"a\")\n",
		"@proto t.v1.T { string x = 1; }\n@dataset other.v1.Row (x)\n(\"a\")\n",
	} {
		_, diags := openInline(t, src)
		assert.Empty(t, messages(diags, "protolsp/dataset"), "%q: %v", src, diags)
	}

	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, didOpenCall(t, "file:///work/inline.pxf", 1,
		"@proto { strin x = 1; }\n@dataset (x)\n(\"a\")\n")))
	require.NoError(t, testHandle(srv, context.Background(), noopReply, didOpenCall(t, "file:///work/plain.pxf", 1,
		"@dataset (x)\n(\"a\")\n")))
	notes := fc.calls()
	require.Len(t, notes, 2)
	for _, n := range notes {
		diags := n.params.Diagnostics
		assert.Empty(t, messages(diags, "protolsp/dataset"), "%v", diags)
		assert.Len(t, messages(diags, "protolsp/proto"), 1, "the failure is still reported, once: %v", diags)
	}
}

// The draft lists a canonical import by one spelling, "pxf/secret.proto":
// "protowire/X" is refused like any import outside the list
// (protowire#369), and reported where the editor shows it.
func TestInlineProto_ProtowireImportSpellingRefused(t *testing.T) {
	_, diags := openInline(t, `@proto """
syntax = "proto3";
package l.v1;
import "protowire/pxf/secret.proto";
message Login { pxf.Secret pw = 1; }
"""
@type l.v1.Login
pw = "s3cret"
`)
	got := messages(diags, "protolsp/proto")
	require.NotEmpty(t, got)
	assert.Contains(t, got[0], `could not open imported file "protowire/pxf/secret.proto"`)
	assert.Contains(t, got[0], "a @proto may import only another @proto in the document")
}

// An indented triple-quoted body is dedented by the parser; a compile
// error in it still lands on its own line and column.
func TestInlineProto_IndentedSourceErrorPosition(t *testing.T) {
	_, diags := openInline(t, "@proto \"\"\"\n  syntax = \"proto3\";\n  import \"acme/other.proto\";\n  message T {}\n  \"\"\"\n@type T\n")
	var got []protocol.Diagnostic
	for _, d := range diags {
		if optValue(d.Source) == "protolsp/proto" {
			got = append(got, d)
		}
	}
	require.Len(t, got, 1, "diagnostics: %v", diags)
	assert.Equal(t, protocol.Position{Line: 2, Character: 2}, got[0].Range.Start, messageText(got[0].Message))
}

// One @proto that does not compile does not take the others' types
// with it: a Secret the good one declares is still masked.
func TestInlineProto_OneFailureKeepsTheOthers(t *testing.T) {
	doc, diags := openInline(t, loginSource+"@proto t.v1.T { strin x = 1; }\n@type l.v1.Login\npw = \"s3cret\"\n")
	assert.Len(t, messages(diags, "protolsp/proto"), 1, "%v", diags)
	assert.Equal(t, []string{"s3cret"}, doc.SecretValues())
}

// A re-analysis that reuses the compiled schema still reports its
// errors on the lines they are on now, and still binds the anonymous
// @proto to its dataset when a typed dataset before it went away.
func TestInlineProto_ReuseAfterEdit(t *testing.T) {
	// A space above the @proto becomes a newline: no offset moves.
	_, diags := changeInline(t,
		"# a #b\n@proto t.v1.T { strin x = 1; }\n@type t.v1.T\n",
		"# a\n#b\n@proto t.v1.T { strin x = 1; }\n@type t.v1.T\n")
	require.Len(t, messages(diags, "protolsp/proto"), 1, "%v", diags)
	for _, d := range diags {
		if optValue(d.Source) == "protolsp/proto" {
			assert.Equal(t, protocol.Position{Line: 2, Character: 16}, d.Range.Start)
		}
	}

	// A typed dataset before the binding becomes a comment of the same
	// length: the typeless dataset keeps its offset but not its index.
	typed := "@dataset x.Y (a)\n(\"1\")\n"
	doc, diags := changeInline(t,
		typed+"@proto { string a = 1; }\n@dataset (a)\n(\"ok\")\n",
		"#"+strings.Repeat(" ", len(typed)-2)+"\n@proto { string a = 1; }\n@dataset (a)\n(\"ok\")\n")
	assert.Empty(t, messages(diags, "protolsp/proto"), "%v", diags)
	require.Len(t, doc.DatasetRegions, 1)
	assert.True(t, doc.DatasetRegions[0].Resolvable)
}

// A descriptor set written with its imports included, as
// `protoc --include_imports` and `buf build` write one, carries the
// well-known files it uses beside its own.
func TestInlineProto_DescriptorFormWithImports(t *testing.T) {
	fdp := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("t.proto"),
		Package:    proto.String("t.v1"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"google/protobuf/timestamp.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Trade"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("at"), Number: proto.Int32(1), JsonName: proto.String("at"),
				Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
				TypeName: proto.String(".google.protobuf.Timestamp"),
				Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			}},
		}},
	}
	ts := protodesc.ToFileDescriptorProto(timestamppb.File_google_protobuf_timestamp_proto)
	raw, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{ts, fdp}})
	require.NoError(t, err)
	doc, diags := openInline(t, "@proto b\""+base64.StdEncoding.EncodeToString(raw)+"\"\n@dataset t.v1.Trade (at)\n(2026-01-02T03:04:05Z)\n")
	assert.Empty(t, messages(diags, "protolsp/proto"))
	assert.Empty(t, messages(diags, "protolsp/dataset"))
	require.Len(t, doc.DatasetRegions, 1)
	assert.True(t, doc.DatasetRegions[0].Resolvable)
}
