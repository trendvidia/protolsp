// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package protoindex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trendvidia/protolsp/bufconfig"
)

const sampleProtoA = `syntax = "proto3";

package acme.demo.v1;

message Foo {
  string name = 1;
}
`

const sampleProtoB = `syntax = "proto3";

package acme.demo.v1;

message Bar {
  int32 count = 1;
}
`

func TestIndex_WalksBufYAMLModules(t *testing.T) {
	// Workspace layout:
	//   workspace/
	//     proto/
	//       acme/foo.proto
	//       acme/bar.proto
	root := t.TempDir()
	mkProto(t, root, "proto/acme/foo.proto", sampleProtoA)
	mkProto(t, root, "proto/acme/bar.proto", sampleProtoB)

	cfg := &bufconfig.Config{
		Version: "v2",
		Modules: []bufconfig.Module{{Path: "proto"}},
	}

	idx := New(root, cfg, bufconfig.Lock{}, nil)

	keys := idx.ImportPaths()
	assert.ElementsMatch(t, []string{"acme/foo.proto", "acme/bar.proto"}, keys,
		"both files appear under their module-relative import paths")

	// Lookup returns a non-nil parsed AST.
	got := idx.Lookup("acme/foo.proto")
	require.NotNil(t, got)
	assert.Equal(t, filepath.Join(root, "proto", "acme", "foo.proto"), got.Name(),
		"AST.Name() carries the resolved on-disk path")
}

func TestIndex_FirstIncludePathWins(t *testing.T) {
	// Two modules declare the same import path; the first one in
	// declaration order should claim the key (buf-compatible
	// precedence). Different file contents prove which won.
	root := t.TempDir()
	mkProto(t, root, "api/acme/foo.proto", sampleProtoA)
	mkProto(t, root, "internal/acme/foo.proto", sampleProtoB)

	cfg := &bufconfig.Config{
		Version: "v2",
		Modules: []bufconfig.Module{
			{Path: "api"},
			{Path: "internal"},
		},
	}
	idx := New(root, cfg, bufconfig.Lock{}, nil)

	got := idx.Lookup("acme/foo.proto")
	require.NotNil(t, got)
	assert.Equal(t, filepath.Join(root, "api", "acme", "foo.proto"), got.Name(),
		"`api` module declared first → its file wins on the collision")
}

func TestIndex_NaiveFallbackWhenNoBufYAML(t *testing.T) {
	// No buf.yaml → workspace root + proto/ subdir become the
	// include paths (open question D from the design doc).
	root := t.TempDir()
	mkProto(t, root, "top.proto", sampleProtoA)
	mkProto(t, root, "proto/acme/foo.proto", sampleProtoB)

	idx := New(root, nil, bufconfig.Lock{}, nil)

	assert.ElementsMatch(t, []string{"top.proto", "acme/foo.proto"}, idx.ImportPaths(),
		"workspace root contributes `top.proto`; proto/ subdir contributes `acme/foo.proto`")
}

func TestIndex_NaiveFallbackOmitsAbsentProtoSubdir(t *testing.T) {
	// No buf.yaml + no proto/ subdir → only the workspace root is
	// an include path. The walk shouldn't include the workspace
	// itself if it has no .proto files (no error, no spurious
	// entries).
	root := t.TempDir()

	idx := New(root, nil, bufconfig.Lock{}, nil)
	assert.Empty(t, idx.ImportPaths(), "no protos → empty index, no errors")

	// IncludePaths should hold just the root (proto/ wasn't added
	// because it doesn't exist).
	paths := idx.IncludePaths()
	require.Len(t, paths, 1)
	assert.Equal(t, root, paths[0])
}

func TestIndex_ProtoSubdirAddedWhenPresent(t *testing.T) {
	// Same as above but proto/ exists — it should appear in
	// IncludePaths.
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "proto"), 0o700))

	idx := New(root, nil, bufconfig.Lock{}, nil)
	paths := idx.IncludePaths()
	require.Len(t, paths, 2)
	assert.Equal(t, root, paths[0])
	assert.Equal(t, filepath.Join(root, "proto"), paths[1])
}

func TestIndex_LookupUnknownImport(t *testing.T) {
	root := t.TempDir()
	mkProto(t, root, "foo.proto", sampleProtoA)

	idx := New(root, nil, bufconfig.Lock{}, nil)
	assert.Nil(t, idx.Lookup("does/not/exist.proto"),
		"unknown imports return nil cleanly — no panic")
}

