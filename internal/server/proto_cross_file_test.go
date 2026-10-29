// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/uri"
)

// setupCrossFileWorkspace stages a two-file proto workspace:
//
//	<root>/buf.yaml                       (v2, module path=proto)
//	<root>/proto/acme/bar.proto           (defines `Bar`)
//	<root>/proto/acme/foo.proto           (imports bar.proto, refs `Bar`)
//
// Returns the workspace root + the absolute paths to the two protos.
// Server is initialized + opened on foo.proto so cross-file hover /
// definition can be exercised against the workspace index built at
// init time.
func setupCrossFileWorkspace(t *testing.T) (root, fooPath, barPath string, srv *Server) {
	t.Helper()
	root = t.TempDir()

	mustWrite(t, filepath.Join(root, "buf.yaml"), `version: v2
modules:
  - path: proto
`)
	barPath = filepath.Join(root, "proto", "acme", "bar.proto")
	mustWrite(t, barPath, `syntax = "proto3";

package acme.demo.v1;

// Render priority for greetings.
message Bar {
  string note = 1;
}
`)
	fooPath = filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.demo.v1;

import "acme/bar.proto";

message Foo {
  Bar reference = 1;
}
`)

	srv, _ = newServerWithFakeConn(t)
	// Workspace root is what LoadProtoIndex consults to build the
	// index. Set it to point at the temp dir; the index walks
	// proto/ from there.
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	// Open foo.proto so the LSP has a document to dispatch against.
	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	return root, fooPath, barPath, srv
}

func TestProtoHover_CrossFile_TypeReferenceResolvesViaImport(t *testing.T) {
	root, fooPath, _, srv := setupCrossFileWorkspace(t)
	_ = root

	fooURI := "file://" + filepath.ToSlash(fooPath)

	// Line 7 of foo.proto (0-indexed) is `  Bar reference = 1;`.
	// Character 3 lands in "Bar".
	hover := hoverFromHandle(t, srv, fooURI, 7, 3)
	require.NotNil(t, hover, "type ref resolves cross-file via import")
	assert.Contains(t, hover.Contents.Value, "**message Bar**",
		"cross-file hover renders the message card from bar.proto")
	assert.Contains(t, hover.Contents.Value, "Render priority for greetings",
		"the imported file's leading comment flows through")
}

func TestProtoDefinition_CrossFile_TypeReferenceJumpsToImportedFile(t *testing.T) {
	root, fooPath, barPath, srv := setupCrossFileWorkspace(t)
	_ = root

	fooURI := "file://" + filepath.ToSlash(fooPath)
	expectedBarURI := uri.URI("file://" + filepath.ToSlash(barPath))

	// Line 7 char 3 → "Bar" in foo.proto's field declaration.
	locs := definitionFromHandle(t, srv, fooURI, 7, 3)
	require.Len(t, locs, 1, "cross-file type ref produces one Location")
	assert.Equal(t, expectedBarURI, locs[0].URI,
		"go-to-def lands on bar.proto")
	// bar.proto declares `message Bar` on line 5 (0-indexed).
	// `Bar` token starts at column 8 (after `message `).
	assert.Equal(t, uint32(5), locs[0].Range.Start.Line,
		"target line is bar.proto's Bar declaration")
	assert.Equal(t, uint32(8), locs[0].Range.Start.Character,
		"target column is the start of the Bar Name token")
}

func TestProtoDefinition_ImportDirective_JumpsToImportedFile(t *testing.T) {
	root, fooPath, barPath, srv := setupCrossFileWorkspace(t)
	_ = root

	fooURI := "file://" + filepath.ToSlash(fooPath)
	expectedBarURI := uri.URI("file://" + filepath.ToSlash(barPath))

	// Line 4 (0-indexed) of foo.proto is `import "acme/bar.proto";`.
	// Character 12 sits inside the quoted string ("acme/bar.proto").
	locs := definitionFromHandle(t, srv, fooURI, 4, 12)
	require.Len(t, locs, 1, "cursor on import path resolves to the imported file")
	assert.Equal(t, expectedBarURI, locs[0].URI)
	assert.Equal(t, uint32(0), locs[0].Range.Start.Line,
		"import jump lands at file start")
	assert.Equal(t, uint32(0), locs[0].Range.Start.Character)
}

func TestProtoHover_CrossFile_UnimportedFileDoesNotResolve(t *testing.T) {
	// Adds a third proto that defines a `Quux` type but isn't
	// imported by foo.proto. A reference to `Quux` from foo.proto
	// must NOT resolve — Phase 3 only walks imports, not the
	// whole workspace.
	root, fooPath, _, srv := setupCrossFileWorkspace(t)

	otherPath := filepath.Join(root, "proto", "other", "quux.proto")
	mustWrite(t, otherPath, `syntax = "proto3";

package acme.other.v1;

message Quux {
  string x = 1;
}
`)
	srv.LoadProtoIndex() // re-index so quux is discoverable

	// Replace foo.proto with one that references Quux without
	// importing it. didChange the open document so the new AST
	// is in place.
	updated := `syntax = "proto3";

package acme.demo.v1;

import "acme/bar.proto";

message Foo {
  Quux uninvited = 1;
}
`
	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, fooURI, 2, updated)))

	// Line 7 char 3 → "Quux".
	hover := hoverFromHandle(t, srv, fooURI, 7, 3)
	if hover != nil {
		assert.NotContains(t, hover.Contents.Value, "message Quux",
			"unimported types must NOT resolve cross-file even when they exist in the workspace")
	}
}

func TestProtoDefinition_NaiveFallback_NoBufYAML(t *testing.T) {
	// Without buf.yaml the index uses the naive fallback
	// (workspace root + proto/). A flat-layout workspace where
	// imports are written relative to the workspace root should
	// still resolve.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "bar.proto"), `syntax = "proto3";

package x.v1;

message Bar {
  string note = 1;
}
`)
	fooPath := filepath.Join(root, "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package x.v1;

import "bar.proto";

message Foo {
  Bar r = 1;
}
`)

	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	expectedBarURI := uri.URI("file://" + filepath.ToSlash(filepath.Join(root, "bar.proto")))

	locs := definitionFromHandle(t, srv, fooURI, 7, 3)
	require.Len(t, locs, 1, "naive-fallback workspace still resolves cross-file")
	assert.Equal(t, expectedBarURI, locs[0].URI)
}

