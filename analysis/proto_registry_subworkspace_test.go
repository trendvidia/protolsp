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

// A registry:// preview opened from a SUB-workspace must resolve
// its content and cross-file imports against THAT sub-workspace's
// registry, not the root's. Before the fix, LookupForNamespace always
// used the root lookup, so a sub-workspace-only type silently failed to
// resolve (or resolved against the wrong registry).

// subOnlyProto is a standalone file the ROOT registry knows. It shares
// nothing with myapp/v1/*, so a root-scoped resolution of a myapp type
// necessarily misses — which is how these tests tell the two registries
// apart.
const subOnlyProto = `syntax = "proto3";
package other.v1;

message Other {
  string x = 1;
}
`

// protoFilesLookupOver builds a real proto_files-backed schema.Lookup
// over the given rel→body files in a fresh temp dir; cleanup closes it.
// Returns just the lookup so a test can install it on the root slot or
// a sub-workspace slot interchangeably.
func protoFilesLookupOver(t *testing.T, files map[string]string) schema.Lookup {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
	}
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
	return lk
}

// newMultiRootRegistryWorkspace wires a root workspace whose registry
// knows ONLY subOnlyProto, and a sub-workspace "svc" whose own registry
// holds greeting.proto + importer.proto. Returns the workspace and the
// absolute sub-workspace dir.
func newMultiRootRegistryWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	rootDir := t.TempDir()
	subDir := filepath.Join(rootDir, "svc")
	require.NoError(t, os.MkdirAll(subDir, 0o700))

	w := New(nil)
	w.ws.SetWorkspaceRoot(uri.File(rootDir))

	// Root registry: an unrelated file, so it can't satisfy myapp/v1/*.
	w.Lookups().Swap(protoFilesLookupOver(t, map[string]string{
		"other/v1/other.proto": subOnlyProto,
	}), nil)

	// Sub-workspace "svc" binds its OWN registry with the myapp files.
	sw := w.EnsureSubWorkspace(subDir)
	w.SwapSubWorkspaceLookup(sw, protoFilesLookupOver(t, map[string]string{
		"myapp/v1/greeting.proto": xfGreetingProto,
		"myapp/v1/importer.proto": xfImporterProto,
	}), nil)

	return w, subDir
}

const importerRegistryURI = "registry://_proto_files_/myapp/v1/importer.proto"
const greetingRegistryURI = "registry://_proto_files_/myapp/v1/greeting.proto"

func TestRegistrySubWorkspace_CrossFileResolvesAgainstSubRegistry(t *testing.T) {
	w, subDir := newMultiRootRegistryWorkspace(t)

	// Preview opened FROM the sub-workspace: OpenShadowProtoForWorkspace
	// records the dir on the shadow doc, so go-to-definition routes the
	// cross-file lookup to the sub's registry — the only one that holds
	// greeting.proto.
	w.OpenShadowProtoForWorkspace(uri.URI(importerRegistryURI), xfImporterProto, subDir)

	// Cursor on the "Greeting" type ref (line 6, char 4).
	loc := w.DefinitionFor(uri.URI(importerRegistryURI), protocol.Position{Line: 6, Character: 4})
	require.NotNil(t, loc, "Greeting resolves through the sub-workspace registry, not the root's")
	assert.Equal(t, uri.URI(greetingRegistryURI), uri.URI(loc.URI),
		"cross-file target is the sub-workspace registry's greeting.proto")
	assert.Equal(t, uint32(3), loc.Range.Start.Line,
		"range points at the Greeting message declaration line")
}

func TestRegistrySubWorkspace_HoverResolvesAgainstSubRegistry(t *testing.T) {
	w, subDir := newMultiRootRegistryWorkspace(t)
	w.OpenShadowProtoForWorkspace(uri.URI(importerRegistryURI), xfImporterProto, subDir)

	// Hover on the same cross-file "Greeting" ref must render its card,
	// which needs the imported file's AST from the sub's registry.
	hov := w.HoverFor(uri.URI(importerRegistryURI), protocol.Position{Line: 6, Character: 4})
	require.NotNil(t, hov, "cross-file hover resolves through the sub-workspace registry")
	assert.Contains(t, hoverText(hov), "Greeting",
		"the hover card renders the imported message declared in the sub's registry")
}

func TestRegistrySubWorkspace_RootScopedPreviewMissesSubOnlyType(t *testing.T) {
	w, _ := newMultiRootRegistryWorkspace(t)

	// The SAME preview, opened at ROOT scope (no workspace dir). The root
	// registry has no myapp/v1/greeting.proto, so the cross-file jump
	// finds nothing, which is correct at root scope.
	w.OpenShadowProto(uri.URI(importerRegistryURI), xfImporterProto)

	loc := w.DefinitionFor(uri.URI(importerRegistryURI), protocol.Position{Line: 6, Character: 4})
	assert.Nil(t, loc, "the root registry can't satisfy a sub-workspace-only cross-file ref")
}

