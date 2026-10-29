# Chameleon integration — chain-aware authoring for layered config

| Field    | Value                                                                                                                                                                                |
|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Status   | Draft                                                                                                                                                                                |
| Date     | 2026-05-16                                                                                                                                                                           |
| Scope    | Make protolsp aware that a `.pxf` file is a **chameleon layer** when the workspace declares a `chain.pxf`. Use that context to weight diagnostics, surface merged values, complete `${field.path}` interpolations, and render the active chain in the editor — without protolsp doing any runtime work chameleon should be doing. |
| Out      | Running chameleon's resolver, sops decryption, memguard handling, or the env-var source at edit time. Coupling protolsp's release cadence to chameleon's. Chain specs nested under nested chain specs (one chain per workspace is enough). Editing `.pxf` files outside any declared layer pattern (those keep today's standalone behavior). |

## Summary

Today, protolsp treats every `.pxf` file the same: parse it, validate
it against the `@type` directive's schema, publish diagnostics. That
model is the right one for standalone documents. It is **wrong** for
files that participate in a [chameleon](https://github.com/trendvidia/chameleon)
chain. `env/dev.pxf` legitimately leaves most fields unset because
`base.pxf` (or `env/sandbox.pxf`, or `env/prod.pxf`) sets them; today
the LSP flags those as "required field missing." `base_url = "${other.url}"`
is a valid interpolation; today the LSP can't complete or validate the
reference. Editing a chain becomes a steady stream of false-positive
errors and unaided typing.

This design pulls the chain into the LSP's context. When protolsp
detects a `chain.pxf` (pointed at one via `.protolsp.pxf`'s
`chameleon { chain_spec = ... }` block),
it parses the spec, builds the environment ancestry, identifies which
files are layers of which environments, and uses that information for:

- **Chain-aware diagnostic severity** — required fields are only
  errors if no layer in the chain sets them.
- **`${field.path}` interpolation support** — completion, go-to-
  definition, cycle detection at edit time, matching the runtime
  semantics.
- **Merged-value hover** — show the user the value as it will resolve
  at runtime, plus which layer contributed it.
- **Chain-aware completion** — completing field paths against the
  resolved message type, recognizing that ancestor layers may have
  introduced fields the current file inherits.
- **Annotation surfacing** — `pxf.default`, `pxf.required`,
  `pxf.Secret` get explicit treatment in hover and diagnostics with
  chameleon-specific framing.

The integration is **strictly editor-time**. protolsp never decrypts
a sops layer, never reads an env var, never runs the merge pipeline
in any sense that would matter at runtime. Those are chameleon's
job and stay chameleon's job. The LSP consumes the chain spec as
metadata.

## Motivation

Three concrete failures of the current model on a chameleon project:

1. **False errors on layer files.** `pxf.required(db_password)` set
   at the schema level → today's protolsp errors on every layer that
   doesn't explicitly set it. Realistic projects have many such
   fields; the layer files become red squiggles end-to-end.
2. **Unaided interpolation.** `api_url = "https://${base_url}/v1"`
   is a runtime feature with no edit-time support. Mistyping the
   field path produces a runtime resolve error you only discover
   in production.
3. **No visibility into resolution.** Looking at `env/dev.pxf` in
   isolation tells you what that layer overrides, not what the
   resolved config actually looks like. Debugging "why is this
   field's value still X in dev" requires running the resolver and
   inspecting; the editor offers nothing.

The chain spec already encodes everything we need to fix all three:
ancestry, layer ordering, layer file paths, context keys. We just
have to read it.

## Goals

1. **Chain visibility.** When a chain is configured, the LSP knows
   which environment a file belongs to and what its ancestors are.
   Exposed via a `protolsp/chainStatus` custom request.
2. **Chain-aware diagnostic weighting.** `pxf.required` fields demote
   from Error to nothing when an ancestor layer sets them. Other
   diagnostic kinds (type mismatch, unknown field) are unaffected —
   those remain per-file errors.
3. **Interpolation support.** `${field.path}` references get
   completion, go-to-definition, and unresolvable-reference / cycle
   diagnostics. Resolution semantics match chameleon's runtime.
4. **Merged-value hover.** Hover on a field surfaces its locally-set
   value AND its merged value across the chain, with a one-line
   "set by: env/sandbox.pxf" hint.
5. **Zero runtime coupling.** No sops, no memguard, no env-var
   reads, no actual decryption. The LSP treats encrypted layers
   as opaque and never executes runtime hooks.

## Non-goals

- **Running chameleon at edit time.** Even a sandboxed in-process
  call to `chameleon.Load[...]` is out — wrong threat model for an
  editor.
- **Schema discovery from the chain spec.** chameleon's `ChainSpec`
  doesn't declare which proto type the layers conform to — that's
  set by the layers themselves via `@type`. We don't try to invent
  a schema-binding field in `ChainSpec`; we read `@type` from the
  layers, same as standalone .pxf.
- **Encrypted-layer participation.** sops-encrypted layers are
  opaque bytes to protolsp. We surface `layerInfo.encrypted = true`
  and degrade to no diagnostics on the file; the editor renders
  whatever badge / tab decoration it prefers from that signal.
  Decryption is the runtime's job and must stay there.
- **Multi-chain workspaces.** Handled by the per-worktree
  `.protolsp.pxf` resolution from
  [`workspace-config.md`](workspace-config.md). A monorepo with
  multiple chameleon apps places one `.protolsp.pxf` per
  app-subtree; each declares its own `chameleon { chain_spec = ... }`
  block; the nearest-ancestor walk routes each document to the
  right chain. No protolsp-side multi-chain management code.
- **Renaming `pxf.required` / `pxf.default` semantics.** chameleon's
  runtime is the source of truth for what those annotations mean;
  the LSP mirrors them, doesn't redefine them.

## Concept: the active chain

When protolsp detects a chain, it computes a small data structure
that drives everything else:

```go
type ActiveChain struct {
    Spec         *chameleonv1.ChainSpec       // parsed spec, untouched
    Environments map[string]EnvironmentInfo   // declared env → ancestry
    Layers       []LayerInstance              // expanded layer list
    Errors       []ChainDiagnostic            // load-time issues (bad templates, etc.)
}

type EnvironmentInfo struct {
    Name      string
    Ancestors []string // root-first; "prod" extends nothing → ["prod"]
}

type LayerInstance struct {
    Source       string  // ChainSpec source name
    PathTemplate string  // unsubstituted: "env/${env}.pxf"
    ResolvedPath string  // after env+context substitution: "env/dev.pxf"
    Environment string   // which env (for PER_ENV) or "" (for ONCE)
    Strategy    string   // "ONCE" | "PER_ENV"
    Encrypted   bool     // sops-stream prefix detected; protolsp won't read body
}
```

Built once when the chain loads; rebuilt when `chain.pxf` itself
changes (we watch it). Every subsequent feature — diagnostics,
completion, hover — reads from this structure.

## Architecture

### Where the integration code lives

A new package `internal/chain` inside protolsp. **Imports the
`chameleon/v1` proto types via the registry** (so we get
`ChainSpec` as `dynamicpb.Message`) — but does NOT import the
chameleon Go module. Chain expansion is small enough (~100 LoC)
that reimplementing it here avoids version-coupling the LSP to
chameleon's runtime API.

Tradeoff: if chameleon later refactors its expansion semantics
(e.g. introduces new strategy kinds), protolsp falls behind until
its own expansion catches up. Acceptable for now — strategies are
on the slow-change list. If/when chameleon ships a
`chameleon/spec` subpackage (pure parsing + expansion, no
runtime), reconsider; until then we own a small parallel
implementation.

### Discovery

Chameleon configuration lives inside the workspace's
`.protolsp.pxf` (see [`workspace-config.md`](workspace-config.md))
under a `chameleon { ... }` block. The chain spec is resolved
once per workspace by the nearest-ancestor walk that already
governs schema-source discovery — no separate convention
scanning.

Order of operations per document:

1. Identify the document's workspace (nearest-ancestor
   `.protolsp.pxf`).
