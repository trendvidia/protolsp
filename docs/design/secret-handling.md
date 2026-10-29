# Secret handling — `pxf.Secret` redaction across protolsp

| Field    | Value                                                                                                                                                                                |
|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Status   | Draft                                                                                                                                                                                |
| Date     | 2026-05-16                                                                                                                                                                           |
| Scope    | Stop protolsp from leaking `pxf.Secret` values through diagnostic messages, hover text, server logs, and the wire. Add an LSP-protocol surface that lets editors mask secret values visually without round-tripping the bytes. Provide authoring guidance that nudges literal secrets toward vault references. |
| Out      | Encrypting secrets at rest (chameleon's sops layer handles that). Sealing values in OS-level memory enclaves (chameleon does that runtime-side via memguard). Replacing or reimagining the `pxf.Secret` schema type (that's protowire's). Per-byte source-file scrubbing (would break source-positioned hover / definition / completion). |

## Summary

`pxf.Secret` is a built-in message type in protowire with two
surface forms — scalar shorthand (`pw = "x"`) and block
(`pw { value = "x"; hint = "..."; fingerprint = "..." }`). The
upstream pxf parser already supports an `OnSecretField(path, value)`
hook in its `UnmarshalOptions`: when set, secret values route
through the callback at decode time and **never land in
`Secret.value` on the proto message**. The hook implementation can
seal / drop / encrypt the value, closing the plaintext-in-heap
window for the proto layer.

protolsp doesn't use that hook today. As a result, secret values
land in heap memory (proto field), can be echoed back through
diagnostic messages on the wire, may appear in `slog` output, and
have no editor-side rendering distinction from any other string.
"Open a pxf file with a secret in it, hit a type error on that
line, and your password ends up in the LSP's `publishDiagnostics`
payload" is the failure mode.

This design fixes it in three layers, ordered by priority:

1. **In-process redaction at the emit boundary** — wire
   `OnSecretField` into protolsp's decoder, redact value text
   from any LSP message we send, suppress logging.
2. **Editor-side signaling** — a small custom LSP method
   (`protolsp/secretRanges` + change notification) returning the
   source ranges of secret values so editors can mask them with
   their own decorators.
3. **Authoring guidance** — warning diagnostics on literal
   scalar secrets and a quick-fix code action that rewrites them
   as references (vault, env, etc.) when grammar / chameleon
   sources allow.

The three layers are independently shippable. Layer 1 is the
must-have correctness work; layers 2 and 3 are user-visible
polish.

## Motivation

Today's failure modes, in increasing order of severity:

1. **Logs.** `slog` calls in the analyze / publish paths can
   include source-positioned error context. A `pxf.Secret` value
   inside a type-mismatch range would be in that context. Logs
   may be shipped to a remote sink (datadog, cloudwatch); now the
   secret is at rest in a third-party system.
2. **Diagnostic messages.** Schema-validation errors from
   `pxf.UnmarshalDescriptor` describe the offending bytes
   ("invalid value `xyz` for field `password`"). The full
   `publishDiagnostics` payload is sent to the client over the
   LSP transport. For pxfed / VS Code over stdio that's local;
   for protolsp-ws / hosted Monaco it crosses the network.
3. **In-memory.** The pxf parser populates `Secret.value` unless
   `OnSecretField` is set. The resulting proto message sits in
   the Server's parsed-document cache for the document's
   lifetime. If the process is core-dumped or attached by a
   debugger, the plaintext is in the dump.
4. **Source bytes.** `Document.Source []byte` holds the entire
   file — including raw secret values — for the lifetime of the
   open document. We can't fix this without breaking
   source-positioned features (hover, completion, definition);
   accepted residual risk, documented.
5. **No editor-side cue.** Even when the value isn't echoed,
   the editor renders it as a plain string. A user pair-
   programming over a screen share has no indication that the
   string under their cursor is sensitive.

Failures (1)–(3) are correctness issues — protolsp shouldn't
hold or transmit secret bytes any longer than necessary.
Failure (5) is a UX issue — once we know which ranges are
sensitive, surfacing that to the editor is cheap.

## Goals

1. **No plaintext in the proto message.** Wire `OnSecretField`
   so `Secret.value` is never populated after decode.
2. **No plaintext on the wire.** Diagnostic messages, hover
   markdown, completion details, code-action previews — every
   LSP payload the server emits redacts secret-value byte ranges
   before sending.
3. **No plaintext in logs.** `slog` paths consult a redactor
   before formatting any source-positioned context.
4. **Editor-renderable signal.** Editors get a way to know
   "this range is a secret value" without the server having to
   echo the bytes — `protolsp/secretRanges`.
5. **Authoring nudge.** Warning diagnostics on literal scalar
   secret values + a quick-fix that rewrites them as references
   (chameleon-aware: `${env.NAME}`, `${vault.foo#bar}`).
6. **Configurable policy.** Per-workspace strictness via
   `.protolsp.pxf`: allow literal secrets / warn / refuse.

## Non-goals

- **At-rest encryption of secret values.** That's sops's job
  via chameleon. protolsp treats encrypted layer bytes as
  opaque (per chameleon-integration's design); plaintext
  secrets in non-encrypted layers are a real workflow but
  they're the user's choice to make.
- **Memory protection at the OS level.** memguard / mlock /
  secret enclaves are runtime concerns; chameleon already
  handles them via the `OnSecretField` hook that pipes values
  into memguard buffers. protolsp's edit-time threat model is
  different (we serve diagnostics + hover, not application
  config); we drop the value, we don't seal it.
- **Source-file byte scrubbing.** `Document.Source` keeps the
  raw bytes because hover, completion, go-to-definition, and
  every cursor-position feature need source-positioned access.
  Stripping bytes in-memory would break the existing feature
  surface for marginal residual-risk reduction.
- **Replacing the `pxf.Secret` type.** That's protowire's
  schema. We consume it; we don't redefine it.
- **Per-field opt-out** ("treat this Secret field as plain
  text"). If the user wants plain text, they should use a
  string field. The whole point of typing it as `pxf.Secret`
  is to opt in to redaction.

## What `pxf.Secret` already does

(Recap; not a redesign.)

`pxf.Secret` is a built-in protowire message:

```proto
message Secret {
  string value       = 1;  // the actual secret bytes
  string hint        = 2;  // human-readable hint (e.g. "AWS key, US-East")
  string fingerprint = 3;  // opaque deterministic hash for auditing
}
```

Two surface forms in PXF source:

- **Scalar shorthand**: `pw = "value"` — sugar for
  `pw { value = "value" }`.
- **Block form**: `pw { value = "..."; hint = "..."; fingerprint = "..." }`.

The pxf parser's `UnmarshalOptions.OnSecretField func(path,
value string) error` intercepts the value at decode time:

- When set, the value is routed through the callback; the
  inner `value` field is left unpopulated on the proto
  message; `hint` and `fingerprint` (deliberately
  non-sensitive) populate normally.
- When unset, the value populates `Secret.value` as a string —
  the current protolsp behavior.

`hint` and `fingerprint` are public by design. The redaction
machinery in this design targets `value` specifically; hint and
fingerprint go through every channel unchanged.

## Layer 1: in-process redaction

Three changes, all inside protolsp's existing analyze /
publish paths. No protocol surface; pure correctness.

### Wire the parser hook

Where protolsp constructs `pxf.UnmarshalOptions`, pass an
`OnSecretField` hook that **drops** the value:

```go
opts := pxf.UnmarshalOptions{
    TypeResolver:   resolver,
    DiscardUnknown: true,
    OnSecretField: func(path, _ string) error {
        // protolsp's edit-time threat model: we don't need the
        // value for any LSP feature. Drop on the floor.
        return nil
    },
}
```

After this change, `Secret.value` on the resulting proto
message is the empty string regardless of what the source said.
`hint` and `fingerprint` populate normally.

This handles failure mode (3) — no plaintext in the proto
message held in the Document cache.

### Secret-range table

During analyze, alongside the AST and the validated message,
build a table of **secret value ranges**:

```go
type SecretRange struct {
    Range    protocol.Range  // value token (scalar) or value-assignment block range
    Path     string          // dotted field path, e.g. "credentials.password"
    Form     SecretForm      // ScalarShorthand | BlockForm
}

type SecretForm int
const (
    ScalarShorthand SecretForm = iota
    BlockForm
)
```

Built by walking the AST: for every assignment / block whose
resolved `FieldDescriptor` has `Message().FullName() ==
"pxf.Secret"`, emit one `SecretRange`. The walk reuses the
same descriptor-resolution logic already used by hover /
diagnostics.

`Document` gains a `SecretRanges []SecretRange` field, kept
fresh by analyze. This single table powers both wire-side
redaction (layer 1) and editor-side signaling (layer 2).

### Redact at the emit boundary

Every LSP payload the server emits that **could** include
source-positioned content goes through a redactor:

- `publishDiagnostics`: for each diagnostic, if its range
  intersects any `SecretRange`, scrub the value-quoting bytes
  from `Diagnostic.Message`. We don't drop the diagnostic — we
  rewrite the value substring to `<redacted>`. The diagnostic
  still tells the user *what* the problem is (type mismatch,
  required missing, etc.), just not *what value* tripped it.
- `hover`: never includes the value today (we render the type
  + leading doc comment + provenance). Confirmed via a test
  that this stays true even when the user hovers a Secret
  field.
- `completion`: details / documentation strings consulted; if a
  candidate's documentation includes a value example that
  happens to be a secret-typed field, redact.
- Code-action previews / edits: if a quick-fix would rewrite
  source bytes that span a secret value range, the preview
  string the editor renders ("replace `<old>` with `<new>`")
  redacts `<old>` to `<redacted>`. The actual TextEdit ranges
  are unchanged — the editor applies the literal source bytes
  it already has on disk; we just don't echo them in the
  preview.

The redactor lives in a small `internal/redact` package:

```go
package redact

// Diagnostic mutates a *protocol.Diagnostic in place, replacing
// any byte spans inside d.Range that overlap a SecretRange with
// "<redacted>" in d.Message. d.Range itself is unchanged.
func Diagnostic(d *protocol.Diagnostic, secrets []SecretRange, source []byte)

// String redacts any secret-value substrings in s when their
// source positions overlap any range in secrets. Used by hover
// and completion paths that include free-form source quotes.
func String(s string, secrets []SecretRange, source []byte) string
```

### Suppress in logs

`slog` calls in the diagnostic / analyze paths consult the
redactor before formatting. The pattern:

```go
s.log.Warn("schema validation error",
    slog.String("field", path),
    slog.String("message", redact.String(err.Error(), doc.SecretRanges, doc.Source)),
)
```

We don't try to scrub arbitrary log values automatically — that
would need taint tracking across the whole codebase. Instead,
the small handful of paths that format source-positioned text
(`analyze`, `publishDiagnostics`, `dispatchHover`,
`dispatchCompletion`, `dispatchCodeAction`) get explicit redact
calls. Anywhere else that ends up logging values is a bug to
fix at the source.

## Layer 2: editor-side signaling

Same shape as `protolsp/datasetRegions` from the dataset-regions
design: a custom pull request + a push notification when ranges
change.

### Request: `protolsp/secretRanges`

```typescript
interface SecretRangesParams {
  textDocument: TextDocumentIdentifier;
}

interface SecretRange {
  range: Range;             // covers ONLY the value token / value-assignment line
  fieldName: string;        // dotted: "credentials.password"
  form: "scalar" | "block"; // ScalarShorthand | BlockForm
}

interface SecretRangesResult {
  ranges: SecretRange[];
}
```

Pull-style; cached on `Document.SecretRanges`. No per-call
recomputation.

### Notification: `protolsp/secretRangesChanged`

```typescript
interface SecretRangesChangedParams {
  textDocument: TextDocumentIdentifier;
  version: number;
  ranges: SecretRange[];
}
```

Server emits when an edit shifts ranges (boundaries change,
fields added / removed). Coalesce identical updates; tag with
document version so the editor discards out-of-order
notifications.

### Editor behavior

- **pxfed**: masks each `SecretRange` with asterisks by
  default. Offers a per-range "Reveal" toggle that decorates
  the range as plain text for that session (no LSP round-trip
  — the editor already has the bytes from `didOpen`). On
  `didChange`, mask reapplies.
- **VS Code**: uses semantic tokens to dim the secret-value
  ranges + adds a hover decorator ("(secret value — content
  hidden)"). Alternative: a CodeLens above the line with a
  "Reveal" command. Choose one based on what reads best — both
  are cheap.

### Why not standard `documentSymbol` / semantic tokens?

`semanticTokens` is the right protocol for "render these bytes
with this color" — but it lacks structured per-token metadata
(field path, form). Editors building richer affordances (a
"Show secrets" view, audit reporting) need the structured
data, hence a dedicated method. Once editors have it, they can
also publish complementary `semanticTokens` themselves.

## Layer 3: authoring guidance

### Warning diagnostic on literal scalar secrets

Every scalar-shorthand assignment to a `pxf.Secret` field with
a literal string value gets a Warning-severity diagnostic:

```
literal secret value at `credentials.password` — consider a
vault reference (`${env.DB_PASSWORD}`,
`${vault.secret/db#password}`, etc.)
```

Severity: `protocol.DiagnosticSeverityWarning`. Source:
`protolsp/security`. Range: the value token (same range as the
corresponding `SecretRange`).

Triggered only on scalar shorthand. Block-form Secret
assignments with a `value = "..."` get the same diagnostic
applied to the inner `value` line.

Doesn't fire when the value text matches a known reference
syntax — `${...}` interpolations are presumed-non-literal and
get a pass. Pattern: `^\s*\$\{.+\}\s*$`.

### Quick-fix code action

For each warning emitted above, offer a Quick Fix code action:

- Title: "Replace literal with env reference"
- Edit: rewrite `pw = "secretvalue"` to `pw = "${env.PW}"`,
  with `PW` derived from the field's dotted path (uppercase
  snake_case).
- If chameleon is configured in the workspace and the workspace
  has an env source with a prefix, derive `PW` from
  `chameleon`'s convention (matches the env-source hover hint
  from chameleon-integration).
- The editor's `applyEdit` path handles the actual rewrite;
  protolsp returns a `WorkspaceEdit`.

We don't auto-apply. The user keeps control over whether the
literal moves to env, vault, or stays.

### Policy: workspace strictness

`.protolsp.pxf` gains a `security` block governing this layer:

```pxf
security {
  inline_secrets = "warn"   # "allow" | "warn" | "refuse"
}
```

- `allow` — no diagnostic. Some shops permit literal secrets
  for local-dev workflows.
- `warn` — Warning diagnostic + quick-fix (the default).
- `refuse` — Error diagnostic. Quick-fix still offered. Strict
  shops where literal secrets are a CI-failing offense.

The setting maps onto the config-protocol tiers — a project
default in Tier 2 (file), with per-user `protolsp.security.inline_secrets`
in Tier 3, and per-session overrides in Tier 4 (rare; useful
for debugging across both strict and lax shops in one
session).

## Protocol surface summary

One request + one notification. Mirrors `protolsp/datasetRegions`
intentionally.

```
Request:        protolsp/secretRanges
Notification:   protolsp/secretRangesChanged
```

Nothing else gets new protocol surface. The redaction work is
in-process; the warning + quick-fix work uses existing
`publishDiagnostics` + `codeAction`.

## Subtleties

### Block form's non-sensitive subfields

`pw { value = "..."; hint = "AWS key"; fingerprint = "abc..." }`
— only the `value` line is a secret range. `hint` and
`fingerprint` propagate normally through hover, completion,
diagnostics, logs. Range computation walks the block's entries
and tags only the `value` assignment's range.

### Source range covers value, not field name

A `SecretRange` covers the value token (scalar) or the
`value = ...` assignment inside a block — NOT the field name.
Hovering the field `password` itself stays useful (shows the
type, the schema's doc comment); only the bytes that would
echo the value are gated. Editors that color or mask based on
`SecretRange` must use the structured range, not "the whole
line."

### Defense in depth: parser + emit boundary

The `OnSecretField` hook prevents in-memory exposure on the
proto message. The emit-boundary redactor prevents wire / log
exposure. Both layers are required even though they overlap:

- If a future code path reads `doc.Source[range]` directly
  (e.g. a code-action preview formats the source line), the
  parser hook doesn't help — only the redactor does.
- If a future code path bypasses the redactor (new feature
  forgets to call it), the parser hook still keeps the value
  out of any proto message we serialize.

Belts and braces. Cheap.

### Workspace config values that ARE secrets

`.protolsp.pxf` itself can carry secret-typed values — e.g.
`schema_sources[0].registry.token` if the schema were declared
that way. The config-protocol design specifies that secret-
typed values redact by default in `getEffectiveConfig`
responses; an explicit `revealSecrets: true` opts in.

The secret-range table for `.protolsp.pxf` is built the same
way as for any other PXF document. Hovering its `token` field
gets the standard "secret value" treatment.

### Source bytes still leak through `protolsp/sourceContent`

Wait — they don't. `protolsp/sourceContent` returns `.proto`
source (schema files), not PXF documents. Schema files don't
contain secrets; they declare types. Confirmed safe.

The `registry://` URI scheme is for go-to-definition into
schema. PXF documents never live behind a `registry://` URI.

### Interaction with `LastGoodParse`

`Document.LastGoodParse` caches the most recent successful AST
across edits. When analyze fails mid-typing, downstream
features (completion, hover) fall back to it. SecretRanges
must also be cached on a per-parse basis so the fallback
preserves redaction — never let a transient parse failure
reset secret coverage to empty.

### `pxf.Secret` value via chameleon's chain

Chameleon resolves secret values from sops layers at runtime;
the resolved value lives in a memguard buffer. None of that
crosses the LSP boundary. At edit-time the user sees the
encrypted layer as opaque (per chameleon-integration); the
plaintext exists nowhere in protolsp.

## Cross-cuts

### With config-protocol.md

The "Secret-typed values redacted by default" decision in
config-protocol relies on the SecretRange table + redactor
from this design. `getEffectiveConfig` calls
`redact.String(...)` on every value before returning it; with
`revealSecrets: true`, skips the call.

### With chameleon-integration.md

Chameleon's merged-value hover ("Chain resolution: base.pxf =
..., env/dev.pxf = ...") consults the redactor for every per-
layer value. Secret values are shown as `<redacted>` in every
row; the user sees which layer SET the secret, just not the
bytes. Matches what the chameleon doc already promises.

### With dataset-regions.md

A `@dataset` of `pxf.Secret` rows is theoretically possible
(bulk credential rotation file). The SecretRange table works
the same — one range per row's value. Huge regions skip from
the full validator anyway (per dataset-regions); their secret
ranges still register so the editor masks them in the virtual
table view.

### With workspace-config.md

`security { inline_secrets = "..." }` adds a new block to the
schema. The config defaults to `"warn"`. Per-user / per-session
overrides flow through the config-protocol tiers.

### With registryStatus / source content / list messages

None of these touch secret values; the existing custom methods
are unchanged.

## Phased delivery

| Phase | Scope | LoC est. |
|-------|-------|----------|
| **S1** | `OnSecretField` hook wired into the decoder. `SecretRange` walk inside analyze. `internal/redact` package. Redaction applied to `publishDiagnostics`, hover, completion details. Logs in analyze / publish redact. Tests: scalar + block; tampered value-quotes scrubbed. | ~500 |
| **S2** | `protolsp/secretRanges` request + `protolsp/secretRangesChanged` notification. Editor side: pxfed mask + reveal; VS Code semantic-token dim. | ~250 protolsp + ~150 pxfed + ~80 VS Code ext |
| **S3** | Warning diagnostics on literal scalars (with `^\$\{...\}$` interpolation pass-through). Quick-fix to rewrite as `${env.NAME}`. | ~250 |
| **S4** | `security { inline_secrets }` policy in `.protolsp.pxf`. Allow / warn / refuse modes. Per-user + per-session via config-protocol tiers. | ~150 |

S1 alone closes the wire / log exposure — it's the must-have.
S2 turns the user-visible editor experience from "indistinguishable
strings" to "obviously masked secrets." S3 is the authoring
nudge. S4 is enforcement for shops that want it.

## Open questions

1. **Should the redactor know about non-`pxf.Secret` sensitive
   patterns?** Some teams want regex-based redaction
   (`^(sk_live_|AKIA|ghp_)\w{20,}$`) for accidentally-pasted
   credentials. Out of scope here; could be a future extension.
   For now, redaction triggers only on `pxf.Secret`-typed
   fields.
2. **`Diagnostic.Message` redaction granularity.** When a type-
   mismatch error message includes both a value (`got "secret-
   bytes"`) and metadata (`expected int`), we want to redact the
   value but keep the metadata. Today's pxf error messages are
   ad-hoc strings; we'd need a small post-processor that knows
   the parser's error-message shapes, OR we lean on byte-range
   intersection (redact any substring that came from the
   secret's range). **Lean**: byte-range intersection — robust
   to future parser-message changes.
3. **Reveal flow in editors.** When a user clicks "Reveal" on a
   masked secret, the editor already has the bytes (from the
   document the user opened). No LSP round-trip needed. But:
   should the reveal be ephemeral (re-mask after N seconds) or
   sticky for the session? **Lean**: sticky for the session,
   ephemeral per-range only via an explicit toggle the user
   reapplies. Avoids both "I revealed once and forgot, now my
   screen-share leaked it" AND "every keystroke re-masks
   annoyingly."
4. **Quick-fix vault references.** Today's grammar supports
   `${env.NAME}` via chameleon. Vault syntax
   (`${vault.path#field}`) is hypothetical. The quick-fix
   should suggest only references the grammar actually
   supports; we hard-code the env form for v1, extend when
   vault syntax lands.
5. **Block-form `value = "${env.X}"` redaction.** A vault
   reference inside a Secret block isn't a literal — the value
   at runtime is the resolved vault content. The source text
   `${env.X}` is itself non-sensitive. Don't redact
   reference-syntax values; redact only when the literal
   pattern matches a non-reference string. Pattern:
   `^\s*"[^$].*"\s*$` for "starts with `"`, not followed by
   `$`" → literal.
6. **What about `pxf.Secret`-typed fields that are repeated /
   map?** A `repeated pxf.Secret` field is unusual but legal.
   The SecretRange walk handles it the same: one range per
   element. Map keys to Secret values: range covers the value
   side; the key is non-sensitive.

## Decision log

| Date       | Decision                                                                              | Why                                                                                          |
|------------|---------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| 2026-05-16 | Three layers: emit-boundary redaction, editor-side signaling, authoring guidance      | Each addresses a distinct exposure mode; can ship independently.                              |
| 2026-05-16 | Wire `OnSecretField` to drop the value, not seal it                                   | Edit-time threat model is different from runtime; drop is the simplest correct policy.        |
| 2026-05-16 | `SecretRange` table in `Document`, computed during analyze                            | One source of truth for both wire redaction and editor signaling.                             |
| 2026-05-16 | Custom method `protolsp/secretRanges` (mirror of `datasetRegions`)                    | Editors need structured per-range metadata, not just colored bytes; semanticTokens insufficient.|
| 2026-05-16 | Range covers value token only, NOT field name                                          | Field name hover stays useful (type + doc comment); only the value bytes are gated.            |
| 2026-05-16 | Defense in depth: parser hook AND emit-boundary redactor                              | If a future code path bypasses one layer, the other still catches the value.                  |
| 2026-05-16 | Warning + quick-fix on literal scalars; skip values matching `^\$\{...\}$`             | Literal-vs-reference is the actionable distinction.                                            |
| 2026-05-16 | `security.inline_secrets` policy in `.protolsp.pxf` (allow / warn / refuse)           | Strictness varies per shop; config-protocol tiers let users override per-session.              |
| 2026-05-16 | Redaction granularity by byte-range intersection (not parser-message-shape parsing)   | Robust to future parser-message changes; simple to implement correctly.                       |
| 2026-05-16 | Source-byte scrubbing of `Document.Source` is out of scope                            | Breaks source-positioned hover / completion / definition for marginal residual-risk reduction.|

## Notes for future readers

- **The redactor is small and ought to stay small.** Two
  functions (`Diagnostic` and `String`). Resist growing it
  into a taint-tracking system. The handful of paths that need
  redaction call it explicitly; anywhere else that ends up
  logging secret values is a bug to fix at the source.
- **`SecretRange` lives alongside `DatasetRegion` and other
  per-document derived state.** As that derived-state set
  grows (semantic tokens, folding ranges, etc.) it may be
  worth introducing a single `Document.Analysis` struct that
  holds all of them. Refactor when there are three or four;
  not before.
- **Layer 3's quick-fix is a wedge for further automation.**
  Once the rewrite-as-reference pattern is in place, adding
  "rewrite as vault reference" or "rewrite as 1password
  reference" is cheap. Avoid over-generalizing the fix
  templates until at least two reference forms are real.
- **The `OnSecretField` hook is shared infrastructure with
  chameleon.** Both use it. If/when protowire-go's
  `UnmarshalOptions` grows additional hook kinds (e.g.
  `OnDeprecatedField`), the same wiring pattern applies.
