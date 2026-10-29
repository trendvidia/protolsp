# Workspace configuration — `.protolsp.pxf` per worktree

| Field    | Value                                                                                                                                                                                |
|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Status   | Draft                                                                                                                                                                                |
| Date     | 2026-05-16                                                                                                                                                                           |
| Scope    | Move protolsp's per-project configuration out of editor-specific `initializationOptions` and into a `.protolsp.pxf` file committed alongside the code. Add native multi-root support via nearest-ancestor resolution. Add a new schema-source kind that loads `.proto` files directly from disk (no registry required). Stays backward-compatible with the existing initializationOptions flow. |
| Out      | Global / user-level config (`~/.config/protolsp/...`). Live watching of `.proto` files inside a `proto_files` source. Importing config from non-PXF formats (TOML, YAML). Bidirectional sync between `.protolsp.pxf` and editor settings. |

## Summary

Today, protolsp reads its registry connection and other workspace
configuration from the LSP `initialize` call's
`initializationOptions`. That model has three real limitations:

1. **Editor-specific.** Every editor that runs protolsp must
   replicate the same configuration UI. VS Code does it via
   `settings.json`; pxfed will need its own form; Neovim users
   wire it through `:lua vim.lsp.start_client`. Configuration
   drifts across environments because there's no shared source
   of truth.
2. **Not committed.** A team using protolsp can't ship its
   schema-source configuration with the code. The first engineer
   on the project re-discovers the right registry address.
3. **One config per LSP session.** A monorepo with two apps,
   each with its own protoregistry namespace, can't be served
   by one protolsp without per-folder workspace gymnastics in
   the editor.

The fix is the same pattern that other project-config tools
landed on years ago: a small config file at the worktree root
(`tsconfig.json`, `.editorconfig`, `pyproject.toml`,
`go.work`, etc.). For protolsp, that file is **`.protolsp.pxf`**.

`.protolsp.pxf` is itself a PXF document validating against a
compiled-in `protolsp.v1.WorkspaceConfig` schema. It declares
one or more **schema sources** (remote `protoregistry`, local
`proto_files`, future kinds), an optional `chameleon` block
pointing at a `chain.pxf`, and workspace-wide settings like log
level. Multiple `.protolsp.pxf` files can coexist in a single
LSP session — each open document is associated with the nearest
ancestor file, and the server maintains one schema `Lookup` per
workspace.

## Motivation

The Q1 conversation around the chameleon-integration design
revealed that the configuration story needs to expand beyond
"one registry per LSP session". Specifically:

- **Local-only proto authoring.** Open-source contributors,
  bootstrappers, and CI all need a schema-aware experience
  without standing up a protoregistry. A `proto_files` schema
  source — point at one or more directories, load `.proto`s
  directly — covers this with no new infrastructure.
- **Mixed sources.** "Shared types from the registry; app types
  from local proto files in this repo" is a realistic pattern
  during a migration. Sources need to compose, not exclude each
  other.
- **Per-worktree configuration.** A monorepo with two chameleon
  apps under sibling subtrees should use two different chain
  specs without the editor having to model multiple workspaces.
- **Configuration as code.** Schema source identity belongs in
  the repo, not in each engineer's editor settings.

## Goals

1. **One config file per worktree**, in PXF, validated against a
   compiled-in `protolsp.v1.WorkspaceConfig` schema.
2. **Nearest-ancestor resolution**: each open document picks up
   the configuration of the nearest `.protolsp.pxf` walking up
   the directory tree. Multiple `.protolsp.pxf` files in one
   session work naturally — no editor support required.
3. **Composable schema sources**: a workspace can declare 1+
   sources of any supported kind (`registry`, `proto_files`,
   future kinds). Lookups compose in declaration order.
4. **Backward compatible**: existing `initializationOptions`
   continue to work as a fallback when no `.protolsp.pxf` is
   discovered for a document.
5. **Self-validating**: edits to `.protolsp.pxf` get
   schema-aware diagnostics against `protolsp.v1.WorkspaceConfig`
   in the user's editor — bad config surfaces as a normal
   diagnostic, not a cryptic startup error.

## Non-goals

