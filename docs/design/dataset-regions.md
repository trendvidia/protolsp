# Dataset regions — region-aware LSP for typed bulk content

| Field    | Value                                                                                                                                                                |
|----------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Status   | Draft                                                                                                                                                                |
| Date     | 2026-05-16                                                                                                                                                           |
| Scope    | Promote `@dataset` blocks from "AST detail" to a first-class protocol-level object. Surface them to editors so the UI can degrade gracefully on huge regions, and let the server validate row-by-row inside a region instead of revalidating the whole document. |
| Out      | Spec changes to `@dataset` itself (frozen at v1.0). Streaming parser. Database-backed dataset storage (separate workstream). pxfed's virtual-table UI internals — covered at a high level here; details land in `pxfed/docs/design/`. |

## Summary

A `.pxf` document is a fine LSP citizen at config size — kilobytes,
hundreds of fields. The PXF format also supports `@dataset
<Type> { … }` blocks intended for bulk typed content: thousands of
rows of the same message type. A 200 MB database dump exported as
PXF is grammar-conforming and will become a real pxfed workload.

LSP isn't built for documents that size. Full-document sync,
whole-document parse + validate, per-keystroke diagnostic publish
— each works fine at KB scale and falls apart at MB+. Rather than
fight the protocol with incremental sync, spatial indexes, and
sampled validation across the board, protolsp will treat
`@dataset` blocks as **typed regions** with their own
LSP-visible identity, then strip the heavy machinery from the
ones that exceed a threshold.

The pre-load pass identifies each region, computes its size and
bound type, and emits the list to the editor over a custom LSP
method. pxfed (and any other consumer) reads the list and decides
per-region whether to render normally or switch to a degraded
"huge region" UI. The server uses the same region map to scope
validation: a row inside a non-huge region validates against the
region's type via the normal pass; a row inside a huge region
validates only when its line is edited (when incremental sync is
in place), not on every analyze.

## Motivation

Three concrete failures the current design has at scale:

- **Transport**: `didChange` carries the whole document
  (`TextDocumentSyncKindFull`). 200 MB resent per keystroke.
  Editor latency goes from microseconds to seconds.
- **Validator cost**: `pxf.UnmarshalDescriptor` walks the whole
  AST and constructs a `dynamicpb.Message` per row. 100K rows
  per analyze is a real GB of transient allocation; multiplied
  by per-keystroke analyze, the GC pressure alone is fatal.
- **Cursor-driven features**: hover / completion / code actions
  / go-to-definition all linear-scan the entry list. 100K entries
  × per-entry cost = perceptible per-cursor lag.