2. If the workspace's config declares a `chameleon` block with
   a non-empty `chain_spec`, load that path (relative to the
   `.protolsp.pxf`'s directory).
3. If the workspace has no `chameleon` block, chain mode is off
   for documents under it — they behave as standalone files.

When chain_spec resolves to a file, parse it as a
`chameleon.v1.ChainSpec`. The workspace's resolver must know
that type — chameleon's protos need to be published to one of
the workspace's schema sources (registry namespace or local
`proto_files` path) just like any other schema. If the resolver
doesn't know `chameleon.v1.ChainSpec`, emit a
`window/showMessage` warning and proceed in standalone mode.

### Layer detection

For an open file, "is it a layer?":

1. Compute the file's path relative to the workspace root.
2. For each `LayerInstance.ResolvedPath`, check exact match.
3. If matched, the file's `LayerInstance` identifies its
   environment, source, and strategy.
4. If unmatched, the file is **standalone** — chain-aware behavior
   is skipped for it. Today's behavior applies.

Detection runs at `didOpen` and is cached on the document.

### Configuration block

Inside `.protolsp.pxf`:

```pxf
@type protolsp.v1.WorkspaceConfig

schema_sources {
  registry {
    address   = "registry.example.com:443"
    namespace = "acme-myapp"
  }
}

chameleon {
  chain_spec = "config/chain.pxf"
  schema_fqn = "myapp.v1.AppConfig"
  # enabled defaults to true when chain_spec is set; set to false
  # to suppress chain-aware behaviors even when a chain.pxf exists.
}
```

