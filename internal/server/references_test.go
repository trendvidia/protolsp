// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

func referencesFromHandle(t *testing.T, srv *Server, u string, line, char uint32, includeDecl bool) []protocol.Location {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1),
		protocol.MethodTextDocumentReferences,
		&protocol.ReferenceParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri.URI(u)},
				Position:     protocol.Position{Line: line, Character: char},
			},
			Context: protocol.ReferenceContext{IncludeDeclaration: includeDecl},
		})
	require.NoError(t, err)

	var captured []protocol.Location
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		if result == nil {
			return nil
		}
		b, err := json.Marshal(result)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &captured))
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return captured
}

func TestReferences_FindsTopLevelFieldAcrossOpenDocs(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	// Two docs that both set `name`. With no `.protolsp.pxf`
	// ancestor they fall into the same "no workspace dir" cohort
	// — references should match across both.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, greetingValid)))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/b.pxf", 1, greetingValid)))

	// greetingValid layout: line 0 @type, line 1 blank, line 2 `name = "world"`.
	// Cursor on the `name` token of a.pxf.
	locs := referencesFromHandle(t, srv, "file:///work/a.pxf", 2, 0, true)
	require.Len(t, locs, 2, "name field referenced in both open docs")

	uris := map[string]bool{}
	for _, l := range locs {
		uris[string(l.URI)] = true
	}
	assert.True(t, uris["file:///work/a.pxf"])
	assert.True(t, uris["file:///work/b.pxf"])
}

func TestReferences_NestedPathOnlyMatchesSamePath(t *testing.T) {
	const layered = `@type acme.demo.v1.Greeting

name = "outer"
`
	const nestedSrc = `@type acme.demo.v1.Greeting

other = "x"
`
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/x.pxf", 1, layered)))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/y.pxf", 1, nestedSrc)))

	locs := referencesFromHandle(t, srv, "file:///work/x.pxf", 2, 0, true)
	require.Len(t, locs, 1, "`name` and `other` are distinct paths")
	assert.Equal(t, uri.URI("file:///work/x.pxf"), locs[0].URI)
}

func TestReferences_ExcludeDeclarationDropsCursorSite(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, greetingValid)))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/b.pxf", 1, greetingValid)))

	locs := referencesFromHandle(t, srv, "file:///work/a.pxf", 2, 0, false)
	require.Len(t, locs, 1, "decl excluded → only b.pxf's occurrence remains")
	assert.Equal(t, uri.URI("file:///work/b.pxf"), locs[0].URI)
}

func TestReferences_CursorOffPathReturnsNil(t *testing.T) {
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/a.pxf", 1, greetingValid)))

	// Line 1 is blank — no path under the cursor.
	locs := referencesFromHandle(t, srv, "file:///work/a.pxf", 1, 0, true)
	assert.Nil(t, locs)
}

func TestReferences_NestedBlockPathScopesToContext(t *testing.T) {
	// `host` appears once at top-level and once inside `db`. They
	// have distinct paths (host vs. db.host) so a reference search
	// from one doesn't return the other.
	const src = `@type acme.demo.v1.Greeting

host = "x"
db {
  host = "y"
}
`
	srv, _, _ := newServerWithLookup(t)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/n.pxf", 1, src)))

	// Cursor on the top-level `host` (line 2).
	topLocs := referencesFromHandle(t, srv, "file:///work/n.pxf", 2, 0, true)
	require.Len(t, topLocs, 1, "top-level host is its own path")
	assert.Equal(t, uint32(2), topLocs[0].Range.Start.Line)

	// Cursor on the nested `host` inside `db` (line 4, col 2).
	innerLocs := referencesFromHandle(t, srv, "file:///work/n.pxf", 4, 2, true)
	require.Len(t, innerLocs, 1, "nested db.host is its own path")
	assert.Equal(t, uint32(4), innerLocs[0].Range.Start.Line)
}

