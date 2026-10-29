# Config protocol — editor ↔ server contract with explicit ownership

| Field    | Value                                                                                                                                                                                |
|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Status   | Draft                                                                                                                                                                                |
| Date     | 2026-05-16                                                                                                                                                                           |
| Scope    | Define the editor ↔ protolsp contract for configuration: where each value comes from, who owns each tier, how the editor reads effective config + provenance, how it pushes per-session overrides, and how it stays in sync with file edits. Adopts standard LSP `workspace/configuration` + `workspace/didChangeConfiguration`; adds three small custom methods for what LSP doesn't natively cover. |
| Out      | Cross-LSP-server config federation (one protolsp instance ↔ another). Cloud-synced editor settings (that's the editor's job). Schema-defining the entire editor settings surface (editors define their own settings UI; we only define the protocol contract). |

## Summary

protolsp has three places configuration can come from:

- Compiled-in defaults.
- A committed `.protolsp.pxf` file (per the
  [workspace-config](workspace-config.md) design).
- The editor — currently via a one-shot
  `initializationOptions` payload at `initialize` time.

What's missing is **ownership clarity** and **runtime
flexibility**. A user debugging "why is the LSP talking to the
wrong registry?" today has no way to inspect *which* tier
supplied the value they're seeing. A user wanting to switch the
active chameleon environment for a debugging session has no
clean path — initializationOptions is one-shot, and they don't
want to edit a committed file.

This doc defines a four-tier configuration model with explicit
per-tier ownership, adopts standard LSP `workspace/configuration`
machinery for the third tier, and adds a small custom-method
surface (three methods, one notification) for the pieces LSP
doesn't natively cover: **introspection** (where did this value
come from?) and **session overrides** (transient changes that
don't touch the file or the user's settings).

## Motivation

Three concrete failures of the current `.protolsp.pxf` +
initializationOptions model:

1. **No per-user / per-session state.** Auth tokens, personal
   cache paths, "use namespace `acme-staging` while I debug this"
   — none belong in a committed file (different per developer)
   and `initializationOptions` is fixed at startup. Editors that
   change a setting mid-session have nowhere to push the change.
2. **No provenance.** When the LSP picks `registry.example.com:443`,
   was that the file, the editor settings, a default, or an
   override? Today there's no way to tell. "Show me effective
   config" is a basic debugging affordance and it doesn't exist.
3. **No defined contract.** LSP has `workspace/configuration` and
   `workspace/didChangeConfiguration` for exactly this — but
   protolsp doesn't use them. Editors are left to "set
   initializationOptions and hope," and behavior diverges between
   VS Code, pxfed, Neovim, and anyone else integrating.

The fix is to name the tiers, define their ownership, adopt the
LSP standards that already exist, and add the narrow custom
surface for provenance + overrides.

## Goals

1. **Four-tier model with named owners**, precedence top-down,
   visible to the editor.
2. **Standard LSP first.** Use `workspace/configuration` /
   `workspace/didChangeConfiguration` /
   `workspace/didChangeWatchedFiles` wherever LSP already
   defines a method that fits. Custom methods only for what LSP
   doesn't cover.
3. **Provenance reporting.** Every effective config value
   carries its source tier so users can see where it came from.
4. **Transient overrides.** Editors can push per-session
   overrides that don't touch the file or the user's persistent
   settings. Cleared on LSP restart by design.
5. **Backward compatible.** Existing `initializationOptions`
   continues to work as a seed for Tier 3 (editor settings)
   during migration. No flag day.

## Non-goals

- **Cross-server federation.** protolsp instances don't talk to
  each other; each owns its own config.
- **Persistent override store.** Overrides live in memory until
  the LSP restarts. If users want a setting to persist, they put
  it in editor settings or the file.
- **Defining the editor's settings UI.** Editors are free to
  surface our config sections however they like (VS Code
  settings.json schema, pxfed form UI, etc.). The protocol just
  defines the contract.
- **A schema for the *editor-only* portion of the surface.**
  Settings like `protolsp.path` (where to find the binary) live
  in the editor and never reach the server. We define
  conventions for the section names but don't ship a proto.

## The four-tier model

| Tier | Source                        | Owner               | Lifetime             | Examples |
|------|-------------------------------|---------------------|----------------------|----------|
| 1    | Compiled-in defaults          | protolsp            | always               | `log_level = "info"`, default refresh interval |
| 2    | `.protolsp.pxf` file          | the **project** (committed) | persisted in git | `schema_sources[].registry.address`, `chameleon.chain_spec` |
| 3    | Editor settings (`workspace/configuration`) | the **user** | persisted by the editor | `protolsp.registry.token`, `protolsp.log_level` |
| 4    | Session overrides (`protolsp/setOverrides`) | the **session** | in-memory only | "switch chameleon active env to `sandbox`" for this debugging session |

