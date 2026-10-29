// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func renameFromHandle(t *testing.T, srv *Server, u string, line, char uint32, newName string) (*protocol.WorkspaceEdit, *jsonrpc2.Error) {
	t.Helper()
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentRename,
		&protocol.RenameParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
				Position:     protocol.Position{Line: line, Character: char},
			},
			NewName: newName,
		})
	require.NoError(t, err)

	var captured *protocol.WorkspaceEdit
	var rpcErr *jsonrpc2.Error
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		if replyErr != nil {
			if e, ok := replyErr.(*jsonrpc2.Error); ok {
				rpcErr = e
				return nil
			}
			require.NoError(t, replyErr)
		}
		if result == nil {
			return nil
		}
		b, err := json.Marshal(result)
		require.NoError(t, err)
		var we protocol.WorkspaceEdit
		require.NoError(t, json.Unmarshal(b, &we))
		captured = &we
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	return captured, rpcErr
}

func TestRename_ProducesEditsAcrossOpenDocs(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, greetingValid)))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/b.pxf", 1, greetingValid)))

	edit, rpcErr := renameFromHandle(t, srv, "file:///work/a.pxf", 2, 0, "label")
	require.Nil(t, rpcErr)
	require.NotNil(t, edit)
	require.Len(t, edit.Changes, 2, "rename touches both docs")

	for _, edits := range edit.Changes {
		require.Len(t, edits, 1)
		assert.Equal(t, "label", edits[0].NewText)
		// Length of `name` is 4 → the edit range covers exactly
		// the identifier.
		assert.Equal(t, uint32(0), edits[0].Range.Start.Character)
		assert.Equal(t, uint32(4), edits[0].Range.End.Character)
	}
}

func TestRename_RejectsInvalidIdentifier(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, greetingValid)))

	for _, badName := range []string{"", "1name", "with space", "true", "false", "null", "has-dash"} {
		edit, rpcErr := renameFromHandle(t, srv, "file:///work/a.pxf", 2, 0, badName)
		assert.Nil(t, edit, "invalid name %q should not produce edits", badName)
		require.NotNil(t, rpcErr, "invalid name %q should return rpc error", badName)
		assert.Equal(t, jsonrpc2.InvalidParams, rpcErr.Code)
	}
}

func TestRename_ReturnsNilWhenCursorOffPath(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, greetingValid)))

	edit, rpcErr := renameFromHandle(t, srv, "file:///work/a.pxf", 1, 0, "ok")
	assert.Nil(t, edit)
	assert.Nil(t, rpcErr)
}

