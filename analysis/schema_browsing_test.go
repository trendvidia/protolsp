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
	"go.lsp.dev/uri"
)

// newProtoFilesWorkspace builds an engine over a proto_files
// workspace with two messages (myapp.v1.Farewell / .Greeting), the
// same shape the shell's initialize produces. Shared by the #203
// schema-browsing recipe tests below.
func newProtoFilesWorkspace(t *testing.T) *Workspace {
	t.Helper()
	dir := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	write(filepath.Join(dir, ".protolsp.pxf"),
		"@type protolsp.v1.WorkspaceConfig\n\nproto_files {\n  paths = [\"proto/\"]\n}\n")
	write(filepath.Join(dir, "proto", "myapp", "v1", "greeting.proto"),
		"syntax = \"proto3\";\npackage myapp.v1;\n\nmessage Greeting {\n  string name = 1;\n}\n\nmessage Farewell {\n  string reason = 1;\n}\n")

	w := New(nil)
	w.ws.SetWorkspaceRoot(uri.File(dir))
	w.SetBufLint(false)
	w.LoadWorkspaceFileConfig()
	w.RebuildRootProtoFilesLookup()
	return w
}

func TestRegistryStatus_NoLookupConfigured(t *testing.T) {
	w := New(nil)
	got := w.RegistryStatus()
	assert.False(t, got.Configured, "fresh engine has no lookup")
	assert.False(t, got.Stale)
}

func TestRegistryStatus_ProtoFilesLookupCounts(t *testing.T) {
	// Snapshot falls through registry → proto_files (W2), so a
	// proto_files-only workspace reads as configured. Local compiles
	// are never cache-stale.
	w := newProtoFilesWorkspace(t)
	got := w.RegistryStatus()
	assert.True(t, got.Configured)
	assert.False(t, got.Stale)
}

func TestListMessages_NoLookupReturnsEmptyList(t *testing.T) {
	// Empty list, not an error — schema browsing simply isn't
	// available, and the non-nil slice keeps the JSON response
	// `[]` rather than `null`.
	w := New(nil)
	res, err := w.ListMessages(context.Background(), "")
	require.NoError(t, err)
	require.NotNil(t, res.Messages)
	assert.Empty(t, res.Messages)
}

func TestListMessages_SortedAndPrefixFiltered(t *testing.T) {
	w := newProtoFilesWorkspace(t)

	res, err := w.ListMessages(context.Background(), "myapp.")
	require.NoError(t, err)
	names := make([]string, 0, len(res.Messages))
	for _, m := range res.Messages {
		names = append(names, m.FullName)
		assert.Equal(t, "myapp/v1/greeting.proto", m.File)
	}
	assert.Equal(t, []string{"myapp.v1.Farewell", "myapp.v1.Greeting"}, names,
		"deduped and sorted by FullName")

	res, err = w.ListMessages(context.Background(), "myapp.v1.G")
	require.NoError(t, err)
	require.Len(t, res.Messages, 1)
	assert.Equal(t, "myapp.v1.Greeting", res.Messages[0].FullName)

	res, err = w.ListMessages(context.Background(), "other.")
	require.NoError(t, err)
	assert.Empty(t, res.Messages)
}

func TestListMessages_CancelledContextSurfacesError(t *testing.T) {
	// The partial result is dropped: a truncated listing rendered
	// as complete would be worse than an error.
	w := newProtoFilesWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.ListMessages(ctx, "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestSourceContent_FetchesThroughLookup(t *testing.T) {
	w := newProtoFilesWorkspace(t)
	res, err := w.SourceContent(context.Background(),
		BuildRegistryURI("local", "myapp/v1/greeting.proto"))
	require.NoError(t, err)
	assert.Contains(t, res.Content, "message Greeting",
		"raw .proto bytes come back through GetSource")
}

func TestSourceContent_MalformedURI(t *testing.T) {
	w := New(nil)
	_, err := w.SourceContent(context.Background(), uri.URI("file:///x.proto"))
	require.ErrorIs(t, err, ErrNotRegistryURI)
}

func TestSourceContent_NoLookupConfigured(t *testing.T) {
	w := New(nil)
	_, err := w.SourceContent(context.Background(),
		BuildRegistryURI("acme", "billing/v1/invoice.proto"))
	require.ErrorIs(t, err, ErrNoSchemaLookup)
}