**Precedence**: Tier 4 wins over Tier 3 wins over Tier 2 wins
over Tier 1. Each effective value carries a `source` field
naming its tier.

### Ownership boundaries

- **Tier 2 is the project's source of truth** for anything that
  should be shared across the team — schema source addresses,
  chameleon chain spec location, the schema FQN for typeless
  layers, log level the project considers reasonable. Anything
  in Tier 2 is reviewed via PR.
- **Tier 3 is the user's source of truth** for anything personal
  — auth tokens (vault-fetched, never committed), personal cache
  path, log-level override (debug while developing). The editor
  persists these; the LSP just consumes them.
- **Tier 4 is the session's source of truth** for anything
  ephemeral — debugging environment switch, ad-hoc namespace
  override. Cleared on restart by design; never written
  anywhere.

If a value could reasonably live in multiple tiers (e.g. log
level), the tier with the most-specific scope wins. A user who
wants verbose logging today doesn't have to edit the project's
committed file.

### Section names

Configuration sections use dotted-path notation rooted at
`protolsp.`:

- `protolsp.<dotted_field_path>` for fields that map to a
  `protolsp.v1.WorkspaceConfig` field. Example:
  `protolsp.schema_sources[0].registry.address`,
  `protolsp.chameleon.chain_spec`,
  `protolsp.log_level`.
- `protolsp.<editor_only_section>` for fields the server doesn't
  read (the editor owns them entirely). Example: `protolsp.path`
  (binary location). Documented for editor implementers but
  outside the server's effective-config response.
- `protolsp.<override_only_section>` for fields that only make
  sense as session overrides. Example:
  `protolsp.chameleon.active_env_override` (forces a specific
  chain's environment for this session regardless of file
  defaults). These are documented but absent from defaults / file
  schema.

Editors map these section names onto their own settings
machinery. VS Code's `settings.json` uses dot-paths natively;
JetBrains / Neovim plugins translate as needed.

## Standard LSP mechanisms (adopt now)

Three existing LSP methods cover most of Tier 3 (editor
settings) without any custom protocol surface.

### `workspace/configuration` (server → client request)

The server asks the client for current values when it needs
them. Sections requested are dotted-path identifiers; client
returns one value per section in the same order.

```
Server → Client:
  workspace/configuration {
    items: [
      { section: "protolsp.registry.token" },
      { section: "protolsp.log_level" }
    ]
  }

Client → Server:
  ["${VAULT_FETCHED}", "debug"]
```

The server pulls when:
- A document opens and the workspace's Tier 3 cache is cold.
- A `workspace/didChangeConfiguration` event invalidates the
  cache.
- An explicit `protolsp/getEffectiveConfig` request forces a
  refresh.

### `workspace/didChangeConfiguration` (client → server notification)

Client tells server "user changed a setting somewhere relevant."
The notification can be vague (no payload) or specific (which
sections changed). Server invalidates the corresponding Tier 3
cache entries; recomputes effective config; emits
`protolsp/effectiveConfigChanged` to the editor.

### `workspace/didChangeWatchedFiles` (already in workspace-config)

Registered with the pattern `**/.protolsp.pxf`. Fires on Tier 2
edits. Server reloads the file, recomputes effective config,
emits `protolsp/effectiveConfigChanged`.

These three mechanisms together cover everything except
**introspection** (which tier supplied which value) and
**transient overrides** (Tier 4). Those need the custom surface
below.

## Custom protolsp methods

Three methods, one notification. Narrow on purpose.

### Request: `protolsp/getEffectiveConfig`

```typescript
interface GetEffectiveConfigParams {
  workspaceDir?: string;  // omit for ambient (first workspace)
  sections?: string[];    // omit for "all"
}

interface ConfigValue {
  value: any;             // resolved value (string, number, object, etc.)
  source: "default" | "file" | "settings" | "override";
  // When source = "file" or "settings", the originating section name.
  // When source = "override", the override key.
  origin?: string;
}

interface GetEffectiveConfigResult {
  workspaceDir: string;
  values: { [section: string]: ConfigValue };
}
```

Returns the effective config with provenance. Editors render
this in a "Show Effective Config" command — sidebar panel,
status-bar tooltip, etc. — with each value tagged by tier.

Filtering by `sections` is for efficiency; omit when the editor
wants the whole picture.

### Request: `protolsp/setOverrides`