- **Global / user-level config.** No `~/.config/protolsp/`. The
  per-workspace file is the single source. If teams want
  defaults, they templatize the file in their project starter.
- **Live `.proto` watching.** `proto_files` sources load at
  workspace open and reload when `.protolsp.pxf` changes or on
  explicit user command. Watching every `.proto` directory for
  edits adds OS-level file-watching cost for marginal benefit;
  users editing a `.proto` and a `.pxf` at the same time can
  reload manually.
- **Non-PXF config formats.** `.protolsp.pxf` is PXF. No TOML
  importer, no YAML importer. PXF is what protolsp speaks; the
  whole point of this config is dogfooding.
- **Bidirectional sync with editor settings.** Editors can read
  `.protolsp.pxf` for display, but the file is authoritative.
  No "edit in settings UI, write to file" round-trip.

## The config file

### Location and naming

**`.protolsp.pxf`** at any directory that should anchor a
workspace. The leading dot follows the project-config dotfile
convention (`.editorconfig`, `.gitignore`, `.golangci.yml`). It
keeps the file visually grouped with other project metadata.

A single workspace = a single `.protolsp.pxf`. Multiple
`.protolsp.pxf` files in one tree = multiple workspaces, each
governing its subtree.

### Format

PXF, validating against `protolsp.v1.WorkspaceConfig`. The
schema descriptor is **compiled into the protolsp binary** —
parsing `.protolsp.pxf` doesn't require a working registry
connection (chicken-and-egg avoided).

```pxf
@type protolsp.v1.WorkspaceConfig

schema_sources {
  # Remote protoregistry source.
  registry {
    address    = "registry.example.com:443"
    namespace  = "acme-billing"
    token      = "${env.PROTOREGISTRY_TOKEN}"
    cache_dir  = "${env.XDG_CACHE_HOME}/protolsp"
  }
}

schema_sources {
  # Local proto files — loaded from disk, no registry needed.
  proto_files {
    paths   = ["proto/", "third_party/proto/"]
    exclude = ["**/*_test.proto"]
  }
}

chameleon {
  chain_spec = "config/chain.pxf"
  schema_fqn = "myapp.v1.AppConfig"
}

log_level = "info"
```