// minimalWsConfig is the smallest .protolsp.pxf that satisfies
// the workspace-config parser — enough for the filesystem-walk
// tests below to anchor a workspace directory.
const minimalWsConfig = `@type protolsp.v1.WorkspaceConfig
`

func TestReferences_FindsHitsInUnopenedFileOnDisk(t *testing.T) {
	// Two .pxf files on disk; the client only opens one. The
	// filesystem walk should still surface the un-opened file's
	// occurrence.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "a.pxf"), []byte(greetingValid), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "b.pxf"), []byte(greetingValid), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)

	// Only a.pxf is opened by the client.
	aURI := uri.File(filepath.Join(workspaceDir, "a.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(aURI), 1, greetingValid)))

	locs := referencesFromHandle(t, srv, string(aURI), 2, 0, true)
	require.Len(t, locs, 2, "filesystem walk should find b.pxf even though it isn't open")

	uris := map[string]bool{}
	for _, l := range locs {
		uris[string(l.URI)] = true
	}
	bURI := uri.File(filepath.Join(workspaceDir, "b.pxf"))
	assert.True(t, uris[string(aURI)], "decl in a.pxf")
	assert.True(t, uris[string(bURI)], "on-disk b.pxf surfaced via walk")
}

func TestReferences_OpenDocOverridesDiskContent(t *testing.T) {
	// The on-disk version has a stale `other = "x"`; the editor
	// has unsaved edits that replace `other` with `name`. A
	// references search for `name` from another file should pick
	// up the in-editor occurrence, not the stale disk one.
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
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(aURI), 1, greetingValid)))
	// b.pxf opened with edited content — open doc must shadow disk.
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(bURI), 1, editedSource)))

	locs := referencesFromHandle(t, srv, string(aURI), 2, 0, true)
	require.Len(t, locs, 2, "edited b.pxf has `name`; should match")
	uris := map[string]bool{}
	for _, l := range locs {
		uris[string(l.URI)] = true
	}
	assert.True(t, uris[string(aURI)])
	assert.True(t, uris[string(bURI)])
}

