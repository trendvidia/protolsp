# protolsp MVP

| Field    | Value                                            |
|----------|--------------------------------------------------|
| Status   | Draft                                            |
| Date     | 2026-05-15                                       |
| Scope    | The protolsp Go service: scaffolding, document sync, diagnostics, hover. VS Code extension wiring as a separate follow-up. |
| Related  | [`../../../protoregistry/docs/design/namespace-hierarchy.md`](../../../protoregistry/docs/design/namespace-hierarchy.md) (sibling repo) |

## Summary

protolsp is the Go Language Server that gives developers schema-aware
authoring of `.pxf` files in any LSP-capable editor (VS Code, Neovim,
Zed, Helix) and in the hosted Monaco IDE. It consumes protoregistry's
schemas via the registry client SDK and emits LSP diagnostics, hover,
and (in later iterations) completion + code actions.

This document defines the MVP: smallest scope that delivers a credible
improvement over the existing syntax-only VS Code extension.

## In scope (MVP)

- **Transports**: stdio (editors) and WebSocket (Monaco / hosted IDE).
- **Document lifecycle**: `textDocument/didOpen`, `didChange`,
  `didClose`. In-memory document state per URI.
- **Parse diagnostics**: every `didChange` re-runs `pxf.Parse`; any
  parse error is published as a `Diagnostic` with line/column from
  the parser's `Position`.
- **Schema-aware diagnostics**: when a parsed document has a `@type`
  directive, resolve the FQN against protoregistry (via the client
  SDK with `WithServerChain`), then run `pxf.UnmarshalDescriptor` to
  catch unknown fields, type mismatches, missing-required, oneof
  violations, etc. Each validation error becomes a `Diagnostic`.
- **Hover**: at a cursor position, identify the AST node, walk to its
  field descriptor, render a hover message with the field name,
  protobuf type, originating file, and **namespace provenance**
  (which namespace in the chain contributed the type — uses the
  `*WithOrigin` resolver methods we landed in phase 2c).
- **Configuration**: a single per-workspace setting telling protolsp
  the registry endpoint (gRPC URL) and the default namespace.

## Out of MVP

These are obvious follow-ups; included here so the MVP boundary is
explicit:

- **Completion** (field-name + message-type + enum-value **landed
  2026-05-15**): field-name completion against the schema's
  `MessageDescriptor` with AST-aware nested-block context
  (brace-counted enclosure check); message-type completion on
  `@type` directives via the client SDK's `RangeMessages`; enum-value
  completion on the right-hand side of an assignment to an
  enum-typed field. `Document` caches `LastGoodParse` so completion
  keeps working through transient mid-edit parse failures (typing
  `field = ` produces a single broken line but the rest of the
  AST is intact). `CompletionProvider` advertised in `initialize`.
  Trigger by typing — no explicit trigger characters configured.
- **Code actions** (deprecation + unknown-field quick-fix **landed
  2026-05-15**): AST walk against the bound descriptor produces
  per-field findings — one Error diagnostic per unknown field
  (with `Data` payload identifying the field for quick-fix
  lookup) and one Hint diagnostic with `Tags: [Deprecated]` per
  use of a `[deprecated = true]` field. The code-action handler
  reads the Data payload and offers a "Remove field" quick-fix
  that deletes the offending line. `UnmarshalDescriptor` runs
  with `DiscardUnknown: true` so type-mismatch / required /
  oneof errors from the decoder don't duplicate the AST findings.
  `CodeActionProvider` advertised in `initialize` with
  `CodeActionKinds: [QuickFix]`. Rename refactors still
  outstanding (lower-value for pfx where each field appears once).
- **Go-to-definition** (**landed 2026-05-15**, registry-backed
  fallback landed 2026-05-15):
  `textDocument/definition` handler. Cursor on a field assignment,
  block name, or `@type` FQN resolves to the corresponding
  descriptor; returns a `Location` with the descriptor's
  `ParentFile().Path()` resolved to a URI plus a `Range` derived
  from `SourceLocations().ByDescriptor`. `DefinitionProvider: true`
  advertised. URI precedence: (1) `file://` against the workspace
  root when the file is on disk (working-copy semantics win);
  (2) `registry://<namespace>/<file-path>` when the resolver
  attributes the file to a tracked namespace — the editor's
  TextDocumentContentProvider for `registry:` calls the custom
  `protolsp/sourceContent` request to fetch bytes via
  `client.Resolver.GetSource`; (3) blind `file://` as the
  last-resort fallback so misconfiguration surfaces as the
  client's "file not found" rather than a silent drop. VS Code
  extension content-provider registration is the remaining piece
  (lives in protowire, not protolsp).
