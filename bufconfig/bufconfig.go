// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package bufconfig parses buf.yaml + buf.lock for the proto-authoring
// Phase 3 import-resolution path. Scope is intentionally narrow: just
// enough to learn (a) which directories under the workspace contain
// module .proto sources and (b) which BSR deps the lock file pins
// to which commits. Phase 3's resolver layers on top of this to map
// `import "foo/bar.proto"` to an on-disk path.
//
// Why not vendor buf's Go SDK: the SDK is large and pulls auth /
// network / module-management surface protolsp doesn't need.
// buf.yaml and buf.lock are small YAML files with stable shape; a
// hand-rolled loader is ~200 LOC, doesn't drift with buf releases on
// the orthogonal features, and isolates protolsp from the SDK's own
// dependency graph. The tradeoff is we have to track buf's schema
// evolution ourselves — flagged in CHANGES.
//
// Schema versions supported: v1 (top-level `name` + `deps` + optional
// `build.roots`) and v2 (`modules: [{path, name}]` list). buf.work.yaml
// (the legacy multi-module workspace format) is explicitly out of
// scope per the proto-authoring design doc.
//
// Discovery convention: callers pass a workspace root; FindBufYAML
// returns the buf.yaml at that root (or nil + no error if absent).
// We don't walk up the directory tree — the workspace root is
// already the canonical anchor pxfed reports through the LSP
// initialize handshake, and walking risks picking up an enclosing
// project's config that has no relation to the file under edit.

package bufconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the merged view of a parsed buf.yaml — schema version
// erased so the resolver doesn't have to branch. Modules carry the
// resolved include paths (relative to the workspace root); Deps lists
// the BSR-named dependencies the lock file then pins to commits.
type Config struct {
	// Version is the source schema version ("v1" or "v2"). Retained
	// for diagnostic logging — resolvers should read Modules / Deps
	// rather than branching on this.
	Version string

	// Modules are the local module roots. Each Path is the
	// workspace-relative directory containing .proto sources. v1
	// emits a single module rooted at the buf.yaml's directory
	// (with optional Build.Roots refining it); v2 emits one entry
	// per `modules:` list element. Order matters for resolution:
	// earlier entries win on duplicate import paths.
	Modules []Module

	// Deps is the list of BSR module names from the `deps:` block,
	// e.g. "buf.build/googleapis/googleapis". Empty when the file
	// has no remote dependencies. Pair with Lock.DepCommit to
	// locate the cached module bytes on disk.
	Deps []string
}

// Module is one local source root the buf.yaml declares. Path is
// workspace-relative; the resolver joins it with the workspace root
// to form an absolute path. Name is the BSR module name when the
// project publishes its module (e.g. "buf.build/acme/foo"); empty
// for unpublished local-only modules.
type Module struct {
	Path string
	Name string
}

// Lock is the parsed buf.lock pinning each BSR dep to a specific
// commit. Empty / missing buf.lock yields an empty Lock with no
// error — many projects use buf.yaml without ever pulling remote
// deps.
type Lock struct {
	// Version is the source schema version. v1 / v2 both use
	// "deps: [{name, commit, digest}]" — the schema is stable for
	// our needs.
	Version string

	// Deps maps the BSR module name (e.g. "buf.build/googleapis/googleapis")
	// to its pinned commit. The cache lookup uses both.
	Deps []LockedDep
}

// LockedDep pairs a BSR dep name with its pinned commit. Both are
// non-empty when present in buf.lock; protolsp doesn't validate the
// digest (it's a buf-CLI concern).
type LockedDep struct {
	Name   string
	Commit string
}

// DepCommit returns the commit pinned for the named BSR module, or
// "" + false when the dep isn't in the lock file. Convenience for
// the resolver — saves every caller from walking Deps.
func (l Lock) DepCommit(name string) (string, bool) {
	for _, d := range l.Deps {
		if d.Name == name {
			return d.Commit, true
		}
	}
	return "", false
}

// FindBufYAML returns the (Config, Lock) pair for the buf.yaml at
// workspaceDir, or (nil, Lock{}, nil) when no buf.yaml exists at
// that root. Errors are returned for present-but-malformed files —
// callers should surface them rather than silently degrading to
// no-resolution.
//
// The function looks for `buf.yaml` (canonical) at workspaceDir.
// buf.work.yaml (legacy multi-module workspace) is not supported —
// the proto-authoring design doc explicitly defers it. A workspace
// containing only buf.work.yaml falls back to "no resolution," which
// matches the design's "no-config workspaces use the naive include
// fallback" path.
func FindBufYAML(workspaceDir string) (*Config, Lock, error) {
	yamlPath := filepath.Join(workspaceDir, "buf.yaml")
	cfg, err := parseBufYAML(yamlPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Lock{}, nil
		}
		return nil, Lock{}, fmt.Errorf("reading %s: %w", yamlPath, err)
	}
	lockPath := filepath.Join(workspaceDir, "buf.lock")
	lock, err := parseBufLock(lockPath)
	if err != nil && !os.IsNotExist(err) {
		// buf.lock is present but broken — surface it. A missing
		// lock is fine (no BSR deps yet).
		return cfg, Lock{}, fmt.Errorf("reading %s: %w", lockPath, err)
	}
	return cfg, lock, nil
}

