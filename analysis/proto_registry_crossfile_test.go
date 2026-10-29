// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/schema"
)

// greeting.proto defines the cross-file target. Line map (0-indexed):
// 0 syntax, 1 package, 2 blank, 3 `message Greeting {`, 6 `}`,
// 7 blank, 8 `enum Tone {`.
const xfGreetingProto = `syntax = "proto3";
package myapp.v1;

message Greeting {
  string name = 1;
  string locale = 2;
}

enum Tone {
  TONE_UNSPECIFIED = 0;
  TONE_FORMAL = 1;
}
`

// importer.proto references Greeting (another file) + Tone. Line map:
// 0 syntax, 1 package, 2 blank, 3 import, 4 blank, 5 `message Envelope {`,
// 6 `  Greeting greeting = 1;`, 7 `  Tone tone = 2;`, 8 `}`.
const xfImporterProto = `syntax = "proto3";
package myapp.v1;

import "myapp/v1/greeting.proto";

message Envelope {
  Greeting greeting = 1;
  Tone tone = 2;
}
`

// nested.proto declares a message nested inside another — the #237
// outer-qualified target. Line map: 3 `message Wrapper {`,
// 4 `  message Payload {`.
const xfNestedProto = `syntax = "proto3";
package myapp.v1;

message Wrapper {
  message Payload {
    string body = 1;
  }
}
`

// newProtoImportWorkspace builds a Workspace whose lookup is a real
// proto_files lookup over greeting.proto + importer.proto +
// nested.proto on disk, and returns it. The registry:// previews in
// the tests resolve their imports through this lookup.
func newProtoImportWorkspace(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
	}
	write("myapp/v1/greeting.proto", xfGreetingProto)
	write("myapp/v1/importer.proto", xfImporterProto)
	write("myapp/v1/nested.proto", xfNestedProto)

	lk, closer, _, err := schema.OpenProtoFiles(context.Background(), schema.ProtoFilesConfig{
		Sources: []schema.ProtoFilesSource{{Paths: []string{root}}},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, lk)
	t.Cleanup(func() {
		if closer != nil {
			_ = closer.Close()
		}
	})

	w := New(nil)
	w.Lookups().Swap(lk, closer)
	return w
}

func TestRegistryCrossFile_TypeRefJumpsToAnotherRegistryFile(t *testing.T) {
	w := newProtoImportWorkspace(t)
	const importerURI = "registry://_proto_files_/myapp/v1/importer.proto"
	w.OpenShadowProto(uri.URI(importerURI), xfImporterProto)

	// Cursor on the "Greeting" type ref (line 6, char 4) — a message
	// declared in another registry file reached via the import.
	loc := w.DefinitionFor(protocol.DocumentURI(importerURI), protocol.Position{Line: 6, Character: 4})
	require.NotNil(t, loc, "cross-file type ref resolves through the namespace lookup")
	assert.Equal(t, uri.URI("registry://_proto_files_/myapp/v1/greeting.proto"), uri.URI(loc.URI),
		"target is the OTHER registry file, as a registry:// URI (not file://)")
	assert.Equal(t, uint32(3), loc.Range.Start.Line,
		"range points at the Greeting message declaration line")
}

func TestRegistryCrossFile_EnumRefResolves(t *testing.T) {
	w := newProtoImportWorkspace(t)
	const importerURI = "registry://_proto_files_/myapp/v1/importer.proto"
	w.OpenShadowProto(uri.URI(importerURI), xfImporterProto)

	// Cursor on the "Tone" enum ref (line 7, char 4). Enums resolve via
	// the parsed AST — the Lookup interface exposes no enum-by-name.
	loc := w.DefinitionFor(protocol.DocumentURI(importerURI), protocol.Position{Line: 7, Character: 4})
	require.NotNil(t, loc, "cross-file enum ref resolves too")
	assert.Equal(t, uri.URI("registry://_proto_files_/myapp/v1/greeting.proto"), uri.URI(loc.URI))
	assert.Equal(t, uint32(8), loc.Range.Start.Line, "range points at the enum declaration line")
}