// TestRename_FindsHitsInUnopenedFileOnDisk mirrors
// TestReferences_FindsHitsInUnopenedFileOnDisk to lock in the
// scope-parity guarantee: rename and references must walk the
// same set of files. If they ever diverge, the user gets a
// silent "Find References shows 3 hits, rename only edits 1"
// bug.
func TestRename_FindsHitsInUnopenedFileOnDisk(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "a.pxf"), []byte(greetingValid), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "b.pxf"), []byte(greetingValid), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	aURI := uri.File(filepath.Join(workspaceDir, "a.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(aURI), 1, greetingValid)))

	edit, rpcErr := renameFromHandle(t, srv, string(aURI), 2, 0, "label")
	require.Nil(t, rpcErr)
	require.NotNil(t, edit)
	require.Len(t, edit.Changes, 2, "rename must reach the on-disk b.pxf, not only the open a.pxf")

	bURI := uri.File(filepath.Join(workspaceDir, "b.pxf"))
	for _, target := range []uri.URI{aURI, bURI} {
		edits, ok := edit.Changes[protocol.DocumentURI(target)]
		require.True(t, ok, "expected an edit entry for %s", target)
		require.Len(t, edits, 1)
		assert.Equal(t, "label", edits[0].NewText)
	}
}

// TestRename_OpenDocOverridesDiskContent is the rename-side mirror
// of TestReferences_OpenDocOverridesDiskContent: when an open
// buffer carries unsaved edits that change which lines contain
// the symbol, rename must use the buffer (not the stale disk
// content) to compute its edits. A client that batches the
// resulting WorkspaceEdit and applies it would otherwise rewrite
// the wrong byte range on the saved file.
func TestRename_OpenDocOverridesDiskContent(t *testing.T) {
	// On-disk b.pxf names the field `other`; the editor has
	// unsaved edits that renamed it to `name`. Rename triggered
	// from a.pxf must find the in-editor `name` occurrence on
	// the b.pxf line 2, not be misled by the disk version's
	// `other` text.
	const editedSource = `@type acme.demo.v1.Greeting

name = "fresh"
`
	const onDisk = `@type acme.demo.v1.Greeting

other = "stale"
`
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "a.pxf"), []byte(greetingValid), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "b.pxf"), []byte(onDisk), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	aURI := uri.File(filepath.Join(workspaceDir, "a.pxf"))
	bURI := uri.File(filepath.Join(workspaceDir, "b.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(aURI), 1, greetingValid)))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(bURI), 1, editedSource)))

	edit, rpcErr := renameFromHandle(t, srv, string(aURI), 2, 0, "label")
	require.Nil(t, rpcErr)
	require.NotNil(t, edit)
	require.Len(t, edit.Changes, 2,
		"open b.pxf has `name`; rename must include it AND a.pxf")

	// b.pxf's edit must anchor on line 2 (the in-editor location)
	// and have NewText="label". An unsoundly disk-reading
	// implementation would either skip b.pxf (no `name` on disk)
	// or anchor on the stale `other` row.
	bEdits, ok := edit.Changes[protocol.DocumentURI(bURI)]
	require.True(t, ok, "rename must include the buffer-state of b.pxf")
	require.Len(t, bEdits, 1)
	assert.Equal(t, "label", bEdits[0].NewText)
	assert.Equal(t, uint32(2), bEdits[0].Range.Start.Line,
		"buffer's `name` lives on line 2 — disk's stale line 2 has `other`, which must not anchor the edit")
}

// TestRename_ScopeMatchesReferences exercises the invariant that
// every (uri, range) tuple References returns must
// also appear in the Rename WorkspaceEdit, on identical input.
// Asserted against a mixed workspace (one open + one on-disk +
// one in a sub-workspace that must be excluded from both).
func TestRename_ScopeMatchesReferences(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "open.pxf"), []byte(greetingValid), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "ondisk.pxf"), []byte(greetingValid), 0o600))

	// Sub-workspace boundary — should NOT appear in either result.
	subDir := filepath.Join(workspaceDir, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(subDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(subDir, "isolated.pxf"), []byte(greetingValid), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	openURI := uri.File(filepath.Join(workspaceDir, "open.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(openURI), 1, greetingValid)))

	// References: collect (uri, start) tuples for stable comparison.
	refs := referencesFromHandle(t, srv, string(openURI), 2, 0, true)
	refKeys := tupleSet(refs)

	// Rename on the same cursor position.
	edit, rpcErr := renameFromHandle(t, srv, string(openURI), 2, 0, "label")
	require.Nil(t, rpcErr)
	require.NotNil(t, edit)
	renameKeys := workspaceEditTupleSet(edit)

	assert.Equal(t, refKeys, renameKeys,
		"rename and references must target the same (uri, start-line:col) tuples")

	// Concretely: open + ondisk in, sub-workspace's isolated out.
	ondiskURI := uri.File(filepath.Join(workspaceDir, "ondisk.pxf"))
	isolatedURI := uri.File(filepath.Join(subDir, "isolated.pxf"))
	for _, want := range []uri.URI{openURI, ondiskURI} {
		_, ok := edit.Changes[protocol.DocumentURI(want)]
		assert.True(t, ok, "expected rename to include %s", want)
	}
	_, isolatedIn := edit.Changes[protocol.DocumentURI(isolatedURI)]
	assert.False(t, isolatedIn, "sub-workspace's isolated.pxf must not leak into rename")
}

// tupleSet flattens a Location list into stable (uri,line,char)
// strings so we can compare two result sets ignoring slice order.
func tupleSet(locs []protocol.Location) []string {
	out := make([]string, 0, len(locs))
	for _, l := range locs {
		out = append(out, formatTuple(string(l.URI), l.Range.Start))
	}
	sort.Strings(out)
	return out
}

func workspaceEditTupleSet(edit *protocol.WorkspaceEdit) []string {
	out := make([]string, 0, len(edit.Changes))
	for u, edits := range edit.Changes {
		for _, e := range edits {
			out = append(out, formatTuple(string(u), e.Range.Start))
		}
	}
	sort.Strings(out)
	return out
}

func formatTuple(u string, p protocol.Position) string {
	return fmt.Sprintf("%s@%d:%d", u, p.Line, p.Character)
}

func TestRename_AdvertisesCapability(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	call, err := jsonrpc2.NewCall(jsonrpc2.NewNumberID(1),
		protocol.MethodInitialize,
		&protocol.InitializeParams{})
	require.NoError(t, err)
	var caps map[string]interface{}
	reply := func(_ context.Context, result interface{}, _ error) error {
		b, _ := json.Marshal(result)
		var probe struct {
			Capabilities map[string]interface{} `json:"capabilities"`
		}
		require.NoError(t, json.Unmarshal(b, &probe))
		caps = probe.Capabilities
		return nil
	}
	require.NoError(t, srv.Handle(context.Background(), reply, call))
	assert.Equal(t, true, caps["renameProvider"])
}