`schema_fqn`, when set, is the assumed message type for every
layer that doesn't carry its own `@type` directive. Chameleon
layer files in the wild often omit `@type` (the runtime knows the
target), so this is the practical default for many projects.

## Protocol surface

Two custom methods plus one notification, all namespaced under
`protolsp/`.

### Request: `protolsp/chainStatus`

```typescript
interface ChainStatusResult {
  configured: boolean;
  chainSpecPath?: string;
  environments: { name: string; ancestors: string[] }[];
  schemaFQN?: string;
  errors: { range?: Range; message: string }[];
}
```

Editors render this in a status-bar element. Empty `environments`
+ no `errors` = "chain mode off". Populated `errors` = "chain
configured but broken" with details for the user.

### Request: `protolsp/layerInfo`

```typescript
interface LayerInfoParams { textDocument: TextDocumentIdentifier; }
interface LayerInfoResult {
  isLayer: boolean;
  environment?: string;     // when isLayer == true
  ancestors?: string[];     // root-first
  strategy?: "ONCE" | "PER_ENV";
  source?: string;          // ChainSpec source name
  encrypted: boolean;       // protolsp won't read encrypted layer bodies
}
```

Editors decorate the file (status bar, breadcrumb) with "Layer:
env/dev.pxf · extends env/sandbox.pxf, base.pxf". Cheap pull;
re-fetched after the editor observes a `chain.pxf` change (see
the notification design note below).

### Notification: `protolsp/chainStatusChanged` (not shipped)

The original design sketched a push notification fired when
`chain.pxf` reloaded so editors could refresh their status-bar
element automatically. That notification was never implemented —
the live-reload path on the server already re-runs the analyze
pipeline for every open document (which republishes
diagnostics), and the workspace-config tier's
`effectiveConfigChanged` notification (CP3) covers the broader
"config changed, refetch" channel that status-bar consumers
already subscribe to. Editors needing chain-specific change
events can poll `chainStatus` after observing
`workspace/didChangeWatchedFiles` on `chain.pxf`. Re-introduce
the notification if a real consumer asks for it.