- **Document formatting**: `pxf format -w` integration.
- **Multi-document workspace symbols**: cross-file symbol search.
- **Round-trip compile**: pfx ↔ binary protowire commands exposed as
  LSP commands.
- **Authentication beyond the workspace token**: per-request scopes,
  user-context propagation.

## Tech stack

- **LSP framework**: `go.lsp.dev/protocol` + `go.lsp.dev/jsonrpc2`.
  Idiomatic for Go LSPs (used by gopls-adjacent tooling). Defines
  the LSP message types and the JSON-RPC dispatch. Alternative
  considered: rolling our own JSON-RPC + LSP types — rejected as
  unnecessary work for a known-stable upstream.
- **Parser**: `github.com/trendvidia/protowire-go/encoding/pxf`.
  Exposes `Parse(data) → *Document`, `UnmarshalDescriptor(data, desc)`
  for schema validation, and `Position` with line/column. Already
  battle-tested in the protowire CLI.
- **Schema resolution**: `github.com/trendvidia/protoregistry/client`.
  Constructed with `WithServerChain()` so the resolver walks the
  registry's authoritative hierarchy.
- **WebSocket transport**: `github.com/coder/websocket` (the
  community-maintained continuation of nhooyr.io/websocket after
  the upstream archive). Lighter and more idiomatic than
  `gorilla/websocket`; supports context cancellation cleanly.

## Architecture

```
┌──────────────────────────────────────┐
│   Editor (VS Code / Monaco / etc.)   │
└──────────────────┬───────────────────┘
                   │ LSP JSON-RPC (stdio or WS)
                   ▼
┌──────────────────────────────────────┐
│           protolsp server            │
│                                      │
│  ┌────────────────────────────────┐  │
│  │  Document store (uri → state)  │  │
│  │  - source bytes                │  │
│  │  - last parse result           │  │
│  │  - last validation result      │  │
│  └────────────────────────────────┘  │
│                                      │
│  ┌────────────┐  ┌────────────────┐  │
│  │ pxf.Parse  │  │ pxf.Unmarshal  │  │
│  └────────────┘  │ Descriptor     │  │
│                  └────────────────┘  │
│                                      │
│  ┌────────────────────────────────┐  │
│  │ protoregistry/client.Resolver  │  │
│  │       (WithServerChain)        │  │
│  └────────────────────────────────┘  │
└──────────────────┬───────────────────┘
                   │ gRPC
                   ▼
       ┌──────────────────────┐
       │   protoregistry      │
       └──────────────────────┘
```

## Package layout

The MVP shape sketched in early drafts of this doc described a
~6-file `internal/server` tree. The package has since grown
substantially as the post-MVP phases shipped (W*, C*, S*, R*,
CP*, I*); the current tree below is a high-level orientation —
run `ls internal/` for the canonical view.

```
protolsp/
  cmd/protolsp/         // stdio entrypoint (binary: protolsp)
  cmd/protolsp-ws/      // websocket entrypoint (binary: protolsp-ws)
  internal/
    server/             // LSP handler + per-feature dispatchers
                        // (hover, completion, definition, code action,
                        // chain-aware features, dataset regions,
                        // secret handling, config-protocol methods)
                        // — see Server's three composed managers
                        //   (lookupMgr, cfg, ws) for state shape
    transport/          // stdio + websocket LSP loops + framing limits
    schema/             // protoregistry client wrapper + internal lookup
    workspaceconfig/    // .protolsp.pxf parser
    chain/              // chameleon chain.pxf parser + resolve order
    positions/          // LSP position ↔ byte offset conversion (UTF-8 / UTF-16)
    interp/             // ${path} interpolation scanner
    redact/             // pxf.Secret redaction helpers
    safeio/             // bounded file reads
  proto/                // generated descriptors (WorkspaceConfig, ChainSpec)
  docs/                 // design docs + roadmap
  go.mod
  go.sum
```

