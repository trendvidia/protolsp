// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package bufconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindBufYAML_V1_ImplicitRoot(t *testing.T) {
	// v1 buf.yaml with no build.roots — module is rooted at "."
	// (the buf.yaml's own directory).
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), `version: v1
name: buf.build/acme/foo
deps:
  - buf.build/googleapis/googleapis
`)

	cfg, lock, err := FindBufYAML(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "v1", cfg.Version)
	assert.Equal(t, []Module{{Path: ".", Name: "buf.build/acme/foo"}}, cfg.Modules,
		"v1 with no build.roots → single module rooted at the buf.yaml's directory")
	assert.Equal(t, []string{"buf.build/googleapis/googleapis"}, cfg.Deps)
	assert.Empty(t, lock.Deps, "no buf.lock → empty Lock")
}

func TestFindBufYAML_V1_BuildRoots(t *testing.T) {
	// v1 with multiple roots — order is preserved (the resolver
	// will use it for priority).
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), `version: v1
name: buf.build/acme/multi
build:
  roots:
    - proto/api
    - proto/internal
`)

	cfg, _, err := FindBufYAML(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, []Module{
		{Path: "proto/api", Name: "buf.build/acme/multi"},
		{Path: "proto/internal", Name: "buf.build/acme/multi"},
	}, cfg.Modules)
}

func TestFindBufYAML_V2(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), `version: v2
modules:
  - path: proto/api
    name: buf.build/acme/api
  - path: proto/internal
deps:
  - buf.build/googleapis/googleapis
`)

	cfg, _, err := FindBufYAML(dir)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "v2", cfg.Version)
	assert.Equal(t, []Module{
		{Path: "proto/api", Name: "buf.build/acme/api"},
		{Path: "proto/internal", Name: ""},
	}, cfg.Modules)
	assert.Equal(t, []string{"buf.build/googleapis/googleapis"}, cfg.Deps)
}

func TestFindBufYAML_V2_EmptyPath_DefaultsToDot(t *testing.T) {
	// v2 module with no `path:` — equivalent to ".".
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), `version: v2
modules:
  - name: buf.build/acme/foo
`)

	cfg, _, err := FindBufYAML(dir)
	require.NoError(t, err)
	require.Equal(t, ".", cfg.Modules[0].Path,
		"omitted path normalizes to current directory")
}

func TestFindBufYAML_MissingFile_ReturnsNilNoError(t *testing.T) {
	// No buf.yaml at the workspace root → (nil, Lock{}, nil).
	// The resolver uses this signal to fall back to naive include
	// paths (proto-authoring design doc, open question D).
	dir := t.TempDir()

	cfg, lock, err := FindBufYAML(dir)
	require.NoError(t, err)
	assert.Nil(t, cfg)
	assert.Empty(t, lock.Deps)
}

func TestFindBufYAML_BadYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), "version: v2\nmodules: not-a-list\n")

	_, _, err := FindBufYAML(dir)
	require.Error(t, err, "malformed yaml surfaces as an error rather than silent no-resolution")
}

func TestFindBufYAML_UnknownVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), `version: v99
modules:
  - path: proto
`)

	_, _, err := FindBufYAML(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported buf.yaml version",
		"explicit error message helps users identify the schema mismatch")
}

func TestFindBufYAML_MissingVersion(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), "modules:\n  - path: proto\n")

	_, _, err := FindBufYAML(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing `version`")
}

func TestFindBufYAML_WithLock(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), `version: v2
modules:
  - path: proto
deps:
  - buf.build/googleapis/googleapis
`)
	writeFile(t, filepath.Join(dir, "buf.lock"), `version: v2
deps:
  - name: buf.build/googleapis/googleapis
    commit: 28151c0d0a1641bf938a7672c500e01d
    digest: shake256:abc123
`)

	_, lock, err := FindBufYAML(dir)
	require.NoError(t, err)
	require.Len(t, lock.Deps, 1)
	assert.Equal(t, "buf.build/googleapis/googleapis", lock.Deps[0].Name)
	assert.Equal(t, "28151c0d0a1641bf938a7672c500e01d", lock.Deps[0].Commit)

	commit, ok := lock.DepCommit("buf.build/googleapis/googleapis")
	assert.True(t, ok)
	assert.Equal(t, "28151c0d0a1641bf938a7672c500e01d", commit)

	_, ok = lock.DepCommit("buf.build/nope/missing")
	assert.False(t, ok, "DepCommit returns false for unlisted deps")
}

func TestFindBufYAML_LockSkipsIncompleteEntries(t *testing.T) {
	// A lock dep missing either name or commit is dropped rather
	// than failing the whole load. We'd rather degrade to "this
	// dep can't resolve" than refuse to serve any cross-file
	// hovers because one entry is malformed.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	writeFile(t, filepath.Join(dir, "buf.lock"), `version: v2
deps:
  - name: buf.build/ok/dep
    commit: abc123
  - commit: missing-name
  - name: buf.build/missing/commit
`)

	_, lock, err := FindBufYAML(dir)
	require.NoError(t, err)
	require.Len(t, lock.Deps, 1)
	assert.Equal(t, "buf.build/ok/dep", lock.Deps[0].Name)
}

func TestFindBufYAML_BrokenLockSurfaces(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "buf.yaml"), "version: v2\nmodules:\n  - path: proto\n")
	writeFile(t, filepath.Join(dir, "buf.lock"), "this is not yaml: [unclosed\n")

	_, _, err := FindBufYAML(dir)
	require.Error(t, err, "malformed buf.lock surfaces — a present-but-broken lock is worse than no lock")
}

func TestModuleCacheDir(t *testing.T) {
	// Smoke check — the path should be non-empty and end in the
	// expected suffix. We don't assert the prefix because it
	// varies per OS, and that's the whole point of using
	// os.UserCacheDir.
	dir, err := ModuleCacheDir()
	require.NoError(t, err)
	assert.Contains(t, dir, filepath.Join("buf", "v3", "modules"),
		"cache dir layout follows buf's documented v3 structure")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}