### Why two methods instead of one

`chainStatus` answers "what's the chain look like?"; `layerInfo`
answers "where am I in it?". Single methods that mix both would
return more data per call than needed (layers are tens of files;
re-sending the whole map on every per-file query is wasteful).

## Chain-aware behaviors

### 1. Required-field weighting

For every `pxf.required(field)` annotation on the bound message
type, the analyze pass walks the current file's
**ancestor layers** (in chain order) and asks: does any of them
set this field?

- **No layer sets it** → emit Error on the current file at its
  `@type` directive (or document start if none). Message:
  "required field `db_password` is not set in any layer of the
  `dev` chain (base.pxf, env/sandbox.pxf, env/dev.pxf)".
- **An ancestor sets it** → no diagnostic. The user's view of
  what they're authoring is correct — they don't have to set the
  field because someone else did.
- **The current file sets it** → no diagnostic (trivial case).

Only the leaf layer carries the error. Other diagnostics (type
mismatch on a *value* the user did type, unknown-field, etc.) are
per-file and remain unchanged.

### 2. `${field.path}` interpolation

Recognized in scalar string values at edit time. Three behaviors:

- **Completion**: cursor immediately after `${` → suggest every
  reachable dotted field path on the bound message. Pulls from the
  schema (same RangeMessages-style walk we already do, scoped to
  field paths instead of message FQNs).
- **Go-to-definition**: cursor on a `${path}` reference → jump to
  the layer in the chain that most recently sets that path. Falls
  back to the schema's definition if no layer sets it.
- **Diagnostics**: unresolvable references emit Error
  ("reference `${nope.field}` not declared in schema"). Cycles
  (detected with the same algorithm chameleon uses at runtime)
  emit Error on every node of the cycle, naming the path.

The diagnostics for both syntax-level issues live alongside the
existing schema-validation diagnostics; same source tag,
`protolsp/schema`.

### 3. Merged-value hover

Hover on a field name shows the existing markdown plus a "Chain
resolution" section:

```
Defined in `myapp/v1/app_config.proto` (namespace `acme-myapp`)

Chain resolution:
  base.pxf:           "mybigproject.com"
  env/sandbox.pxf:    "stg.mybigproject.com"
  env/dev.pxf:        "dev.mybigproject.com"      ← effective

Annotations:
  default:            "fallback.example.com"
  required:           false
  interpolated by:    api_url
```

The "Chain resolution" rows come from a per-layer parse of the
ancestors. Cached per layer; invalidated when any of the layers
change.

When a field is `pxf.Secret`, the value column is redacted (see
the secret-redaction design notes); the rows still show which
layer set the secret, just not the bytes.

### 4. Annotation surfacing

Three annotations have explicit treatment:

- **`pxf.default`**: hover surfaces the default value. Diagnostic
  Hint when a layer redundantly sets the same value the default
  would have provided ("matches default; consider removing").
- **`pxf.required`**: drives §1 above.
- **`pxf.Secret`**: drives the redaction work tracked in the
  secret-redaction design doc; values are masked across all
  chain-aware features.

## Subtleties

### Schema availability gates everything

Chain-aware features all assume the bound message type's
descriptor is in the resolver. If schema validation is off
(empty registry config, or the registry is unreachable and there's
no on-disk cache), chain mode degrades gracefully:

- `chainStatus` still returns the spec (parse-only).
- `layerInfo` still classifies files.
- Diagnostic weighting, interpolation completion, merged-value
  hover — all skipped.

A `window/showMessage` warning at `initialized` time notes the
degradation so users understand why the editor is quieter.

### sops-encrypted layers

Detection: leading bytes match the sops JSON / YAML marker (a
small content sniff). Encrypted layers:

- Are excluded from the ancestor-walk in §1 (their content is
  unknown to the LSP).