func TestRegistryCrossFile_OuterQualifiedNestedTypeResolves(t *testing.T) {
	w := newProtoImportWorkspace(t)
	// A preview in a DIFFERENT package references the nested type by
	// its outer-qualified name: package + enclosing message +
	// leaf. The old qualifier-equals-package check filtered this out.
	const src = `syntax = "proto3";
package other.ns;

import "myapp/v1/nested.proto";

message Box {
  myapp.v1.Wrapper.Payload payload = 1;
}
`
	const u = "registry://_proto_files_/other/ns/box.proto"
	w.OpenShadowProto(uri.URI(u), src)

	// Line 6: `  myapp.v1.Wrapper.Payload payload = 1;` — cursor on
	// "Payload".
	loc := w.DefinitionFor(protocol.DocumentURI(u), protocol.Position{Line: 6, Character: 20})
	require.NotNil(t, loc, "outer-qualified nested type resolves through the namespace lookup")
	assert.Equal(t, uri.URI("registry://_proto_files_/myapp/v1/nested.proto"), uri.URI(loc.URI))
	assert.Equal(t, uint32(4), loc.Range.Start.Line,
		"range points at the nested Payload declaration line")

	// A nested path that misses keeps the graceful nil.
	hover := w.HoverFor(protocol.DocumentURI(u), protocol.Position{Line: 6, Character: 20})
	require.NotNil(t, hover, "outer-qualified nested type hovers")
	assert.Contains(t, hover.Contents.Value, "**message Payload**")
}

func TestRegistryCrossFile_ImportDirectiveJumps(t *testing.T) {
	w := newProtoImportWorkspace(t)
	const importerURI = "registry://_proto_files_/myapp/v1/importer.proto"
	w.OpenShadowProto(uri.URI(importerURI), xfImporterProto)

	// Cursor inside the import string (line 3). Jumps to the imported
	// registry file at its start.
	loc := w.DefinitionFor(protocol.DocumentURI(importerURI), protocol.Position{Line: 3, Character: 15})
	require.NotNil(t, loc, "import directive resolves to the imported registry file")
	assert.Equal(t, uri.URI("registry://_proto_files_/myapp/v1/greeting.proto"), uri.URI(loc.URI))
	assert.Equal(t, uint32(0), loc.Range.Start.Line, "import jump lands at file start")
}

func TestRegistryCrossFile_UnresolvedImportReturnsNil(t *testing.T) {
	w := newProtoImportWorkspace(t)
	// A preview whose import doesn't exist in the lookup.
	const src = `syntax = "proto3";
package myapp.v1;

import "myapp/v1/missing.proto";

message Envelope {
  Ghost g = 1;
}
`
	const u = "registry://_proto_files_/myapp/v1/broken.proto"
	w.OpenShadowProto(uri.URI(u), src)

	// Type ref to a type in the missing import → no target.
	loc := w.DefinitionFor(protocol.DocumentURI(u), protocol.Position{Line: 6, Character: 2})
	assert.Nil(t, loc, "a type from an unresolvable import has nowhere to jump")

	// Import directive on the missing file → no target.
	impLoc := w.DefinitionFor(protocol.DocumentURI(u), protocol.Position{Line: 3, Character: 15})
	assert.Nil(t, impLoc, "an import that doesn't resolve returns nil")
}

func TestRegistryCrossFile_InFileStillWins(t *testing.T) {
	w := newProtoImportWorkspace(t)
	// A self-contained preview: the type is declared in the same file,
	// so resolution stays in-buffer (stamps the request URI) and never
	// consults the lookup.
	const src = `syntax = "proto3";
package myapp.v1;

message Outer {
  Inner inner = 1;
}

message Inner {
  string x = 1;
}
`
	const u = "registry://_proto_files_/myapp/v1/self.proto"
	w.OpenShadowProto(uri.URI(u), src)

	// Cursor on "Inner" (line 4). Resolves in-file to line 7.
	loc := w.DefinitionFor(protocol.DocumentURI(u), protocol.Position{Line: 4, Character: 4})
	require.NotNil(t, loc)
	assert.Equal(t, uri.URI(u), uri.URI(loc.URI), "in-file jump stays on the same preview")
	assert.Equal(t, uint32(7), loc.Range.Start.Line)
}
