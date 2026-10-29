// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protowire-go/encoding/pxf"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The compiled schema is reused while the @proto directives and their
// bindings stay as they are, however the rest of the document moves,
// and compiled again when one of them changes.
func TestLoadInlineSchema_Reuse(t *testing.T) {
	doc := &Document{}
	load := func(src string) {
		t.Helper()
		parsed, err := pxf.Parse([]byte(src))
		require.NoError(t, err)
		doc.Source, doc.Parsed = []byte(src), parsed
		loadInlineSchema(doc)
	}
	const header = "@proto t.v1.T { string x = 1; }\n@proto { string a = 1; }\n"
	load(header + "@dataset (a)\n(\"1\")\n")
	mt := doc.inline.findMessage("t.v1.T")
	require.NotNil(t, mt)
	first := mt.Descriptor()

	load("# moved\n" + header + "@dataset t.v1.T (x)\n(\"0\")\n@dataset (a)\n(\"1\")\n(\"2\")\n")
	assert.True(t, first == doc.inline.findMessage("t.v1.T").Descriptor(), "reused")
	require.Len(t, doc.Parsed.Datasets, 2)
	assert.Equal(t, anonPrefix+"0", doc.datasetType(&doc.Parsed.Datasets[1]), "bound against this parse")

	load("@proto t.v1.T { string x = 1; int32 y = 2; }\n@proto { string a = 1; }\n@dataset (a)\n(\"1\")\n")
	assert.False(t, first == doc.inline.findMessage("t.v1.T").Descriptor(), "compiled again")
}

// A @proto that does not compile contributes no type, nor does one
// that imports it; the others' types stay.
func TestCompileInlineSchema_KeepsWhatCompiled(t *testing.T) {
	src := "@proto t.v1.Bad { strin x = 1; }\n@proto t.v1.Good { string x = 1; }\n" +
		"@proto \"\"\"\nsyntax = \"proto3\";\npackage u.v1;\nimport \"__protolsp_inline_0.proto\";\nmessage Uses { t.v1.Bad b = 1; }\n\"\"\"\n"
	parsed, err := pxf.Parse([]byte(src))
	require.NoError(t, err)
	doc := &Document{Source: []byte(src), Parsed: parsed}
	loadInlineSchema(doc)
	assert.NotEmpty(t, doc.inline.diags)
	assert.NotNil(t, doc.inline.findMessage("t.v1.Good"))
	assert.Nil(t, doc.inline.findMessage("t.v1.Bad"))
	assert.Nil(t, doc.inline.findMessage("u.v1.Uses"))
}

// A source-form @proto imports only what the draft lists (protowire#369,
// protowire#388). testdata/proto-imports is protowire's cross-port
// fixture set (testdata/proto-imports at v1.13.0), each file stating its
// verdict in its leading comment: an accepted import compiles and
// defines the @type, a rejected one is reported as the import refused.
func TestCompileInlineSchema_Imports(t *testing.T) {
	paths, err := filepath.Glob("testdata/proto-imports/*.pxf")
	require.NoError(t, err)
	require.Len(t, paths, 9)
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			require.NoError(t, err)
			parsed, err := pxf.Parse(src)
			require.NoError(t, err)
			doc := &Document{Source: src, Parsed: parsed}
			loadInlineSchema(doc)
			var msgs []string
			for _, d := range doc.inline.diags {
				msgs = append(msgs, d.Message)
			}
			switch {
			case bytes.HasPrefix(src, []byte("# accept:")):
				assert.Empty(t, msgs)
				assert.NotNil(t, doc.inline.findMessage(protoreflect.FullName(parsed.TypeURL)), "defines %s", parsed.TypeURL)
			case bytes.HasPrefix(src, []byte("# reject:")):
				require.NotEmpty(t, msgs)
				assert.Contains(t, msgs[0], "@proto does not compile")
				assert.Contains(t, msgs[0], "a @proto may import only another @proto in the document")
			default:
				t.Fatal("no verdict in the leading comment")
			}
		})
	}
}
