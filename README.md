# protolsp

Go Language Server for `.pxf` files (Proto eXpressive Format) — schema-aware
authoring backed by [protoregistry](https://github.com/trendvidia/protoregistry).

protolsp powers parse + schema diagnostics, hover with namespace
provenance, completion (field names, message types, enum values),
quick-fix code actions, and go-to-definition for any LSP-capable
editor (VS Code via the [PXF extension](https://github.com/trendvidia/protowire/tree/main/editors/vscode),
Neovim, Zed, Helix) plus the hosted Monaco IDE over WebSocket.

## Install

```sh
git clone https://github.com/trendvidia/protolsp
cd protolsp
go install ./cmd/protolsp ./cmd/protolsp-ws    # stdio, websocket
```

For a release, check out its tag first (`git checkout v0.23.0`).

> **Known limitation: `go install …@latest` does not work.** protolsp's
> `go.mod` replaces `google.golang.org/protobuf` with the
> [TrendVidia fork](https://github.com/trendvidia/protobuf-go), which
> carries the namespaced registry types protolsp and
> [protoregistry](https://github.com/trendvidia/protoregistry) use.
> `go install` refuses a module whose `go.mod` has a `replace`, so
> install from a checkout as above. This is accepted for now; it goes
> away only if those types reach upstream protobuf-go.
>
> The same `replace` matters if you import protolsp as a library: Go
> ignores a dependency's `replace`, so copy the line into your own
> `go.mod` (this version, or a later fork release):
>
> ```
> replace google.golang.org/protobuf => github.com/trendvidia/protobuf-go v1.36.12
> ```

The default build is the community build. Building with
`-tags protocheck` adds protocheck runtime validation through the
govm expression engine, and needs the `govm` and `protocheck-govm`
modules, which are not public.

## Features at v0.1.0

- **Parse diagnostics** via `pxf.Parse` — syntax errors with 1-indexed
  position fidelity from the parser, converted to LSP's 0-indexed range.
- **Schema-aware diagnostics** via `pxf.UnmarshalDescriptor`. The bound
  resolver is constructed with `WithServerChain` so an organization's
  namespace hierarchy resolves automatically.
- **Hover** with field type, defining file path, leading proto doc
  comments, and **namespace provenance** (`Defined in <path> (namespace
  <id>)`) when the resolver attributes the file to a tracked namespace.
- **Completion** for field names (AST-aware nested-block context),
  message types on `@type` directives, and enum values on the RHS of
  enum-typed field assignments.
- **Code actions** — Hint diagnostics with `Tags: [Deprecated]` for
  `[deprecated = true]` fields plus a "Remove field" quick-fix for
  unknown fields.
- **Go-to-definition** for field assignments, block names, and
  `@type` FQNs. URI precedence: local `file://` when on disk →
  `registry://<namespace>/<file-path>` for registry-backed virtual
  documents → blind `file://` last-resort.
- **Chameleon chain support** — layer classification, chain-aware
  required-field weighting, `${field.path}` go-to-definition and
  cycle detection, merged-value hover. See
  [`docs/design/chameleon-integration.md`](docs/design/chameleon-integration.md).
- **Secret redaction** — `pxf.Secret` values are scrubbed from
  diagnostics, hover, and completion details before reaching the
  wire. Literal-secret warnings + env-ref quick-fix.
- **Dataset region awareness** — `@dataset` block detection,
  per-row validation for huge regions, region-aware hover.

## Custom LSP methods

protolsp exposes several `protolsp/*` requests + notifications
beyond the standard LSP surface. Editor implementers wiring
status bars, sidebars, or extension UIs consume these directly.

**Requests:**

| Method | Purpose | Design |
|---|---|---|
| `protolsp/sourceContent` | Fetch bytes for a `registry://` URI via `protoregistry/client.Resolver.GetSource`. Routes `_proto_files_` to the workspace proto sources and `_internal_` to the embedded shipped types (see [Registry URI namespaces](#registry-uri-namespaces)). | mvp |
| `protolsp/listMessages` | Enumerate every message type the resolver knows (sorted, deduped, prefix-filterable). | mvp |
| `protolsp/registryStatus` | `{configured, stale}` — is the resolver wired, and is it serving live or from on-disk cache? Status-bar input. | mvp |
| `protolsp/configList` | Enumerate every config attribute known to the four-tier resolver, with type + section metadata. Drives the "configurable attributes" UI in editors that want to render a settings explorer. | [config-protocol](docs/design/config-protocol.md) |
| `protolsp/configGet` | Read one attribute's effective value + tier provenance. | [config-protocol](docs/design/config-protocol.md) |
| `protolsp/configSet` | Set or clear one attribute at the override tier. Drives the per-attribute settings forms. | [config-protocol](docs/design/config-protocol.md) |
| `protolsp/getEffectiveConfig` | Four-tier-resolved (default / file / settings / override) view of every known config section, with provenance. `revealSecrets` flag opts out of name-pattern redaction. | [config-protocol](docs/design/config-protocol.md) |
| `protolsp/setOverrides` | Install Tier 4 session overrides (e.g. for "Show debug logs for this session" UI). | [config-protocol](docs/design/config-protocol.md) |
| `protolsp/secretRanges` | Per-document ranges of every `pxf.Secret`-typed value. Editors mask these exactly. | [secret-handling](docs/design/secret-handling.md) |
| `protolsp/datasetRegions` | Per-document `@dataset` regions with row counts + Huge flag for placeholder rendering. | [dataset-regions](docs/design/dataset-regions.md) |
| `protolsp/chainStatus` | Workspace's chameleon chain summary: environments, ancestors, errors. | [chameleon-integration](docs/design/chameleon-integration.md) |
| `protolsp/layerInfo` | Per-document classification: is this file a chain layer? Which environment? | [chameleon-integration](docs/design/chameleon-integration.md) |

**Notifications:**

| Method | Fires when |
|---|---|
| `protolsp/effectiveConfigChanged` | A `setOverrides` call changes any effective value. Payload mirrors the changed subset of `getEffectiveConfig`. |
| `protolsp/secretRangesChanged` | Re-analyze produced a `SecretRanges` slice that differs from the prior snapshot. |
| `protolsp/datasetRegionsChanged` | Re-analyze produced a `DatasetRegions` slice that differs from the prior snapshot. |

Wire shapes live next to each method's dispatcher in
`internal/server` (search for `MethodSourceContent` etc.); the
linked design docs cover the protocol-level intent.

## Connect-RPC services

protolsp 0.3+ runs a typed Connect-RPC sidecar alongside the LSP
surface. Strongly-typed clients (pxfed, future hosted IDE) prefer
the RPC path; legacy LSP custom methods stay registered for
backward compatibility (deprecation horizon tracked in
[#113](https://github.com/trendvidia/protolsp/issues/113)).

Service definitions live in [`proto/protolsp/v1/rpc/*.proto`](proto/protolsp/v1/rpc).
Request/response message shapes regenerate from those `.proto`
files via `cd proto && buf generate` (see [Regenerating proto bindings](#regenerating-proto-bindings)).

| Service | Method | Kind | Mirrors LSP method |
|---|---|---|---|
| [ConfigService](proto/protolsp/v1/rpc/config.proto) | `ListAttributes` | unary | `protolsp/configList` |
| | `GetAttribute` | unary | `protolsp/configGet` |
| | `SetAttribute` | unary | `protolsp/configSet` |
| [RegistryService](proto/protolsp/v1/rpc/registry.proto) | `GetStatus` | unary | `protolsp/registryStatus` |
| [SchemaService](proto/protolsp/v1/rpc/schema.proto) | `ListMessages` | unary | `protolsp/listMessages` |
| | `GetSourceContent` | unary | `protolsp/sourceContent` |
| [ChameleonService](proto/protolsp/v1/rpc/chameleon.proto) | `GetChainStatus` | unary | `protolsp/chainStatus` |
| | `GetLayerInfo` | unary | `protolsp/layerInfo` |
| [SecretsService](proto/protolsp/v1/rpc/secrets.proto) | `GetRanges` | unary | `protolsp/secretRanges` |
| | `Subscribe` | server-streaming | `protolsp/secretRangesChanged` |
| [DatasetsService](proto/protolsp/v1/rpc/datasets.proto) | `GetRegions` | unary | `protolsp/datasetRegions` |
| | `Subscribe` | server-streaming | `protolsp/datasetRegionsChanged` |
| [EffectiveConfigService](proto/protolsp/v1/rpc/effective_config.proto) | `GetEffectiveConfig` | unary | `protolsp/getEffectiveConfig` |
| | `SetOverrides` | unary | `protolsp/setOverrides` |
| | `Subscribe` | server-streaming | `protolsp/effectiveConfigChanged` |

`Subscribe` streams send the current snapshot as their first
message, then change events for every subsequent update — clients
don't need a separate `Get*` round-trip before subscribing.

### Sidecar discovery

The sidecar listens on `127.0.0.1:<ephemeral>` over HTTP/2 (h2c).
Its address and a per-session bearer token are returned in the
LSP `initialize` result so clients can dial in without out-of-band
coordination:

```json
{
  "capabilities": { ... },
  "serverInfo": {
    "name": "protolsp",
    "version": "0.x.y",
    "rpc": {
      "address": "http://127.0.0.1:54321",
      "token":   "<64-char hex>"
    }
  }
}
```

Clients present `Authorization: Bearer <token>` on every request.
The token is 32 random bytes hex-encoded — sufficient against
drive-by connections from other local processes; no replay
protection since loopback-only. The address + token are stable
for the lifetime of the LSP session and regenerated on each
restart.

Editors that don't know about `rpc` ignore the extra key.

### Registry URI namespaces

`registry://<namespace>/<path>` URIs returned by go-to-definition
and consumed by `GetSourceContent` use two reserved namespaces in
addition to real registry namespace IDs:

| Namespace | Backed by | Use |
|---|---|---|
| `_proto_files_` | The workspace's `.protolsp.pxf` `proto_files { paths = [...] }` block. | Types defined in local `.proto` files alongside the `.pxf` workspace. |
| `_internal_` | Embedded shipped `.proto` descriptors compiled into the protolsp binary. | Types protolsp ships internally (e.g. `protolsp.v1.WorkspaceConfig`). |
| _any other_ | The configured protoregistry namespace. | Schema-as-a-service: registry-backed types fetched over the wire and cached on disk. |

Both reserved names use leading/trailing underscores so they're
distinguishable from real reverse-DNS namespace IDs while staying
URI-safe per RFC 3986 (parentheses in the original `(proto_files)`
were rejected by Monaco's URI parser).

## Transports

- **stdio** (`cmd/protolsp`): the universal LSP transport.
  Editor extensions spawn the binary as a child process.
- **WebSocket** (`cmd/protolsp-ws`): for browser-hosted Monaco
  clients. `--addr`, `--path`, `--origins`,
  `--insecure-skip-origin-verify`, `--max-message-bytes` flags;
  `/healthz` endpoint.
- **Connect-RPC sidecar** (bound at initialize): typed proto
  services covering the custom-method surface, advertised in
  `InitializeResult.serverInfo.rpc`. See
  [Connect-RPC services](#connect-rpc-services) above for the
  per-service method table and the discovery payload.

## Regenerating proto bindings

The proto files live under `proto/`. Go bindings (`*.pb.go`,
`rpcpbconnect/*.connect.go`) are committed alongside their sources;
regenerate after editing a `.proto`:

```sh
cd proto && buf generate
```

Prerequisites (one-time):

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
```

TypeScript stubs for clients like pxfed are NOT generated here —
each client owns its own codegen. See `pxfed/buf.gen.ts.yaml` +
`pnpm rpc:gen` for the pxfed-side pipeline.

## Configuration

protolsp resolves configuration through four tiers, highest
precedence first:

1. **Override** — session-scoped via `protolsp/setOverrides`. In-memory only.
2. **Settings** — Tier 3, pulled from the editor via standard `workspace/configuration` (or seeded once from `initializationOptions` for legacy clients — see below).
3. **File** — `.protolsp.pxf` checked into the repo.
4. **Default** — compiled into the binary.

See [`docs/design/config-protocol.md`](docs/design/config-protocol.md)
for the full model.

### `.protolsp.pxf` (recommended)

Commit a `.protolsp.pxf` to the workspace root (or any subtree —
nearest-ancestor wins, monorepos with per-service config work
out of the box):

```pxf
@type protolsp.v1.WorkspaceConfig

registry {
  address   = "registry.example.com:443"
  namespace = "acme-billing"
  token     = "${env.PROTOREGISTRY_TOKEN}"
  cache_dir = "${env.HOME}/.cache/protolsp"
}

chameleon {
  enabled    = true
  chain_spec = "chain.pxf"
}
```

`${env.X}` interpolation reads from the LSP process's environment
at parse time — keep secrets out of the committed file.

See [`docs/design/workspace-config.md`](docs/design/workspace-config.md)
for the full schema (registry, chameleon, schema_sources, security)
and the multi-root / sub-workspace discovery model.

### Editor settings (Tier 3)

Editors that implement `workspace/configuration` deliver settings
under the `protolsp.*` namespace. The VS Code extension wires
the standard `protolsp.registry.address`, `protolsp.registry.namespace`,
`protolsp.registry.token`, `protolsp.registry.cacheDir`,
`protolsp.security.inline_secrets` keys.

### `initializationOptions` (legacy seed)

For clients that don't implement `workspace/configuration`,
protolsp still accepts the legacy seed shape at `initialize`:

```json
{
  "registry": {
    "address":   "registry.example.com:443",
    "namespace": "acme-billing",
    "token":     "<optional bearer token>",
    "cacheDir":  "/path/to/cache"
  }
}
```

This path is **deprecated** — clients that supply registry config
via `initializationOptions` AND advertise `workspace/configuration`
capability receive a one-time `window/showMessage` nudge at
`initialized` to migrate. The seed will be removed in a future
release (see the migration plan in `docs/design/config-protocol.md`).

### Schema validation lifecycle

Empty `registry.address` (across all four tiers) disables schema
validation — protolsp degrades to parse-only diagnostics.

`cacheDir`, when set, enables the protoregistry client's on-disk
descriptor cache. Successful refreshes are persisted, and if the
registry is unreachable at startup the resolver falls back to the
last-known-good snapshot. Clients can read the live/stale state
via the `protolsp/registryStatus` custom request, and a warning
`window/showMessage` is sent at `initialized` time when the
resolver loaded stale.

## Design

- [`docs/design/protolsp-mvp.md`](docs/design/protolsp-mvp.md) — MVP scope and milestone-by-milestone phased delivery (M1–M6).
- [`docs/design/workspace-config.md`](docs/design/workspace-config.md) — `.protolsp.pxf` schema, multi-root discovery (W1–W6).
- [`docs/design/config-protocol.md`](docs/design/config-protocol.md) — four-tier resolution, `getEffectiveConfig`/`setOverrides` (CP1–CP5).
- [`docs/design/chameleon-integration.md`](docs/design/chameleon-integration.md) — chain support, layer classification, merged-value hover (C1–C7).
- [`docs/design/dataset-regions.md`](docs/design/dataset-regions.md) — `@dataset` detection, per-row validation, region-aware hover (R1–R6).
- [`docs/design/secret-handling.md`](docs/design/secret-handling.md) — `pxf.Secret` redaction, ranges request, inline-secrets policy (S1–S4).
- [`docs/design/incremental-sync.md`](docs/design/incremental-sync.md) — incremental `didChange`, position-encoding negotiation, analyze debouncing (I1–I3).
- [`docs/roadmap.md`](docs/roadmap.md) — open gaps from the 2026-05-18 audit, prioritized by tier.

## Status

v0.1.0 — initial public release. The LSP feature surface is stable;
custom LSP method shapes (`protolsp/sourceContent`,
`protolsp/listMessages`) may still evolve in 0.x. See
[`CHANGELOG.md`](CHANGELOG.md).

## License

MIT