A size gate alone would solve the cost problem ("over 50 MB →
parse-only") but loses something the dataset structure makes
possible: per-row validation. The grammar already tells us "rows
2–100,000 are all of type `acme.billing.Invoice`" — we should
exploit that.

## Goals

1. **Region awareness in the protocol.** Editors can ask "where
   are the `@dataset` regions in this document, what type are
   they bound to, and how big are they?" and get back a list.
2. **Server-driven threshold.** The server decides which regions
   count as "huge" so VS Code, pxfed, and the hosted Monaco IDE
   stay consistent. Threshold is configurable.
3. **Region-scoped validation.** Validation strategy varies per
   region: full-pass for non-huge, edit-scoped for huge.
4. **Resilience to mid-edit syntax.** Region detection works
   when the document doesn't fully parse — typing inside an
   unclosed `@dataset { …` shouldn't make the regions vanish.
5. **No spec changes.** This is a tooling layer on top of the
   v1.0 grammar; `@dataset` semantics are unchanged.

## Non-goals

- **A streaming parser.** Out of scope here; would help, but the
  region split delivers most of the user-visible win without it.
- **Database-style row storage.** pxfed's huge-region UI will
  page rows; that's a separate workstream around how pxfed
  stores and serves the underlying bytes.
- **Region-aware editing in VS Code.** The VS Code extension
  gets the regions and a fold decoration / CodeLens; it does
  not implement a virtual-table view. That's pxfed-specific.
- **Sub-region indexing (per-row positions).** Not required for
  the feature; would add memory cost proportional to row count
  for marginal benefit.

## Concept: dataset region

A region is a contiguous block of source bytes corresponding to
one `@dataset Foo.Bar { … }` directive plus its body. Each
region has:

| Field        | Type     | Meaning                                                                  |
|--------------|----------|--------------------------------------------------------------------------|
| `range`      | `Range`  | LSP range covering the directive line through the closing `}`.           |
| `typeFQN`    | string   | The fully-qualified name written after `@dataset`. `""` when none typed. |
| `resolvable` | bool     | Whether the row type is known. False → can't do row validation. With `typeFQN` `""`, true means the region is bound to the anonymous `@proto` before it. |
| `rowCount`   | int      | Number of row entries inside, or `-1` when the region didn't fully parse.|
| `byteSize`   | int      | Source bytes the region occupies.                                        |
| `huge`       | bool     | Server's call: true when row count or byte size exceeds the threshold.   |

Regions are non-overlapping (the grammar nests `@dataset` only
under blocks, not other `@dataset`s in practice). Future-proofing
note: nested `@dataset`s — if they ever appear — would be
represented as separate regions; the outer region's `rowCount`
counts rows in its own body, not the inner region's rows.

## Protocol extension

Two custom LSP methods, both namespaced under `protolsp/`:

### Request: `protolsp/datasetRegions`

```typescript
interface DatasetRegionsParams {
  textDocument: TextDocumentIdentifier;
}

interface DatasetRegion {
  range: Range;
  typeFQN: string;
  resolvable: boolean;
  rowCount: number;       // -1 when parsing didn't complete
  byteSize: number;
  huge: boolean;
}

interface DatasetRegionsResult {
  regions: DatasetRegion[];
}
```

Pull-style: editor asks at first connect, after reconnect, or when
it suspects its view is stale. Server returns the current snapshot.

### Notification: `protolsp/datasetRegionsChanged`

```typescript
interface DatasetRegionsChangedParams {
  textDocument: TextDocumentIdentifier;
  version: number;        // matches the document version
  regions: DatasetRegion[];
}
```

Server pushes a fresh region list whenever a `didChange` could have
shifted boundaries or counts. Avoids pxfed polling on every
keystroke. The client treats this as a full replacement; no diff
shape (regions are typically few — tens, not thousands).

### Why not `documentSymbol`?

`textDocument/documentSymbol` returns a hierarchical outline and
is consumed by editor outline panes. It tolerates extension via
`detail`/`tags` but the response contract is "show me the
outline" — using it to drive editor-behavior switching mixes
concerns. A dedicated method keeps the semantics explicit and
leaves `documentSymbol` free to do its actual job later.

## Server architecture

### Two detection passes

**Line-based pre-pass** runs on every `didOpen` and `didChange`,
independent of parse success:

1. Scan source bytes for lines starting with `@dataset` (after
   whitespace stripping).
2. From each match, brace-count forward to find the closing `}`.
   Track string literals so braces inside strings don't fool the
   counter.
3. Emit a tentative region with: `range` (start of directive line
   to end of closing brace, or end-of-document if unclosed),
   `typeFQN` (the token after `@dataset` on the directive line),
   `byteSize`. `rowCount` defaults to `-1`; `resolvable` defaults
   to `false`; `huge` is computed from `byteSize` alone at this
   stage.

**AST-integrated pass** runs at the end of `analyze` when parse
succeeds. For each `@dataset` block in `doc.Parsed.Entries`:

1. Take the parsed range (start/end positions are already in the
   AST).
2. Read `typeFQN` from the AST.
3. Count rows by walking `Entries` (`len(block.Entries)`).
4. Ask the resolver `FindMessageByName(typeFQN)`. Set
   `resolvable` accordingly.
5. Compute `huge` from rows + bytes.

The AST-integrated result replaces the line-based result for the
emitted regions when both ran. The line-based pre-pass remains
the fallback whenever `doc.Parsed == nil` (parse failed) or when
the AST is from `doc.LastGoodParse` (mid-edit).

### Where the regions live

`Document` gains a `Regions []DatasetRegion` field, recomputed at
the end of `analyze`. The dispatcher for `protolsp/datasetRegions`
reads it directly; no per-request recomputation.

### When notifications fire

`analyze` already runs on every `didOpen` / `didChange`. After
the region list is built, the server compares it to the previous
snapshot. If they differ — region added/removed, range changed,
huge flag flipped — emit `protolsp/datasetRegionsChanged` with
the new list and the document version.

Skip the notification when the list is structurally identical
(common: typing inside a row only changes content, not
boundaries). Saves a non-trivial amount of wire traffic for
files with stable structure.

### Validation strategy by region

`analyze`'s current behavior — parse + (if `@type` set)
`UnmarshalDescriptor` against the document type — keeps running.
The new region pipeline layers on:

1. **Non-huge regions**: the existing whole-document
   `UnmarshalDescriptor` already validates them as part of the
   parent message. No change needed.
2. **Huge regions** with `resolvable: true`: skip them from the
   full-document validator. Each row is validated independently
   when the user edits it (`didChange` with incremental sync
   tells us the changed range; we map that to a row, run
   `UnmarshalDescriptor` on just that row's bytes against the
   region's `typeFQN`, publish diagnostics scoped to the row).
3. **Huge regions** with `resolvable: false`: no per-row
   validation possible. The directive line gets a single
   diagnostic ("unknown type `Foo.Bar` — row validation
   disabled") and the rows themselves are left alone.

A `pxf validate` CLI command (separate workstream) handles
whole-region validation outside the LSP request/response cycle
when a user explicitly wants it.

### Region-aware completion & hover

The cursor-position → region → type lookup is identical to what
the validator does:

- Given a cursor at `(line, col)`, scan `doc.Regions` for the
  region whose `range` contains it.
- If found, use `region.typeFQN` for completion and hover lookups
  inside that region's rows.
- If not found (cursor is outside any dataset), fall back to the
  document's `@type`.

This is O(N) in number of regions, which is small (tens, almost
never hundreds). No need for a tree.

## Editor behavior

### pxfed

- Listens for `protolsp/datasetRegionsChanged` from connection
  startup.
- Renders normal Monaco for non-huge regions.
- For huge regions: collapses the body to a single placeholder
  line — `[N rows · K KB · Type]` — that the user can click to
  expand a virtual-table view. The view is paginated, read-only,
  backed by a separate reader that streams rows from the file
  without loading them into Monaco's buffer.
- Edits inside the placeholder fold are gated to "open table to
  edit" so users don't accidentally type into 100K-row content.
- Status bar reflects: total regions, sum of rows in huge
  regions, schema staleness (from `protolsp/registryStatus`).

### VS Code extension

- Gets the regions, adds a `FoldingRange` per huge region with
  the same placeholder text.
- A CodeLens on the directive line offers "Validate region"
  (runs `pxf validate` on that range, displays results).
- Normal text editing inside still allowed — users on VS Code
  pay the cost if they choose to edit a huge dataset there.

### Hosted Monaco IDE

Same renderer as pxfed. Inherits the virtual-table view.

## Subtleties

### Resilient detection while typing

When the user types `@dataset Foo.Bar {` and pauses, the document
has an unclosed brace. The line-based pre-pass still emits a
region — its `range` extends to end-of-document; `rowCount` stays
`-1`; `huge` is computed from byte size alone. When the user
types the closing `}`, the next pre-pass tightens the range.

`LastGoodParse` (already cached on `Document` for completion)
provides the most-recent valid AST whenever the current parse
fails — both validation and region detection consult it so
features keep working through transient mid-edit invalid states.

### Type resolution prerequisite

`resolvable: false` is a non-fatal degraded state: the editor
still renders the placeholder for huge regions, the user can
still navigate, but row validation can't run. The directive line
gets a "unknown type" diagnostic on its directive line so the
problem is visible.

A common cause: the registry hasn't loaded yet, or
`initializationOptions.registry` is empty. We don't gate the
region pipeline on schema availability; we just degrade.

### Threshold ownership

Server-side, with compile-time defaults: a region is `huge` when
`rowCount >= 1000` OR `byteSize >= 100 KB`. The thresholds live
on `Server.hugeRegionBytes` / `Server.hugeRegionRows` and could be
wired through the four-tier config (`dataset.huge_threshold.*`)
when a real consumer asks for tuning — today every shipped
editor surface uses the defaults, so the proposed initOptions
shape from earlier drafts was never implemented. Re-introduce
via `.protolsp.pxf` + `workspace/configuration` (matching the
shape of `security.inline_secrets`) if the gap becomes real.

### Cross-region edits

Pastes that cross a region boundary are the worst case. The
line-based pre-pass handles them in one O(bytes) scan; the
AST-integrated pass runs on the next successful parse. Between
those two passes the editor sees a snapshot that's never
internally inconsistent (boundaries from pre-pass; counts and
resolvability from the most recent AST).

### Region-aware competing methods

Several existing custom methods become region-aware:

- `protolsp/listMessages` — unchanged. Returns types from the
  resolver, not per-region.
- `protolsp/sourceContent` — unchanged. Region info doesn't
  affect file fetch.
- `protolsp/registryStatus` — unchanged.

So this is additive at the protocol layer; nothing existing
needs to change shape.

### Non-`@dataset` huge blocks

A message with a `repeated` field of 100K entries (not declared
via `@dataset`) hits the same scaling cliff but isn't covered by
region detection as currently scoped. Three options for a future
extension, in order of preference:

1. Detect any block whose byte size exceeds the threshold and
   flag it as `huge` even without a `@dataset` directive (no
   `typeFQN` set; no per-row validation, just UI degradation).
2. Encourage authors to use `@dataset` for bulk content (and
   `pxf q` to convert `repeated` fields when ingesting).
3. Leave it alone — the size gate from "Out of MVP" handles it
   coarsely.

Lean: #1 once the `@dataset` path is in production. Adds a third
detection pass that's purely size-based.

## Phased delivery

| Phase | Scope | LoC est. |
|-------|-------|----------|
| **R1** | Line-based pre-pass + `protolsp/datasetRegions` request + `Document.Regions` field. Server-side threshold reading from initializationOptions. Tests cover detection on well-formed and unclosed `@dataset` blocks. No notification yet; editor pulls on first connect. | ~400 |
| **R2** | AST-integrated pass that adds `rowCount` + `resolvable`. `protolsp/datasetRegionsChanged` notification fires on structural changes. Tests cover full vs degraded states. | ~250 |
| **R3** | Region-aware completion + hover (cursor → region → type lookup). Reuses the existing AST-walk infrastructure; this is mostly wiring. | ~150 |
| **R4** | Incremental sync (`TextDocumentSyncKindIncremental`) lands as a prerequisite. Per-row validation on `didChange` inside a huge region. | ~400 |
| **R5** | pxfed virtual-table view consumes the regions list; renders huge regions as placeholders that open a paginated table. Lands in pxfed, not protolsp. | ~600 in pxfed |
| **R6** | VS Code extension: FoldingRange + CodeLens for huge regions. "Validate region" command. | ~80 in protowire |

R1 alone is shippable — pxfed and VS Code can already render
fold placeholders for huge regions before per-row validation
exists. Each later phase is independently demoable.

## Open questions

1. **Threshold heuristic.** Rows alone? Bytes alone? Both with
   min()? Bytes are easier to compute pre-parse; rows are more
   semantic. **Lean**: both with OR — flag if either exceeds.
2. **Region boundaries and `@proto` embedded schemas.** If a
   `@dataset` is bound to a type declared inline via `@proto`,
   region detection runs before the inline schema is loaded
   into the resolver. Solution: resolver gains a "load
   anonymous @proto" hook called from `analyze`; the
   AST-integrated pass runs after that. Out of scope for R1.
3. **Document-version race**. Between a `didChange` arriving
   and the server emitting `datasetRegionsChanged`, more
   `didChange` events may arrive. We tag the notification with
   the document version; the client discards notifications
   older than its current version. (Standard LSP pattern.)
4. **Non-`@dataset` huge blocks** — see §Subtleties.
5. **Region storage in pxfed**. If a user opens a 1 GB file,
   does pxfed buffer the whole thing in memory just to expose
   the huge-region body to the virtual-table reader? Probably
   not. The virtual-table reader should mmap or stream the file
   independently of Monaco's buffer. Details for pxfed's own
   design doc.
6. **Compatibility with `@table` / `@view` / future directives**.
   `@table` is reserved for a future storage-definition meaning
   (per the v1.0 spec freeze). If/when it lands, region
   detection should treat it similarly — same pipeline, possibly
   different placeholder text in editors. Designed-in via the
   detection pass being directive-name-driven, easy to extend.

## Decision log

| Date       | Decision                                                                 | Why                                                                              |
|------------|--------------------------------------------------------------------------|----------------------------------------------------------------------------------|
| 2026-05-16 | Region detection as a server-side concept, exposed via custom LSP method | Editors stay consistent; no per-editor heuristics.                               |
| 2026-05-16 | Two-pass detection (line-based + AST-integrated)                         | Resilience to mid-edit invalid states without losing accuracy when parse works.  |
| 2026-05-16 | Custom method, not `documentSymbol` overload                             | Keeps semantics explicit; outline integration can come later as a separate pass. |
| 2026-05-16 | Threshold server-side, client-overridable                                | Defaults are a UX call; per-workspace overrides are a power-user concession.     |
| 2026-05-16 | Per-row validation gated on incremental sync                             | Without delta info, "validate the changed row" can't be computed cheaply.        |

## Notes for future readers

- Region detection is a thin protocol layer over what the grammar
  already encodes. If `@dataset` semantics ever change (e.g. add
  per-region options like `chunked: true`), the protocol shape
  can grow new fields additively — the field-level granularity
  of `DatasetRegion` was chosen with that in mind.
- The cursor → region → type lookup pattern (used by validation,
  completion, hover) is a small enough function to inline
  everywhere it's needed. Resist the temptation to build a
  generic "region context" abstraction until two more directives
  carry similar semantics.
- The hosted Monaco IDE shares the renderer with pxfed, so the
  virtual-table view (R5) benefits both. Avoid Electron-specific
  APIs in the renderer code paths; route I/O through the
  protolsp custom-method surface where possible.
