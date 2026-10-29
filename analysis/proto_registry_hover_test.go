// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// Reuses newProtoImportWorkspace + the xf* fixtures from
// proto_registry_crossfile_test.go (same package): greeting.proto
// defines Greeting (message) + Tone (enum); importer.proto imports it
// and references both.

func TestRegistryCrossFileHover_MessageCard(t *testing.T) {
	w := newProtoImportWorkspace(t)
	const importerURI = "registry://_proto_files_/myapp/v1/importer.proto"
	w.OpenShadowProto(uri.URI(importerURI), xfImporterProto)

	// Cursor on the "Greeting" type ref (line 6, char 4) — declared in
	// another registry file.
	h := w.HoverFor(protocol.DocumentURI(importerURI), protocol.Position{Line: 6, Character: 4})
	require.NotNil(t, h, "cross-file type ref hovers through the namespace lookup")
	assert.Contains(t, h.Contents.Value, "message Greeting", "renders the imported message's card")
	assert.Contains(t, h.Contents.Value, "greeting.proto", "card attributes the defining registry file")
}

func TestRegistryCrossFileHover_EnumCard(t *testing.T) {
	w := newProtoImportWorkspace(t)
	const importerURI = "registry://_proto_files_/myapp/v1/importer.proto"
	w.OpenShadowProto(uri.URI(importerURI), xfImporterProto)

	// Cursor on the "Tone" enum ref (line 7, char 4).
	h := w.HoverFor(protocol.DocumentURI(importerURI), protocol.Position{Line: 7, Character: 4})
	require.NotNil(t, h, "cross-file enum ref hovers too")
	assert.Contains(t, h.Contents.Value, "enum Tone", "renders the imported enum's card")
}

func TestRegistryCrossFileHover_InFileStillWorks(t *testing.T) {
	w := newProtoImportWorkspace(t)
	// A self-contained preview: the referenced type is in the same
	// file, so hover resolves in-buffer without consulting the lookup —
	// confirming the docURI threading didn't break the in-file path.
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

	// Cursor on "Inner" (line 4).
	h := w.HoverFor(protocol.DocumentURI(u), protocol.Position{Line: 4, Character: 4})
	require.NotNil(t, h, "in-file type ref still hovers")
	assert.Contains(t, h.Contents.Value, "message Inner")
}

func TestRegistryCrossFileHover_UnresolvedReturnsNil(t *testing.T) {
	w := newProtoImportWorkspace(t)
	const src = `syntax = "proto3";
package myapp.v1;

import "myapp/v1/missing.proto";

message Envelope {
  Ghost g = 1;
}
`
	const u = "registry://_proto_files_/myapp/v1/broken.proto"
	w.OpenShadowProto(uri.URI(u), src)

	// A type from an unresolvable import → no card.
	h := w.HoverFor(protocol.DocumentURI(u), protocol.Position{Line: 6, Character: 2})
	assert.Nil(t, h, "an unresolvable cross-file type has no hover card")
}