// Sanity: Phase 2's in-file resolution still works when the index
// is in place. Catches a regression where cross-file lookup would
// accidentally win even when an in-file match exists.
func TestProtoDefinition_CrossFile_InFileStillWins(t *testing.T) {
	// Workspace where TWO files declare `Bar`. foo.proto imports
	// bar.proto AND also defines its own Bar. The in-file Bar
	// must win.
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	mustWrite(t, filepath.Join(root, "proto", "acme", "bar.proto"), `syntax = "proto3";

package acme.v1;

message Bar { string fromBar = 1; }
`)
	fooPath := filepath.Join(root, "proto", "acme", "foo.proto")
	mustWrite(t, fooPath, `syntax = "proto3";

package acme.v1;

import "acme/bar.proto";

message Bar { string fromFoo = 1; }

message Wrapper {
  Bar field = 1;
}
`)

	srv, _ := newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()

	fooURI := "file://" + filepath.ToSlash(fooPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, fooURI, 1, mustRead(t, fooPath))))

	// Line 9 (0-indexed): `  Bar field = 1;`. Character 3 → "Bar".
	locs := definitionFromHandle(t, srv, fooURI, 9, 3)
	require.Len(t, locs, 1)
	assert.Equal(t, uri.URI("file://"+filepath.ToSlash(fooPath)), locs[0].URI,
		"in-file Bar declaration wins over the imported Bar")
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// A workspace-local import the proto index resolves must not
// surface as a compile error — the same buf.yaml-driven include paths
// that back hover / definition / completion also back the compile
// pass. Before the fix every cross-file import squiggled with
// "file does not exist" even though the index resolved it fine.
func TestDidOpen_WorkspaceImportCompilesCleanViaIndex(t *testing.T) {
	_, fooPath, _, srv := setupCrossFileWorkspace(t)

	fooURI := uri.URI("file://" + filepath.ToSlash(fooPath))
	doc := srv.Documents().Get(fooURI)
	require.NotNil(t, doc)
	assert.Empty(t, doc.ProtoDiagnostics,
		"an import the index resolves must not produce diagnostics")
}

// Negative: with the index built, an import the index CANNOT
// supply still errors — resolution through the include paths must not
// swallow genuinely broken imports.
func TestDidOpen_MissingImportStillErrorsWithIndexBuilt(t *testing.T) {
	root, _, _, srv := setupCrossFileWorkspace(t)

	bazPath := filepath.Join(root, "proto", "acme", "baz.proto")
	mustWrite(t, bazPath, `syntax = "proto3";

package acme.demo.v1;

import "acme/missing.proto";
`)
	bazURI := "file://" + filepath.ToSlash(bazPath)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, bazURI, 1, mustRead(t, bazPath))))

	doc := srv.Documents().Get(uri.URI(bazURI))
	require.NotNil(t, doc)
	require.NotEmpty(t, doc.ProtoDiagnostics,
		"an import outside the index must still error")
	for _, d := range doc.ProtoDiagnostics {
		assert.Equal(t, "protolsp/proto", d.Source)
	}
}