`${env.NAME}` interpolation resolves at config-load time
(matching chameleon's syntax). Useful for tokens and per-user
cache paths without baking them into the committed file.

### Schema sketch

```proto
syntax = "proto3";
package protolsp.v1;

message WorkspaceConfig {
  // One or more schema sources composed in declaration order.
  // Lookups try them sequentially; first hit wins, misses fall
  // through.
  repeated SchemaSource schema_sources = 1;

  // Optional chameleon chain-aware authoring config.
  ChameleonConfig chameleon = 2;

  // "" | "debug" | "info" | "warn" | "error"
  string log_level = 3;
}

message SchemaSource {
  oneof source {
    RegistrySource registry = 1;
    ProtoFilesSource proto_files = 2;
  }
}

message RegistrySource {
  string address    = 1;
  string namespace  = 2;
  string token      = 3;
  string cache_dir  = 4;
}

message ProtoFilesSource {
  // Directories scanned for *.proto, relative to .protolsp.pxf's dir.
  repeated string paths = 1;
  // Future W2 additions (not in proto/protolsp/v1 today):
  //   repeated string exclude = 2;  // glob excludes e.g. "**/*_test.proto"
  //   repeated string include = 3;  // explicit include patterns
  // Today's proto only ships `paths`; the `proto_files` source
  // itself is design-only (parser warns + skips — see W2 phase
  // in the Phased delivery section).
}

message ChameleonConfig {
  string chain_spec = 1;  // path to chain.pxf, relative to config dir
  string schema_fqn = 2;  // default @type for layers without one
  bool   enabled    = 3;  // default true when chain_spec is set
}
```

## Resolution: nearest ancestor walk

For each open document URI:

1. Convert URI → filesystem path.
2. Walk parents from that path up to the LSP-reported workspace
   root (or filesystem root if no workspace root was sent),
   looking for `.protolsp.pxf` at each level.
3. The first found = the document's workspace.
4. Cache the resolution on the Document so subsequent operations
   skip the walk.

If no file is found, fall back to `initializationOptions` from
the LSP `initialize` request (today's behavior). If those are
empty too, the document gets parse-only diagnostics.

Cache invalidation: on `workspace/didChangeWatchedFiles` events
for any `*.protolsp.pxf` glob, or on explicit
`workspace/executeCommand("protolsp/reloadConfig")`.

## Schema source composition

Multiple `schema_sources` in one workspace compose into one
resolver chain. Declaration order = lookup precedence:

- Source A (declared first) is the primary resolver.
- Source B becomes A's parent via the existing
  `protoregistry/client.WithFallback` (or `WithParent` when B is
  another `client.Resolver`).
- Source C becomes B's parent, and so on.
- Lookups walk: A's local → A's chain ancestors → B → C.

This reuses the resolver-chain machinery that already supports
`WithServerChain` and hierarchical fallback. No new code on the
protoregistry side.

For `proto_files` sources: load `.proto`s into a
`*protoregistry.Files`, then wrap as a `WithFallback` parent.
The compiled-in fallback registry approach means the file
descriptors come from the local filesystem at workspace-open
time; the LSP holds them in memory for the session.

### The `proto_files` source in detail

At workspace load:

1. Walk every `paths[]` entry. For each, walk the directory tree
   for files matching `*.proto`.
2. Apply `exclude[]` glob patterns (relative path match).
3. If `include[]` is set, also filter to matches (default: all
   non-excluded `.proto`).
4. Compile via the embedded `protoc`-equivalent (we use
   `protocompile` or `buf`'s parser — TBD; lean
   [`buf`](https://github.com/bufbuild/protocompile) for license
   reasons and pure-Go support).
5. Register every file in a `*protoregistry.Files`. Cross-file
   imports resolve against the same set.
6. Surface that registry as a schema source.

Failures during compilation surface as a diagnostic on
`.protolsp.pxf` itself ("couldn't compile `proto/foo.proto`:
syntax error at line 12") so the user sees the issue in their
editor without rummaging through logs.

## Server architecture changes

### `Workspaces` map on Server

```go
type Server struct {
    // ... existing fields ...
    workspaces *Workspaces
}

type Workspaces struct {
    mu     sync.RWMutex
    byPath map[string]*Workspace  // workspace dir → Workspace
}

type Workspace struct {
    Dir    string             // absolute directory containing .protolsp.pxf
    Config *WorkspaceConfig
    Lookup schema.Lookup
    Closer io.Closer
}

// Get resolves a document URI to its workspace via nearest-ancestor
// walk. Returns nil + ok=false when no .protolsp.pxf is found in
// any ancestor; the caller falls back to initializationOptions.
func (w *Workspaces) Get(uri DocumentURI) (*Workspace, bool)
```

`Server.lookup` (the singleton) stays as the
initializationOptions-derived fallback. Code paths that
currently consult `s.lookup` are updated to:

```go
lookup := s.lookupFor(uri)  // workspace's Lookup or s.lookup
```

with `lookupFor` doing the nearest-ancestor walk + cache.

### Document → workspace caching

Each `Document` gains a `WorkspaceDir string` field. Set on
`didOpen` after the resolution walk; reused on subsequent
operations. Cleared when the resolution invalidates (config
file changed within the document's ancestor chain).

### Per-workspace closers

Workspaces own their `Lookup` and the underlying client
Resolver. Server shutdown closes every workspace's resolver in
turn, then closes the fallback lookup. Order matters because
ancestor closers might share grpc conns when (later) we add
shared-conn pooling.

## File watching

protolsp subscribes to file changes via
`workspace/didChangeWatchedFiles`. The patterns it registers:

- `**/.protolsp.pxf` — reload workspace on edit, create,
  delete.
- `**/chain.pxf` (when chameleon is configured for at least one
  workspace) — handled by the chameleon integration; not in
  this doc's scope.

When a `.protolsp.pxf` changes:

1. Reparse + revalidate. If invalid, surface diagnostics on
   the file and **keep** the previous Lookup serving. We don't
   tear down a working state because of a typo.
2. If valid, diff against the previous Config. If schema sources
   changed, close the old Lookup and build a new one.
3. Iterate documents whose `WorkspaceDir` matches; reanalyze
   each.

When a `.protolsp.pxf` is deleted, the workspace dissolves;
documents that were associated with it re-resolve (likely to a
parent workspace, or the fallback).

When a new `.protolsp.pxf` appears (typically right after `git
checkout`), documents under it re-resolve. Documents previously
associated with a parent workspace move to the new one.

## Migration

The existing `initializationOptions.registry` block continues to
work as a **session-level fallback**. Precedence per document:

1. `.protolsp.pxf` in any ancestor → its config wins.
2. Otherwise → `initializationOptions` (today's behavior).
3. Otherwise → no schema, parse-only diagnostics.

This means:
- Users who don't add a `.protolsp.pxf` keep working unchanged.
- A repo that adds `.protolsp.pxf` immediately takes precedence
  for files under it.
- Mixed: a workspace with `.protolsp.pxf` for one app and no
  config for sibling files works — sibling files fall through
  to `initializationOptions`.

We'll deprecate the `initializationOptions.registry` block in a
future minor (with a `window/showMessage` nudge), but it
doesn't go away until at least one full release after
`.protolsp.pxf` is the default.

## Cross-cuts

### With chameleon-integration

The `chameleon { chain_spec = ... }` block lives in
`.protolsp.pxf`. The chameleon-integration design doc currently
references `initializationOptions.chameleon.*`; that gets
replaced with a pointer to this doc in a follow-up commit. The
underlying behavior is unchanged.

### With `registry://` URIs and `protolsp/sourceContent`

`registry://<namespace>/<path>` URIs need to resolve through the
right workspace. The handler now finds the document's workspace
and routes the `GetSource` call there. When the workspace has
multiple schema sources, the call walks them in order until one
returns success.

### With cache (`WithDiskCache`)

Each `RegistrySource.cache_dir` is per-source. The same
`.protolsp.pxf` declaring two registry sources can give each its
own cache directory, or share one — entirely the user's call.
For `proto_files` sources there's no cache (descriptors are
loaded fresh from disk on workspace open).

### With dataset regions

No direct interaction. Region detection runs against whatever
schema the document's workspace provides; the workspace just
hands the resolver to the region computation.

### With `protolsp/registryStatus`

The status response becomes per-workspace. Either:
- Add an optional `workspaceDir` param to the request; server
  returns that workspace's status. Default = ambient (probably
  the first workspace).
- Add a separate `protolsp/workspaceStatus` that enumerates all
  active workspaces with each one's per-source status.

Lean: extend `registryStatus` with optional `textDocument`
param. When set, return the status for that document's
workspace; when absent, return a combined "all workspaces"
status.

## Phased delivery

| Phase | Scope | Status |
|-------|-------|--------|
| **W1** | Parse `.protolsp.pxf`, single workspace, single registry source. `protolsp.v1.WorkspaceConfig` proto embedded in binary. Fallback to initializationOptions when not present. Tests cover well-formed + malformed config files. | ✅ shipped |
| **W2** | `proto_files` source: walk dirs, compile via protocompile, register. Compile errors surface as diagnostics on `.protolsp.pxf`. | ✅ shipped (`internal/schema/protofiles.go` + parser handler) |
| **W3** | Multi-source composition (chain registry + proto_files in one workspace) via existing `WithFallback`. Source ordering = declaration order. | ✅ shipped (`schema.WithFallback` option threaded through `dialAndInstallLookup` / `dialAndInstallSubWorkspaceLookup`) |
| **W4** | Multi-root: nearest-ancestor walk per document; `Workspaces` map; Document.WorkspaceDir caching. Multiple lookups coexist on one Server. | ✅ shipped (W4a + W4b in `workspace.go`) |
| **W5** | File watching + reload via `workspace/didChangeWatchedFiles`. Reanalyze affected documents on config change. | ✅ shipped |
| **W6** | Wire `chameleon { ... }` block. (Deprecation nudge for `initializationOptions.chameleon.*` dropped — that entry point was decided against and never shipped; see `chameleon-integration.md` decision log row dated 2026-05-16.) | ✅ shipped |

W1 is shippable on its own — single workspace, single source,
no multi-root. It unlocks "commit your registry config" without
the rest of the surface. Each subsequent phase adds capability;
nothing requires a flag day.

## Open questions

1. **`.proto` compilation engine.** `protocompile` (buf) is the
   strongest candidate — pure Go, well-tested, license-compatible.
   `protoc` shellout is out (external binary dependency). Other
   pure-Go parsers exist but are less complete. Confirm
   protocompile and lock it in W2.
2. **Workspace root reporting.** LSP's `initialize` sends
   `WorkspaceFolders`. The nearest-ancestor walk stops at those
   roots (don't walk past them to the filesystem root). Edge
   case: a `.protolsp.pxf` exists OUTSIDE any workspace folder.
   Our policy: ignore — the user opened a file the workspace
   isn't supposed to claim.
3. **Symlinks and case-insensitive filesystems.** Workspace
   discovery should canonicalize paths (resolve symlinks,
   lowercase on Windows / macOS HFS+). The Workspaces map keys
   off canonical paths to avoid two entries for the same dir.
4. **What happens if `.protolsp.pxf` is itself a chameleon
   layer?** Unlikely but legal: the user keeps a `base.pxf` and
   `env/dev.pxf` of their `.protolsp.pxf`. We don't try to
   merge those. The on-disk `.protolsp.pxf` (whatever's
   committed) is authoritative. Chameleon is for the user's
   application config, not for protolsp's own config.
5. **Bootstrapping the embedded `protolsp.v1.WorkspaceConfig`
   schema.** The proto descriptor is compiled into the binary
   via `embed.FS` or `go:embed`. It must round-trip through
   protowire-go and validate exactly the wire format the binary
   compiles against — meaning we keep the `.proto` source in
   the repo (e.g. `proto/protolsp/v1/workspace_config.proto`)
   and check in the generated descriptor blob. CI verifies the
   blob matches the source.
6. **Reporting which workspace served a document.** Useful for
   debugging but adds noise in normal use. Lean: emit on
   `--log-level=debug` only; surface via the
   `protolsp/workspaceStatus` request for the editor.

## Decision log

| Date       | Decision                                                                              | Why                                                                                          |
|------------|---------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| 2026-05-16 | File name `.protolsp.pxf` (leading dot)                                               | Project-config dotfile convention; visually groups with other metadata.                       |
| 2026-05-16 | PXF as the config format                                                              | protolsp already speaks PXF; self-validating; `${env.X}` interpolation comes for free.        |
| 2026-05-16 | Compile the WorkspaceConfig descriptor into the binary                                | Bootstrapping: parsing config can't require a working registry. Avoids chicken-and-egg.       |
| 2026-05-16 | Declaration order = source precedence                                                 | Simple. Matches how config files in other tools work.                                         |
| 2026-05-16 | Sources compose via existing `WithFallback`/`WithParent`                              | No new resolver machinery; the hierarchical-fallback case is already battle-tested.           |
| 2026-05-16 | Backward compatible: initializationOptions stays as a fallback                        | Existing VS Code extension users don't need a migration; new projects get the better path.    |
| 2026-05-16 | `proto_files`: load-once + reload on `.protolsp.pxf` change                           | OS-level proto-dir watching costs too much for marginal benefit.                              |
| 2026-05-16 | Per-document workspace caching on `Document`                                          | Avoids per-request directory walks; cheap to invalidate on config-file change.                |

## Notes for future readers

- The PXF-as-config choice is load-bearing for chameleon
  consistency. If you ever change it, also rethink whether
  `chain.pxf` should follow.
- Nearest-ancestor walk is the same primitive that `tsconfig`,
  `.editorconfig`, and `pyproject.toml` use. There's a wealth
  of prior art on resolution edge cases — symlinks, mount
  boundaries, mixed-case filesystems. Cross-reference those
  before reinventing.
- The `proto_files` source is a wedge for offline-first
  workflows (a strong pxfed differentiator). Make sure W2 is
  good enough to be a primary recommendation, not a fallback.
- `Workspaces` is a small abstraction; resist growing it.
  Anything that wants per-workspace state should live on
  `Workspace`, not as separate maps on `Server`.