func TestIndex_NilReceiverSafe(t *testing.T) {
	// Defensive: callers that haven't built an index yet should be
	// able to ask anyway. All getters tolerate nil.
	var idx *Index
	assert.Nil(t, idx.Lookup("x"))
	assert.Nil(t, idx.ImportPaths())
	assert.Nil(t, idx.IncludePaths())
}

func TestIndex_EmptyWorkspaceRoot(t *testing.T) {
	idx := New("", nil, bufconfig.Lock{}, nil)
	assert.Empty(t, idx.ImportPaths())
	assert.Empty(t, idx.IncludePaths(),
		"empty root → empty index, no walks attempted")
}

func TestIndex_LazyParse(t *testing.T) {
	// The first Lookup parses; subsequent Lookups return the same
	// cached *FileNode (pointer equality proves it's not re-parsed).
	root := t.TempDir()
	mkProto(t, root, "foo.proto", sampleProtoA)

	idx := New(root, nil, bufconfig.Lock{}, nil)
	first := idx.Lookup("foo.proto")
	second := idx.Lookup("foo.proto")
	require.NotNil(t, first)
	assert.Same(t, first, second, "lookups return the same cached AST")
}

func TestIndex_LookupHandlesBrokenProto(t *testing.T) {
	// A .proto with a parse error gets discovered but yields nil
	// AST on Lookup. The resolver treats nil-AST same as
	// unresolved — no special handling needed.
	root := t.TempDir()
	mkProto(t, root, "broken.proto", "this is not valid proto syntax {")

	idx := New(root, nil, bufconfig.Lock{}, nil)
	// The file IS in the index (discovery records it).
	assert.Contains(t, idx.ImportPaths(), "broken.proto")
	// But Lookup may return non-nil (protocompile recovers many
	// parse errors and still produces a FileNode) or nil. Either
	// is acceptable — the resolver downstream handles both.
	_ = idx.Lookup("broken.proto") // no panic is the assertion
}

func TestIndex_ForwardSlashImportKeys(t *testing.T) {
	// Sanity check: import keys use forward slashes regardless of
	// host OS. On Unix this is automatic; protocompile / proto
	// itself only emits forward-slash imports.
	root := t.TempDir()
	mkProto(t, root, "proto/a/b/c.proto", sampleProtoA)

	cfg := &bufconfig.Config{
		Version: "v2",
		Modules: []bufconfig.Module{{Path: "proto"}},
	}
	idx := New(root, cfg, bufconfig.Lock{}, nil)
	assert.Contains(t, idx.ImportPaths(), "a/b/c.proto",
		"nested path uses forward slashes")
	assert.NotNil(t, idx.Lookup("a/b/c.proto"))
}

// mkProto creates root/relPath, including parent directories, with
// the given content. The contents don't need to be parseable for
// most tests — discovery doesn't validate.
func mkProto(t *testing.T, root, relPath, content string) {
	t.Helper()
	abs := filepath.Join(root, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o700))
	require.NoError(t, os.WriteFile(abs, []byte(content), 0o600))
}

// TestIndex_BSRCacheLookup stages a fake buf module cache under a
// temp dir and verifies the index resolves a `buf.lock`-pinned dep
// against that cache. This is the regression guard for the BSR
// integration path that previously had no test coverage (the
// production code went straight from bufconfig.ModuleCacheDir() to
// the user's real filesystem, which made it untestable; the
// NewWithCacheRoot override added in this PR is purely for test
// fixtures).
func TestIndex_BSRCacheLookup(t *testing.T) {
	// Workspace declares a single module + a single BSR dep.
	root := t.TempDir()
	mkProto(t, root, "proto/local.proto", sampleProtoA)

	// Stage the fake cache: <cache>/<name>/<commit>/files/<path>.proto.
	// This mirrors the layout buf CLI v3 maintains under
	// $UserCacheDir/buf/v3/modules.
	cacheRoot := t.TempDir()
	depName := "buf.build/googleapis/googleapis"
	commit := "deadbeefdeadbeefdeadbeefdeadbeef"
	mkProto(t, filepath.Join(cacheRoot, depName, commit, "files"),
		"google/api/annotations.proto", sampleProtoB)

	cfg := &bufconfig.Config{
		Version: "v2",
		Modules: []bufconfig.Module{{Path: "proto"}},
		Deps:    []string{depName},
	}
	lock := bufconfig.Lock{
		Version: "v2",
		Deps:    []bufconfig.LockedDep{{Name: depName, Commit: commit}},
	}

	idx := NewWithCacheRoot(root, cfg, lock, cacheRoot, nil)

	// The local module file appears under its module-relative key.
	assert.NotNil(t, idx.Lookup("local.proto"),
		"local module file resolves against the workspace's `proto/` module")

	// The BSR dep file appears under its module-relative key from
	// the cache's `files/` root.
	assert.NotNil(t, idx.Lookup("google/api/annotations.proto"),
		"BSR dep file resolves against the cache")

	// Both paths show up in the IncludePaths list, in priority
	// order: module roots first, BSR cache dirs second.
	paths := idx.IncludePaths()
	require.Len(t, paths, 2)
	assert.Equal(t, filepath.Join(root, "proto"), paths[0],
		"local module root has higher priority than BSR cache")
	assert.Equal(t, filepath.Join(cacheRoot, depName, commit, "files"), paths[1],
		"BSR cache dir is the resolved per-dep path")
}