- Show as `encrypted: true` in `layerInfo`.
- Don't generate per-file diagnostics — they're opaque.
- Emit **no LSP-side notification**. The editor reads
  `layerInfo.encrypted` and renders its own affordance —
  pxfed paints a lock badge on the tab; VS Code surfaces a
  similar decoration via a file-icon overlay. This keeps the
  notice from being a popup that interrupts every file open and
  pushes the UX responsibility to where it belongs.

Important: protolsp **never** invokes sops itself. Decryption is
a runtime concern with key-management implications; running it at
edit time would surface plaintext into editor processes, swap, log
files, etc. Hard line.

### Env-var source

A `kind = "env"` source emits values from process env vars at
resolve time. At edit time those values don't exist. protolsp:

- Marks env-sourced fields with a hover hint: "supplied by env
  var `MYAPP_DB_PASSWORD` at runtime".
- Does NOT include env values in the chain-resolution display
  (we can't observe them; pretending we can is misleading).
- Required-field weighting treats an env source as "sets the
  field" — the runtime will populate it, so the LSP shouldn't
  error on it.

Determining the env var name per field uses chameleon's
discovery rules (dotted path → `UPPER_SNAKE` + source prefix). We
reimplement the rule; chameleon's #78 PR describes it precisely.

### PER_ENV layer expansion

A single chain entry `{ path: "env/${env}.pxf", strategy: PER_ENV }`
materializes as one `LayerInstance` per ancestor. Chain expansion
in `internal/chain` mirrors chameleon's expand:

- Walk ancestors root-first (`prod`, `sandbox`, `dev`).
- For each, substitute `${env}` (and any context keys) and emit a
  `LayerInstance` with `environment` set.

`ONCE` strategy emits a single `LayerInstance` with `environment = ""`.

### Template context keys

Layer templates like `region/${ctx.region}.pxf` depend on the
caller-supplied context. The LSP doesn't know what context a
production resolve would receive. Two reasonable behaviors:

1. **Treat all context-keyed layers as optional**: don't include
   them in the ancestor walk for required weighting. Their effect
   varies per resolve; no edit-time guarantee.
2. **Default context inference**: read `default_value` from
   `context_keys` declarations in `ChainSpec`; substitute those.

Lean: **(2) when defaults exist, otherwise (1)**. Surface the
chosen context in `chainStatus` so the user knows what was
assumed.

### chain.pxf editing

The chain spec is itself a `.pxf` document. Editing it triggers a
`chain.pxf` reload (either via `workspace/didChangeWatchedFiles`
on save or the live-reload-from-edit path before save) and
republished diagnostics across every open layer document. The
`chainStatusChanged` notification originally sketched here was
never implemented — see the §"Custom protolsp methods" note. Editors
poll `chainStatus` after observing the file change if they need
chain-summary refresh.

### Workspace without a chain

No `chameleon { ... }` block in `.protolsp.pxf` (or
`enabled = false` to suppress an existing one) → protolsp
behaves exactly as today. The integration is invisible to anyone
not using chameleon.

## Phased delivery

| Phase | Scope | LoC est. |
|-------|-------|----------|
| **C1** | Chain discovery + parsing. `internal/chain` package: `Load`, `Expand`, basic types. `protolsp/chainStatus` request. No diagnostic-shape changes yet — visibility only. | ~400 |
| **C2** | Per-file `protolsp/layerInfo` request. Layer detection cached on `Document`. Still no diagnostic changes. | ~150 |
| **C3** | Chain-aware `pxf.required` weighting. The first user-visible behavior change. Tests pin the demote-to-no-error case across `ONCE` and `PER_ENV` strategies. | ~300 |
| **C4** | `${field.path}` recognition + completion + diagnostics for unresolvable / cyclic references. | ~400 |
| **C5** | `${field.path}` go-to-definition (jump to the deepest layer setting it; fall back to schema). Builds on the C4 detection. | ~150 |
| **C6** | Merged-value hover (§3). Per-layer value caches with invalidation. | ~250 |
| **C7** | `pxf.default` redundancy hint; env-source field hints. Lower priority but high signal-to-noise. | ~150 |

Each phase is independently demoable. C1 + C2 unlock the
"editor knows about the chain" experience for free; C3 is the
first false-positive killer; C4–C5 are the productivity boost;
C6–C7 are polish.

Roadmap ordering note: ship secret redaction and the editor-side
`pxf.Secret` masking (from the prior design) **before** C6, so
merged-value hover inherits redaction without special-casing.

## Open questions

(All six initial open questions were resolved during the design
review — see the decision log for the outcomes. New ones that
emerge during implementation will land here.)

## Decision log

| Date       | Decision                                                                | Why                                                                                          |
|------------|-------------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| 2026-05-16 | Editor-time integration only; no runtime coupling                       | Different threat models, different cadences; protolsp shouldn't decrypt or read env at edit. |
| 2026-05-16 | Reimplement chain expansion in protolsp                                 | Avoid version-coupling; expansion is small. Revisit if chameleon ships a pure-spec subpackage.|
| 2026-05-16 | Two custom methods (`chainStatus` + `layerInfo`) over one combined      | Separates "what's the chain" from "where am I in it"; cheaper per-file queries.              |
| 2026-05-16 | sops-encrypted layers are opaque; degrade gracefully                    | Decryption at edit-time leaks plaintext into editor process; out of scope.                   |
| 2026-05-16 | Required-field error lives on the leaf layer only                       | Where the user is; surfacing on every ancestor file is noisy.                                |
| 2026-05-16 | Schema discovery: `@type` per-layer first, `schema_fqn` from `.protolsp.pxf` fallback | Matches existing convention; lets chain-aware mode work for both styles of layer file. |
| 2026-05-16 | Chain-blind row validation for `@dataset` blocks inside chain layers    | Chameleon projects don't use `@dataset` in practice; if it ever happens, rows validate per the dataset-regions design independent of chain context. |
| 2026-05-16 | Env-source hover hint only on detected env-sourced fields, single line  | Noiseless when not env-sourced; strictly descriptive ("supplied by env var X at runtime").   |
| 2026-05-16 | No LSP-side notice for encrypted layers; editors render their own badge from `layerInfo.encrypted` | Avoids popup-per-file; UI responsibility belongs to the editor. |
| 2026-05-16 | Reimplemented chain expansion targets ChainSpec v1 / chameleon v0.17, version reported via `chainStatus` | ChainSpec + expansion are stable; targeted version lets users align protolsp ↔ chameleon. |
| 2026-05-16 | Multi-chain workspaces handled via per-worktree `.protolsp.pxf` nearest-ancestor walk | No protolsp-side multi-chain management; falls out of the workspace-config design.    |
| 2026-05-16 | Chameleon configuration lives in `.protolsp.pxf`, not `initializationOptions` | Aligns with the workspace-config design; one source of truth per worktree, committed to git. |

## Notes for future readers

- This is a thin awareness layer, not a runtime reimplementation.
  Resist the temptation to add features that require running the
  full resolver pipeline at edit time — every such feature
  introduces production-only behaviors into the editor.
- The `internal/chain` package's API surface should be small:
  `Load`, `Expand`, `LayerFor(path)`, a `Resolve(field)` helper
  for hover. Keep it boring.
- When chameleon's `ChainSpec` grows new strategy kinds or
  source types, protolsp may need to extend its expansion in
  lockstep. Treat that as expected ongoing maintenance, not a
  protocol break.
- Cross-cuts with `pxf.Secret` redaction and `@dataset` regions
  are documented in their respective design docs. Anything that
  surfaces values through hover / diagnostics should consult both
  redaction and dataset-region status before formatting.