## Document workflow

1. Editor sends `textDocument/didOpen` with URI + source.
2. Server stores the source in the document store keyed by URI.
3. Server invokes `pxf.Parse(source)`.
   - **Parse error** → publish a single diagnostic at the parser's
     reported position with severity `Error`. Stop here.
   - **Parse success** → continue to schema validation.
4. If the parsed `Document.TypeURL` is non-empty, resolve it through
   the client resolver:
   - Hit → run `pxf.UnmarshalDescriptor(source, descriptor)`.
     - On error: surface the error as a diagnostic (positions extracted
       from the error if possible; otherwise top-of-file).
     - On success: zero diagnostics from validation.
   - Miss → emit a single "unknown type `<FQN>`" diagnostic.
5. If `TypeURL` is empty, emit a single info-severity diagnostic
   "no `@type` directive; cannot validate against a schema" (or
   nothing — TBD; see Q1 below).
6. Cache the result; on `didChange`, repeat from step 2 with the new
   source.

## Hover workflow

1. Editor sends `textDocument/hover` with URI + position.
2. Server retrieves the cached parse result for the URI.
3. Walk the AST to find the node containing the position. For an MVP,
   this is: identify the field assignment under the cursor.
4. Look up the field's `FieldDescriptor` via the resolved message
   descriptor.
5. Use `Resolver.FindFileByPathWithOrigin(field.ParentFile().Path())`
   to determine the namespace that contributed the field's defining
   file.
6. Render a markdown hover message:
   - Field name + protobuf type
   - Originating file path
   - Originating namespace ("defined in `<namespace>`")
   - Doc comment from the proto source (if available via
     `FieldDescriptor.Options`)

## Configuration

Single workspace-level config block (JSON, sent in the LSP
`initialize` params under `initializationOptions`):

```json
{
  "registry": {
    "address": "registry.example.com:443",
    "namespace": "acme-billing",
    "token": "..."
  }
}
```

- `address` — gRPC endpoint of protoregistry.
- `namespace` — primary namespace for the workspace; the resolver
  is constructed bound to this namespace, and `WithServerChain`
  expands it.
- `token` — optional bearer token. In production this would come
  from a secret manager / OAuth flow; for MVP just accept it as a
  string in config.

Future enhancements (post-MVP): per-folder configs, OIDC token
refresh, allow overriding the namespace per-file via a directive.

## Phased delivery

Each phase is independently mergeable and leaves the tree green.