// LocalImportPaths lists only files under the workspace root — BSR
// cache dep files stay out, so the declared-name export (#272) never
// force-parses the module cache.
func TestIndex_LocalImportPaths(t *testing.T) {
	root := t.TempDir()
	mkProto(t, root, "proto/local.proto", sampleProtoA)
	mkProto(t, root, "proto/acme/other.proto", sampleProtoB)

	cacheRoot := t.TempDir()
	depName := "buf.build/googleapis/googleapis"
	commit := "deadbeefdeadbeefdeadbeefdeadbeef"
	mkProto(t, filepath.Join(cacheRoot, depName, commit, "files"),
		"google/api/annotations.proto", sampleProtoB)

	cfg := &bufconfig.Config{
		Version: "v2",
		Modules: []bufconfig.Module{{Path: "proto"}},
		Deps:    []string{depName},
	}
	lock := bufconfig.Lock{
		Version: "v2",
		Deps:    []bufconfig.LockedDep{{Name: depName, Commit: commit}},
	}

	idx := NewWithCacheRoot(root, cfg, lock, cacheRoot, nil)

	assert.Equal(t, []string{"acme/other.proto", "local.proto"}, idx.LocalImportPaths(),
		"workspace-local files only, sorted; the cache dep is excluded")
	assert.NotNil(t, idx.Lookup("google/api/annotations.proto"),
		"the cache dep is still resolvable through Lookup")

	var nilIdx *Index
	assert.Nil(t, nilIdx.LocalImportPaths(), "nil receiver degrades to nil")
	assert.Nil(t, New("", nil, bufconfig.Lock{}, nil).LocalImportPaths(),
		"empty workspace root yields nil")
}

func TestIndex_BSRCacheLookup_MissingCacheDirSkipped(t *testing.T) {
	// buf.lock lists a dep but the cache doesn't have it on disk —
	// the index should degrade silently (no panic, no error) and
	// just not include that BSR include path. The user gets a
	// "no resolution" hover for the dep's types, which is
	// accurate: the bytes aren't there to resolve against.
	root := t.TempDir()
	mkProto(t, root, "proto/local.proto", sampleProtoA)

	cacheRoot := t.TempDir() // empty cache; the named dep won't be found

	cfg := &bufconfig.Config{
		Version: "v2",
		Modules: []bufconfig.Module{{Path: "proto"}},
		Deps:    []string{"buf.build/absent/dep"},
	}
	lock := bufconfig.Lock{
		Version: "v2",
		Deps:    []bufconfig.LockedDep{{Name: "buf.build/absent/dep", Commit: "abc"}},
	}

	idx := NewWithCacheRoot(root, cfg, lock, cacheRoot, nil)

	// Only the local module include path made the cut.
	paths := idx.IncludePaths()
	require.Len(t, paths, 1, "missing BSR cache dir is skipped, not propagated as an error")
	assert.Equal(t, filepath.Join(root, "proto"), paths[0])

	// Lookups for the missing dep return nil.
	assert.Nil(t, idx.Lookup("absent/types.proto"))
}

func TestIndex_BSRCacheLookup_EmptyCacheRootSkipsAllDeps(t *testing.T) {
	// Passing cacheRoot="" via the testable constructor skips BSR
	// integration entirely — useful for tests that want to verify
	// module-only behavior without touching the cache.
	root := t.TempDir()
	mkProto(t, root, "proto/local.proto", sampleProtoA)

	cfg := &bufconfig.Config{
		Version: "v2",
		Modules: []bufconfig.Module{{Path: "proto"}},
		Deps:    []string{"buf.build/whatever/dep"},
	}
	lock := bufconfig.Lock{
		Deps: []bufconfig.LockedDep{{Name: "buf.build/whatever/dep", Commit: "abc"}},
	}

	idx := NewWithCacheRoot(root, cfg, lock, "", nil)
	paths := idx.IncludePaths()
	require.Len(t, paths, 1, "empty cacheRoot disables BSR lookups")
}