func TestRegistrySubWorkspace_SourceContentRoutesByWorkspace(t *testing.T) {
	w, subDir := newMultiRootRegistryWorkspace(t)

	// Scoped to the sub-workspace: its registry serves the file.
	sub, err := w.SourceContentForWorkspace(context.Background(), uri.URI(importerRegistryURI), subDir)
	require.NoError(t, err, "the sub-workspace registry serves its own file")
	assert.Equal(t, xfImporterProto, sub.Content)

	// Root-scoped (the plain SourceContent entry point): the root
	// registry has no myapp/v1/importer.proto, so the fetch fails rather
	// than silently returning a wrong-registry file.
	_, rootErr := w.SourceContent(context.Background(), uri.URI(importerRegistryURI))
	require.Error(t, rootErr, "the root registry has no myapp/v1/importer.proto")
}

func TestRegistrySubWorkspace_ASTCacheKeyedByWorkspace(t *testing.T) {
	// Two sub-workspaces bind different registries under the SAME
	// namespace + import path. The cross-file AST cache must key on the
	// workspace dir so one sub's parse can't satisfy the other's fetch.
	rootDir := t.TempDir()
	subA := filepath.Join(rootDir, "svc-a")
	subB := filepath.Join(rootDir, "svc-b")
	require.NoError(t, os.MkdirAll(subA, 0o700))
	require.NoError(t, os.MkdirAll(subB, 0o700))

	w := New(nil)
	w.ws.SetWorkspaceRoot(uri.File(rootDir))

	// Same path "myapp/v1/greeting.proto" in both, but different package
	// — the distinguishing signal the cache must not conflate.
	const greetingV1 = "syntax = \"proto3\";\npackage myapp.v1;\nmessage Greeting { string name = 1; }\n"
	const greetingV2 = "syntax = \"proto3\";\npackage myapp.v2;\nmessage Greeting { string name = 1; }\n"

	swA := w.EnsureSubWorkspace(subA)
	w.SwapSubWorkspaceLookup(swA, protoFilesLookupOver(t, map[string]string{
		"myapp/v1/greeting.proto": greetingV1,
	}), nil)
	swB := w.EnsureSubWorkspace(subB)
	w.SwapSubWorkspaceLookup(swB, protoFilesLookupOver(t, map[string]string{
		"myapp/v1/greeting.proto": greetingV2,
	}), nil)

	ctx := context.Background()
	astA := w.registryImportAST(ctx, "_proto_files_", subA, "myapp/v1/greeting.proto")
	astB := w.registryImportAST(ctx, "_proto_files_", subB, "myapp/v1/greeting.proto")
	require.NotNil(t, astA)
	require.NotNil(t, astB)
	assert.Equal(t, "myapp.v1", filePackage(astA), "svc-a resolves against its own registry")
	assert.Equal(t, "myapp.v2", filePackage(astB),
		"svc-b resolves against ITS registry — not svc-a's cached parse under the same path")
}

func TestRegistrySubWorkspace_LookupSwapClearsCache(t *testing.T) {
	// A sub-workspace registry rebuild must drop the cross-file AST cache
	// (notifyLookupChanged now mirrors the root manager's onChanged), so
	// a stale parse from the old lookup can't leak into the new one.
	rootDir := t.TempDir()
	subDir := filepath.Join(rootDir, "svc")
	require.NoError(t, os.MkdirAll(subDir, 0o700))

	w := New(nil)
	w.ws.SetWorkspaceRoot(uri.File(rootDir))

	const greetingV1 = "syntax = \"proto3\";\npackage myapp.v1;\nmessage Greeting { string name = 1; }\n"
	const greetingV2 = "syntax = \"proto3\";\npackage myapp.v2;\nmessage Greeting { string name = 1; }\n"

	sw := w.EnsureSubWorkspace(subDir)
	w.SwapSubWorkspaceLookup(sw, protoFilesLookupOver(t, map[string]string{
		"myapp/v1/greeting.proto": greetingV1,
	}), nil)

	ctx := context.Background()
	first := w.registryImportAST(ctx, "_proto_files_", subDir, "myapp/v1/greeting.proto")
	require.NotNil(t, first)
	assert.Equal(t, "myapp.v1", filePackage(first))

	// Rebuild the sub's registry with different content; the swap fires
	// notifyLookupChanged, which clears the cache.
	w.SwapSubWorkspaceLookup(sw, protoFilesLookupOver(t, map[string]string{
		"myapp/v1/greeting.proto": greetingV2,
	}), nil)

	second := w.registryImportAST(ctx, "_proto_files_", subDir, "myapp/v1/greeting.proto")
	require.NotNil(t, second)
	assert.Equal(t, "myapp.v2", filePackage(second),
		"after the sub-workspace lookup swap the cache re-parses against the new registry")
}