// parseBufYAML is the schema-version-aware reader. v1 and v2 have
// different shapes — we read into a raw struct that covers both,
// then normalize into the version-erased Config above.
func parseBufYAML(path string) (*Config, error) {
	data, err := os.ReadFile(path) //#nosec G304 -- path composed from caller-supplied workspaceDir + fixed basename
	if err != nil {
		return nil, err
	}
	var raw bufYAMLRaw
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing yaml: %w", err)
	}
	switch raw.Version {
	case "v1":
		return normalizeV1(raw), nil
	case "v2":
		return normalizeV2(raw), nil
	case "":
		return nil, fmt.Errorf("missing `version` field; expected v1 or v2")
	default:
		return nil, fmt.Errorf("unsupported buf.yaml version %q (expected v1 or v2)", raw.Version)
	}
}

// bufYAMLRaw is the union of buf.yaml v1 + v2 fields. v1 has
// top-level Name / Deps + Build.Roots; v2 has the Modules list.
// Fields not present in a given version simply unmarshal as zero
// values.
type bufYAMLRaw struct {
	Version string         `yaml:"version"`
	Name    string         `yaml:"name"`    // v1 only
	Deps    []string       `yaml:"deps"`    // v1 + v2
	Build   bufYAMLBuild   `yaml:"build"`   // v1 only
	Modules []bufYAMLV2Mod `yaml:"modules"` // v2 only
}

// bufYAMLBuild is the v1 `build:` block. Roots are the legacy
// equivalent of v2's module Paths — relative directories under the
// buf.yaml containing module .proto sources. When unset, the
// buf.yaml's own directory is the implicit single root.
type bufYAMLBuild struct {
	Roots []string `yaml:"roots"`
}

// bufYAMLV2Mod is one entry in the v2 `modules:` list. Path is the
// directory (relative to the buf.yaml). Name is the optional BSR
// publishing name.
type bufYAMLV2Mod struct {
	Path string `yaml:"path"`
	Name string `yaml:"name"`
}

// normalizeV1 collapses a v1 raw config into the version-erased
// Config shape. The single module is rooted at "." (the buf.yaml's
// directory) by default; build.roots refine it to one entry per
// listed root.
func normalizeV1(raw bufYAMLRaw) *Config {
	cfg := &Config{
		Version: "v1",
		Deps:    raw.Deps,
	}
	if len(raw.Build.Roots) == 0 {
		cfg.Modules = []Module{{Path: ".", Name: raw.Name}}
	} else {
		for _, r := range raw.Build.Roots {
			cfg.Modules = append(cfg.Modules, Module{Path: r, Name: raw.Name})
		}
	}
	return cfg
}

// normalizeV2 collapses a v2 raw config into Config. Modules pass
// through as-is; deps come from the top-level deps block (v2 keeps
// deps at the top level, not per-module).
func normalizeV2(raw bufYAMLRaw) *Config {
	cfg := &Config{
		Version: "v2",
		Deps:    raw.Deps,
	}
	for _, m := range raw.Modules {
		path := m.Path
		if path == "" {
			path = "."
		}
		cfg.Modules = append(cfg.Modules, Module{Path: path, Name: m.Name})
	}
	return cfg
}

// parseBufLock reads buf.lock and returns its Deps. Missing file
// returns an os.IsNotExist error so callers can distinguish "no
// lock" (fine) from "broken lock" (surface).
func parseBufLock(path string) (Lock, error) {
	data, err := os.ReadFile(path) //#nosec G304 -- path composed from caller-supplied workspaceDir + fixed basename
	if err != nil {
		return Lock{}, err
	}
	var raw bufLockRaw
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Lock{}, fmt.Errorf("parsing yaml: %w", err)
	}
	lock := Lock{Version: raw.Version}
	for _, d := range raw.Deps {
		if d.Name == "" || d.Commit == "" {
			// Skip incomplete entries — buf is supposed to write
			// both, but we'd rather drop a stray than fail the
			// whole resolver setup.
			continue
		}
		lock.Deps = append(lock.Deps, LockedDep{Name: d.Name, Commit: d.Commit})
	}
	return lock, nil
}

// bufLockRaw covers v1 and v2 lock shapes — both use `deps: [{name,
// commit, digest}]`. The digest is buf-internal (used to verify
// fetched bytes); protolsp reads commit-pinned bytes from the cache
// that buf itself already validated.
type bufLockRaw struct {
	Version string          `yaml:"version"`
	Deps    []bufLockRawDep `yaml:"deps"`
}

type bufLockRawDep struct {
	Name   string `yaml:"name"`
	Commit string `yaml:"commit"`
}

// ModuleCacheDir returns the absolute path to the buf CLI cache
// directory where downloaded BSR modules live, or "" + an error
// when no usable cache directory can be located.
//
// buf uses the platform's user cache directory plus "buf/v3/modules"
// (one path component per OS, but Go's os.UserCacheDir() abstracts
// that — on macOS it returns ~/Library/Caches, on Linux it follows
// XDG_CACHE_HOME or ~/.cache, on Windows %LocalAppData%).
//
// Cache layout under that root:
//
//	buf.build/<owner>/<name>/<commit>/files/<import-path>.proto
//
// Phase 3's resolver joins ModuleCacheDir() + the LockedDep fields
// to locate descriptor bytes for a remote import.
func ModuleCacheDir() (string, error) {
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating user cache dir: %w", err)
	}
	return filepath.Join(root, "buf", "v3", "modules"), nil
}