| Phase | Scope | Notes |
|-------|-------|-------|
| **M1** | Scaffolding: `go.mod`, `cmd/protolsp` stdio entrypoint, LSP `initialize`/`shutdown` handlers, empty document store. No diagnostics yet. Smoke-testable via `npx vscode-languageclient`-style harness. | ~200 LoC |
| **M2** | Document sync (`didOpen`/`didChange`/`didClose`) + parse diagnostics via `pxf.Parse`. Position conversion utility (1-indexed pxf → 0-indexed LSP). Per-document state caches the parsed AST for M4 to reuse. Server gains `AttachConn` so it can send server→client `publishDiagnostics` notifications. | ~300 LoC; landed 2026-05-15 |
| **M3** | New `internal/schema` package with `Lookup` interface + `Config` parsing + `Open` constructor wrapping `protoregistry/client` with `WithServerChain`. `dispatchInitialize` reads `initializationOptions.registry` (`{address, namespace, token}`) and constructs the resolver; failures degrade to parse-only diagnostics rather than crashing. `Server.Close` releases the resolver on shutdown. `analyze` runs parse → (if `@type` bound and a Lookup is configured) `pxf.UnmarshalDescriptor`; first error wins. `diagnosticFromError` helper unifies parse + schema diagnostic construction. Tests use a fake `Lookup` with a hand-built `Greeting` MessageType, covering valid, unknown-field, type-mismatch, unknown-type, no-`@type`, parse-failure-suppresses-schema, and no-Lookup-skips-validation cases. | ~600 LoC; landed 2026-05-15 |
| **M4** | `textDocument/hover` handler with line-match heuristic over the parsed AST (the parser emits start positions only, no end ranges, so column-precise hover is a follow-up). Resolves field name → `FieldDescriptor` against the bound schema, renders markdown with field name, type label (with `repeated`/`map<>` cardinality), defining file path, and the proto source's leading doc comment when available. Falls back to "Unknown field" markdown when the user types a name not on the message. Capability advertised via `HoverProvider: true` in initialize. Namespace provenance follow-up landed 2026-05-15 via protoregistry PR #11 (client SDK `*WithOrigin`) + protolsp PR #12: when the resolver attributes a file to a tracked namespace, hover renders `` Defined in `<path>` (namespace `<id>`) ``; empty origin (ad-hoc `WithParent`/`WithFallback` tiers) falls back to the bare path. | ~300 LoC; landed 2026-05-15 |
| **M5** | `internal/transport/websocket.go` implements `wsStream` (jsonrpc2.Stream adapter — one WS message per JSON-RPC frame, matching the LSP-over-WebSocket convention used by `vscode-languageclient/browser`) and `WebsocketHandler` (an `http.Handler` accepting upgrades, each spawning its own `server.Server`). New `cmd/protolsp-ws` binary with `--addr`, `--path`, `--origins`, `--insecure-skip-origin-verify`, `--max-message-bytes` flags, plus `/healthz`. Fixed an existing latent panic in the exit path: M1's `ErrServerClosed` return went through jsonrpc2's `fail()` which uses `atomic.Value.Store` — a subsequent Read failure with a different concrete error type panicked. Exit now closes the connection directly. Tests: full WS round-trip (Accept → upgrade → initialize → exit → clean close), binary-frame rejection, plain HTTP doesn't accidentally upgrade. | ~400 LoC; landed 2026-05-15 |
| **M6** | VS Code extension rewritten as a `vscode-languageclient` host that spawns the `protolsp` binary over stdio. Drops the in-extension `@trendvidia/protowire` parser; adds a `PXF Language Server` configuration section (`protolsp.path`, `protolsp.logLevel`, `protolsp.registry.{address,namespace,token}`) forwarded as LSP `initializationOptions`. Spawn failure surfaces a single `ErrorMessage` with an "Open Settings" action. Lands in `trendvidia/protowire` (where the extension lives), not protolsp. Per-platform `.vsix` bundles that ship the binary are deferred — users currently `go install` protolsp. | ~280 LoC in protowire; landed 2026-05-15 |

M1–M4 ship the headline value (schema-aware diagnostics + hover with
provenance). M5 unlocks Monaco. M6 makes the existing VS Code users
benefit.

## Open questions

1. **Position fidelity in `UnmarshalDescriptor` errors**: the decoder
   may return errors that lack a specific `Position`. For M3, map
   such errors to a synthetic position at the start of the document
   and include the error text in the diagnostic message. Better
   position extraction is a follow-up that may require parser-side
   work in `protowire-go`.
2. **Refresh behavior**: the client resolver has a background refresh
   loop. Should diagnostics auto-republish when the registry's
   current schemas change? For M3 the answer is no — diagnostics
   refresh on `didChange`. Later, we can wire a registry-side
   "schema updated" event to trigger republish across all open
   documents in that namespace.

## Resolved questions

- **Empty-`@type` behavior** (M3): silent. A `.pxf` file without an
  `@type` directive emits no info-severity diagnostic — the
  "validation skipped" message would have been noise on the dominant
  authoring path (new file, type not yet bound). Users discover
  schema validation via the registry-config docs / hover labels
  rather than a per-document banner. Revisit if anyone asks for
  the info diagnostic.

## Non-goals

protolsp deliberately does not try to be:

- **Not a generic protobuf editor**: protolsp is `.pxf`-only. Editing
  `.proto` sources is out of scope (Buf's `bufls` covers that).
- **Not a workspace-aware refactoring tool**: cross-file renames and
  symbol moves are off the table for MVP and probably v1.
- **Not a binary-format viewer**: visualizing serialized protowire
  bytes belongs in a separate tool, not the LSP.

## What this unblocks

Once M1–M5 are in:

- Hosted IDE (Monaco) can offer schema-aware authoring of agent tool
  schemas, workflow inputs, etc. — the platform's authoring layer is
  real.
- Any LSP-capable editor user gets the same benefit on desktop.
- Agent tool synthesis (future phase) can rely on the LSP for
  human-author validation of synthesized pfx fixtures.