func TestReferences_SkipsSubWorkspace(t *testing.T) {
	// Sub-workspace boundary stops the walk so cross-workspace
	// occurrences don't leak in.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "a.pxf"), []byte(greetingValid), 0o600))

	subDir := filepath.Join(workspaceDir, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(subDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(subDir, "leaked.pxf"), []byte(greetingValid), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	aURI := uri.File(filepath.Join(workspaceDir, "a.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(aURI), 1, greetingValid)))

	locs := referencesFromHandle(t, srv, string(aURI), 2, 0, true)
	require.Len(t, locs, 1, "sub-workspace's `name` must not leak in")
	assert.Equal(t, uri.URI(aURI), locs[0].URI)
}

func TestReferences_SkipsHiddenDirsAndNonPxfFiles(t *testing.T) {
	// Hidden dirs (.git etc) and non-.pxf files must not be
	// walked, even when their bytes happen to look like a match.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "a.pxf"), []byte(greetingValid), 0o600))

	hiddenDir := filepath.Join(workspaceDir, ".git")
	require.NoError(t, os.MkdirAll(hiddenDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(hiddenDir, "stash.pxf"), []byte(greetingValid), 0o600))
	// A .txt file with content that would otherwise match. The
	// extension filter must reject it.
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "notes.txt"), []byte(greetingValid), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	aURI := uri.File(filepath.Join(workspaceDir, "a.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(aURI), 1, greetingValid)))

	locs := referencesFromHandle(t, srv, string(aURI), 2, 0, true)
	require.Len(t, locs, 1, "hidden-dir + non-pxf occurrences must be skipped")
	assert.Equal(t, uri.URI(aURI), locs[0].URI)
}

// TestReferences_LargeWorkspaceCorrectness exercises the
// parallel walk on a workspace bigger than NumCPU so the worker
// pool actually fans out. Verifies all hits surface (one per
// file) and that the result is a flat, complete list — the
// parallel implementation must not drop work or duplicate
// candidates.
func TestReferences_LargeWorkspaceCorrectness(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))

	// 50 .pxf files: well past NumCPU on every machine, small
	// enough that the test completes in a few ms.
	const fileCount = 50
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("layer-%02d.pxf", i)
		require.NoError(t, os.WriteFile(
			filepath.Join(workspaceDir, name), []byte(greetingValid), 0o600))
	}

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	openURI := uri.File(filepath.Join(workspaceDir, "layer-00.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(openURI), 1, greetingValid)))

	locs := referencesFromHandle(t, srv, string(openURI), 2, 0, true)
	require.Len(t, locs, fileCount,
		"every layer file's `name` field must surface exactly once")

	// All hits should be distinct URIs (no double-processing).
	seen := make(map[string]bool, fileCount)
	for _, l := range locs {
		seen[string(l.URI)] = true
	}
	assert.Len(t, seen, fileCount, "no duplicate URIs in result set")
}

// TestReferences_TruncatesAndWarnsPastCap drops the
// per-workspace cap to a small number, writes more files than
// that, and asserts: (a) the result is truncated, (b) a
// window/showMessage warning fires so the client can tell the
// user the answer is partial.
func TestReferences_TruncatesAndWarnsPastCap(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))

	const cap = 3
	const fileCount = 10
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("layer-%02d.pxf", i)
		require.NoError(t, os.WriteFile(
			filepath.Join(workspaceDir, name), []byte(greetingValid), 0o600))
	}

	srv, fc := initializeWithWorkspace(t, workspaceDir, nil)
	srv.SetMaxReferencesFiles(cap)

	openURI := uri.File(filepath.Join(workspaceDir, "layer-00.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(openURI), 1, greetingValid)))

	locs := referencesFromHandle(t, srv, string(openURI), 2, 0, true)
	// Result is at most `cap` from disk; the open-doc shadow pass
	// at the end may add the in-editor copy if it isn't already
	// represented — so we assert "<= cap+1" rather than "== cap".
	assert.LessOrEqual(t, len(locs), cap+1,
		"truncated walk should not return more than cap+1 hits (open-doc shadow may overlap)")
	assert.GreaterOrEqual(t, len(locs), 1,
		"truncated walk must still return some hits")

	// Truncation must surface as a window/showMessage warning so
	// the client can tell the user the answer is partial.
	msgs := fc.shownMessages()
	var truncMsg *protocol.ShowMessageParams
	for i := range msgs {
		if strings.Contains(msgs[i].Message, "references results truncated") {
			truncMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, truncMsg, "expected a 'references results truncated' showMessage; got: %v", msgs)
	assert.Equal(t, protocol.MessageTypeWarning, truncMsg.Type)
	assert.Contains(t, truncMsg.Message, fmt.Sprintf("%d", cap),
		"warning should cite the effective cap")
}

// TestReferences_NoWarningUnderCap is the negative counterpart:
// when the workspace fits under the cap, no truncation warning
// should fire — keeping the user from getting a popup on every
// references click on a normal-sized workspace.
func TestReferences_NoWarningUnderCap(t *testing.T) {
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(minimalWsConfig), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "a.pxf"), []byte(greetingValid), 0o600))

	srv, fc := initializeWithWorkspace(t, workspaceDir, nil)
	// Default cap is 10000, way above the single file we wrote.

	aURI := uri.File(filepath.Join(workspaceDir, "a.pxf"))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(aURI), 1, greetingValid)))

	_ = referencesFromHandle(t, srv, string(aURI), 2, 0, true)
	for _, m := range fc.shownMessages() {
		assert.NotContains(t, m.Message, "references results truncated",
			"under-cap workspaces must not produce truncation warnings")
	}
}

func TestReferences_AdvertisesCapability(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	call, err := newTestCall(jsonrpc2.NewNumberID(1),
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
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	assert.Equal(t, true, caps["referencesProvider"])
}