```typescript
interface SetOverridesParams {
  workspaceDir?: string;  // omit for ambient
  overrides: {
    // Section name → new value. `null` clears an existing override.
    [section: string]: any | null;
  };
}

interface SetOverridesResult {
  applied: {
    [section: string]: {
      previousSource: "default" | "file" | "settings" | "override";
      newValue: any;
    };
  };
}
```

Pushes Tier 4 overrides. Server:

1. Validates each section against the known config schema
   (rejects unknown sections with `InvalidParams`).
2. Stores the override in-memory keyed by workspace.
3. Recomputes effective config for affected sections.
4. Emits `protolsp/effectiveConfigChanged`.
5. Returns the applied set with `previousSource` so the editor
   can offer "Revert" actions per setting.

Setting an override to `null` clears it; the lower tiers take
back over.

### Notification: `protolsp/effectiveConfigChanged`

```typescript
interface EffectiveConfigChangedParams {
  workspaceDir: string;
  changes: {
    [section: string]: ConfigValue;
  };
}
```

Server emits when any effective value shifts — file edit,
settings change (after the editor sends
`didChangeConfiguration`), override set/cleared. Editors refresh
their status-bar widgets and any "Show Effective Config" UI
that's open.

Coalesce when multiple sections change in the same tick
(typically a single file edit that touches several values).
Don't fire when the values are byte-identical to the previous
snapshot — same-value notifications are noise.

## Sequence diagrams

### Editor startup

```
Editor → Server: initialize { initializationOptions: { registry: { ... } } }
                 // initializationOptions seeds Tier 3; server doesn't
                 // know yet whether `workspace/configuration` is
                 // supported.

Server → Editor: initialize result (with capabilities)

Editor → Server: initialized

Server → Editor: client/registerCapability
                 (register `workspace/didChangeConfiguration`,
                  register file watcher on **/.protolsp.pxf)

Server: discovers .protolsp.pxf at workspace root, parses it (Tier 2)
        compiles defaults (Tier 1)
        seeds Tier 3 from initializationOptions

Server → Editor: workspace/configuration { items: [...] }
                 (pulls current values for any Tier 3 sections it cares
                  about — overrides initializationOptions seed if the
                  client returns different values)

Editor → Server: [values...]

Server: effective config now computed; ready to serve requests
```

### User changes a setting mid-session

```
Editor → Server: workspace/didChangeConfiguration { settings: ... }
                 (notification — may or may not name specific sections)

Server → Editor: workspace/configuration { items: [<affected sections>] }
Editor → Server: [new values]

Server: recompute effective config; diff against previous
Server → Editor: protolsp/effectiveConfigChanged { changes: { ... } }
```

### User pushes a session override

```
Editor → Server: protolsp/setOverrides {
                   overrides: { "protolsp.chameleon.active_env_override": "sandbox" }
                 }

Server: validate, store, recompute
Server → Editor: setOverrides result { applied: { ... } }
Server → Editor: protolsp/effectiveConfigChanged { changes: { ... } }
```

### `.protolsp.pxf` saved

```
Editor → Server: workspace/didChangeWatchedFiles {
                   changes: [{ uri: ".../.protolsp.pxf", type: Changed }]
                 }

Server: reload file, recompute Tier 2
Server: diff effective config
Server → Editor: protolsp/effectiveConfigChanged { changes: { ... } }
```

## Provenance reporting in practice

`getEffectiveConfig` returns a map keyed by section name. For
the example workspace from the workspace-config doc:

```json
{
  "workspaceDir": "/Users/alice/proj",
  "values": {
    "protolsp.schema_sources[0].registry.address": {
      "value": "registry.example.com:443",
      "source": "file",
      "origin": "schema_sources[0].registry.address"
    },
    "protolsp.schema_sources[0].registry.token": {
      "value": "${VAULT_FETCHED}",
      "source": "settings",
      "origin": "protolsp.registry.token"
    },
    "protolsp.chameleon.chain_spec": {
      "value": "config/chain.pxf",
      "source": "file",
      "origin": "chameleon.chain_spec"
    },
    "protolsp.chameleon.active_env_override": {
      "value": "sandbox",
      "source": "override",
      "origin": "session"
    },
    "protolsp.log_level": {
      "value": "debug",
      "source": "settings",
      "origin": "protolsp.log_level"
    }
  }
}
```

Editors render a sidebar with one row per value, tier-colored
badges, and a "Revert override" button on rows where
`source = "override"`.

## Migration plan for `initializationOptions`

`initializationOptions` continues to work, with two
clarifications:

1. **At `initialize` time, server seeds Tier 3** from
   `initializationOptions.registry.*` and
   `initializationOptions.chameleon.*` (in the same shape we use
   today). This means a client that **doesn't** support
   `workspace/configuration` still gets the same behavior as
   today.
2. **At `initialized` time, server attempts
   `workspace/configuration`.** If the client returns values for
   the configured sections, those values **replace** the seed.
   If the client doesn't support the method (returns
   MethodNotFound), the seed sticks.

This means:
- **Existing VS Code extension users** keep working without any
  changes. They'll get the seed-only path until the extension
  adopts `workspace/configuration` (small change).
- **New editor integrations** (pxfed, future hosted Monaco IDE,
  Neovim plugin authors) skip the seed entirely and use the full
  protocol from day one.

Deprecation timeline:
- v0.2.0: this design ships. Both paths supported. Doc nudges
  editors to adopt `workspace/configuration`.
- v0.3.0 or later: `initializationOptions.registry.*` /
  `initializationOptions.chameleon.*` emit a one-time
  `window/showMessage` notice at initialize ("Configuration via
  initializationOptions is deprecated; the editor should adopt
  workspace/configuration"). Functionality unchanged.
- v1.0.0 (or v0.5.0+, whichever comes first): seed path removed.
  Editors must use the dynamic protocol.

## Cross-cuts

### With workspace-config.md

The `.protolsp.pxf` file IS Tier 2. The workspace-config doc's
"backward compatibility with initializationOptions" section gets
a small follow-up edit reframing initializationOptions as a Tier
3 seed (covered by the migration plan above).

### With chameleon-integration.md

"Switch the active environment for this debugging session"
becomes a `setOverrides` call:

```json
{ "overrides": { "protolsp.chameleon.active_env_override": "sandbox" } }
```

The chameleon integration's chain-aware features consult the
override before the file's default. Mention in the chameleon
doc; no protocol change there.

### With `protolsp/registryStatus`

The `registryStatus` request becomes a **convenience view** over
`getEffectiveConfig` — same data shape, narrower selection.
Keep it for backward compat (the VS Code extension already
calls it); document that `getEffectiveConfig` is the more
general path going forward.

Stale-mode (from `WithDiskCache`) lives on the resolver itself
and is reported via the configured schema source's status —
exposed through `registryStatus` and through
`getEffectiveConfig` under
`protolsp.schema_sources[N].registry.stale`. Same data, two
ways to access.

### With `pxf.Secret` redaction

Tier 3 values that contain secrets (e.g.
`protolsp.registry.token`) should be **redacted** in
`getEffectiveConfig` responses by default. Editors can ask for
the unredacted form via an explicit
`{ revealSecrets: true }` flag if the user takes a deliberate
action ("Show token"). Server logs always redact.

Section names that should redact-by-default: `*.token`,
`*.password`, `*.secret`, plus any field whose schema type is
`pxf.Secret`. The list lives in `internal/config` and is
checked when assembling the response.

## Phased delivery

| Phase | Scope | LoC est. |
|-------|-------|----------|
| **CP1** | Adopt standard `workspace/configuration` + `workspace/didChangeConfiguration`. Server pulls Tier 3 from the client at `initialized` and after change notifications. `initializationOptions` continues to seed Tier 3 when the client doesn't support the standard methods. | ~400 |
| **CP2** | `protolsp/getEffectiveConfig` + provenance reporting. Implements the four-tier resolution. Tier 4 always returns empty here (no overrides yet). | ~250 |
| **CP3** | `protolsp/setOverrides` + `protolsp/effectiveConfigChanged` notification. Override storage per workspace (in-memory map). | ~250 |
| **CP4** | Redaction in `getEffectiveConfig` responses; `revealSecrets` opt-in flag. Cross-references the secret-redaction work. | ~100 |
| **CP5** | Editor adoption: VS Code extension migrates from `initializationOptions` to `workspace/configuration`; pxfed builds the "Show Effective Config" sidebar; deprecation notice (`window/showMessage`) wired up. | ~200 in protolsp + ~150 in protowire ext + ~200 in pxfed |

CP1 alone closes most of the ownership ambiguity — the editor
becomes a real Tier 3 owner with bidirectional updates. CP2 +
CP3 give the editor the introspection and override surface that
fills the rest of the gap. CP4 is the secret-handling cross-cut.
CP5 is the rollout.

## Open questions

1. **Section naming for array indices.** Today's example uses
   `protolsp.schema_sources[0].registry.address`. JSON-path
   bracket syntax inside section names is readable but
   non-standard. Alternatives: dotted indices
   (`protolsp.schema_sources.0.registry.address`); section-name
   wildcards (`protolsp.schema_sources[].registry.address` for
   the whole array). **Lean**: bracket syntax for individual
   array members; the `[]` form returns the whole array.
2. **Override scope.** Per-workspace by default. Should there be
   a "global" tier above workspace overrides for cross-workspace
   debugging (e.g. "force debug logging across every workspace
   for this session")? Likely YAGNI for v1; revisit if anyone
   asks.
3. **Validation of override values against the WorkspaceConfig
   schema.** When a `setOverrides` call writes
   `protolsp.chameleon.chain_spec = "/path/to/missing.pxf"`,
   should the server validate path existence + parseability
   before applying? **Lean**: validate that the section name is
   known + the value's *type* matches the schema; defer
   *semantic* validation to the consumer of the value (so
   "missing file" surfaces as a normal diagnostic, not as a
   `setOverrides` rejection).
4. **`workspace/configuration` semantics on multi-root.** When
   the editor has multiple workspaces, the standard LSP method
   takes an optional `scopeUri` per item. We honor that —
   different workspaces can have different Tier 3 values. The
   editor's settings UI typically handles per-folder settings
   via the same mechanism, so this should align with editor
   expectations.
5. **What happens during initial settings pull failure.** If
   the client returns an error (or never responds) to
   `workspace/configuration`, the server should fall back to the
   initializationOptions seed and log a warning, not block
   `initialized`. Pin in tests.
6. **Effective-config push frequency.** A user typing in
   `.protolsp.pxf` could fire `effectiveConfigChanged` on every
   keystroke. Debounce by ~200 ms (matching the planned analyze
   debounce); coalesce same-tick changes.

## Decision log

| Date       | Decision                                                                                          | Why                                                                                          |
|------------|---------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| 2026-05-16 | Four-tier model (default / file / settings / override) with precedence override > settings > file > default | Names the owners; resolves the "who controls X" ambiguity that prompted this design.    |
| 2026-05-16 | Adopt standard `workspace/configuration` + `workspace/didChangeConfiguration` for Tier 3          | LSP standard exists; using it brings every editor along consistently.                        |
| 2026-05-16 | Three custom methods: `getEffectiveConfig`, `setOverrides`, `effectiveConfigChanged` notification | Narrow surface; covers exactly what LSP doesn't (provenance + transient overrides).          |
| 2026-05-16 | Section names = dotted paths rooted at `protolsp.`; mirror proto field paths where applicable     | Matches LSP's section-name conventions; lets the schema double as documentation.             |
| 2026-05-16 | Overrides in-memory only; cleared on LSP restart                                                  | Forces a clear "this is transient" mental model; users wanting persistence use Tier 2 or 3.  |
| 2026-05-16 | `initializationOptions` continues as a Tier 3 seed; deprecated over two minor releases            | Backward compatible; deprecation timeline visible to integrators.                            |
| 2026-05-16 | `registryStatus` becomes a convenience view; kept for backward compat                             | Existing extension callers don't break; `getEffectiveConfig` is the canonical path going forward. |
| 2026-05-16 | Secret-typed values redacted by default in responses; explicit `revealSecrets: true` opts in      | Defense-in-depth alongside the secret-redaction work; wire / log exposure stays closed.      |
| 2026-05-18 | Keep `params.RootURI` accepted as a single-root fallback; drop in the same release that removes the `initializationOptions` seed path | LSP spec asks servers to keep accepting `RootURI` from pre-3.6 clients. Both paths target the same "old client" cohort, so removing them together avoids two breaking changes for the same audience. Static-check warnings on the field are silenced with a documented `//nolint:staticcheck` at the call site. |

## Notes for future readers

- **The custom-method surface is narrow on purpose.** Three
  methods plus one notification. If you're tempted to add a
  fourth, ask whether the use case fits into the four-tier
  model first. The whole point of this design is that ownership
  is *fully* expressible in tiers + sections; anything else is
  scope creep.
- **Don't grow Tier 4 into a backup persistence layer.**
  Overrides exist for ephemeral state. Users keep wanting to
  persist them; the right answer is "put it in Tier 3 (editor
  settings)" — which is durable, user-owned, and already in the
  protocol. Resist requests to add an `--persist-overrides`
  flag; that's just Tier 3 with extra steps.
- **Provenance is the under-appreciated feature.** Once editors
  expose the tier badge in their UI, support burden drops
  noticeably — "why is it using registry X?" answers itself.
  Push editor implementations to surface it prominently.
- **The deprecation of `initializationOptions` is real but
  patient.** Two minor releases minimum. Don't shortcut it
  because new code is cleaner without the seed path; existing
  integrations break, and we don't have many integrations to
  break in the first place — make the experience smooth for
  whoever's there.
