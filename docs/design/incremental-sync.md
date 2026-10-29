# Incremental document sync

| Field    | Value                                                                                                                                                                                |
|----------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Status   | Draft                                                                                                                                                                                |
| Date     | 2026-05-16                                                                                                                                                                           |
| Scope    | Switch protolsp from full-document sync (`TextDocumentSyncKindFull`) to incremental sync (`TextDocumentSyncKindIncremental`). Maintain a server-side document model that applies edits in place, with correct LSP position-encoding handling. Negotiate UTF-8 position encoding when the client supports it. Pair with analyze debouncing so rapid edits coalesce instead of running the validator N times in a row. |
| Out      | Incremental parsing (Tree-sitter-style AST reuse across edits — pxf.Parse remains whole-document). Differential analyze (we still reanalyze the entire AST after edits; just on debounced events). Rope / piece-table buffer representations (naïve copy-on-edit is fine at the sizes protolsp serves). A protocol-level resync mechanism for when client and server fall out of sync (cleanup is reopen-the-file for now). |

## Summary

protolsp's M2 design picked full-document sync to keep things
simple: every `didChange` carries the entire document text, and
the server replaces its copy wholesale. That works at KB scale.
At MB scale it's perceptibly slow; at the GB scale a chameleon-
free `@dataset` could reach (per the dataset-regions design),
it's unusable.

This design switches to **incremental sync**. The client sends
the byte-range and replacement text for each edit; the server
applies the edit to its own copy. Per-keystroke wire traffic
goes from "the whole document" to "the byte the user typed."
The whole-document reanalyze still runs after each edit
(unchanged from today), but it runs against a smaller-delta
state, and the incoming `didChange` payload is microseconds
instead of seconds.

Two additional pieces ride along because they make sense to
solve together:

- **Position encoding negotiation.** LSP defaults to UTF-16
  character offsets in `Position.Character`. Modern clients
  support negotiating UTF-8 via `positionEncodings`. Switching
  to UTF-8 saves a conversion step on every position
  computation; falls back to UTF-16 for clients that haven't
  adopted the newer field.
- **Analyze debouncing.** Today's pipeline runs the full
  parse + schema-validate on every `didChange`. With
  incremental sync the input is cheaper, but rapid keystrokes
  still trigger N analyses in a row. A small debounce
  (~150 ms) coalesces them.

After this lands, dataset-regions R4 (per-row validation on
huge regions) can layer on top — incremental sync exposes the
exact byte-range an edit touched, which is what R4 needs to
identify which rows changed.

## Motivation

Three concrete failures of `TextDocumentSyncKindFull` at scale:

1. **Wire cost.** Each `didChange` notification carries the
   entire document. A 1 MB `.pxf` with a 30 ms keystroke
   cadence sustained for one second = 30 MB of wire traffic per
   second. On `protolsp-ws` over a real network, that's
   bandwidth-limited.
2. **Parse cost.** `pxf.Parse` runs over the whole document on
   every keystroke. Today's debouncing is zero — every event
   triggers a parse synchronously inside the handler. For a
   100 KB document, that's tens of milliseconds per parse,
   sustained as the user types.
3. **Validation cost.** `pxf.UnmarshalDescriptor` walks the
   AST and constructs `dynamicpb.Message` instances. For a
   100K-row `@dataset` that's GB of transient allocation per
   keystroke. GC pressure alone is fatal.

The wire cost is fully addressed by incremental sync. Parse and
validation costs are reduced by debouncing and (later, in
dataset-regions R4) by skipping huge regions from the full
pass. This design lays the foundation for all of the above.

## Goals

1. **Server-side document model that applies edits correctly**
   per the LSP spec. Each `TextDocumentContentChangeEvent`
   applies relative to the document state after all preceding
   events in the same notification, not to the original.
2. **Position encoding negotiation.** Advertise
   `positionEncodings: ["utf-8", "utf-16"]` so capable clients
   skip the UTF-16 dance. Fall back to UTF-16 when the client
   doesn't advertise UTF-8 support.
3. **Source-positioned features survive edits.** Parse, hover,
   completion, code actions, go-to-definition all keep
   producing correct positions because they run against the
   post-edit source. No position-shifting heuristics; we
   discard and rebuild derived state on every edit.
4. **Analyze debouncing.** Coalesce rapid edits so the
   validator runs at most every ~150 ms during sustained
   typing.
5. **No protocol regression.** Clients that only support
   full sync continue to work — we still accept full-document
   `TextDocumentContentChangeEvent`s (the LSP spec allows
   either form even when incremental is advertised).

## Non-goals

- **Incremental parsing.** Tree-sitter-style AST reuse across
  edits would be a major rewrite of `pxf.Parse`. Out of scope.
- **Differential analyze.** "Only revalidate the changed
  region" sounds appealing but turns into a maintenance burden
  for marginal benefit at protolsp's typical document sizes.
  Dataset-regions R4 handles the huge-document case
  specifically.
- **Rope / piece-table buffers.** Naïve copy-on-edit
  (`bytes.Buffer`-style) is fine up to a few MB. We'd switch
  representations only with profiling evidence — and protolsp's
  document model is unlikely to grow into rope territory.
- **A "resync" protocol method.** If the client and server
  fall out of sync (version skew, dropped notifications,
  client bug), the recovery is "close and reopen the file."
  Not building a custom resync handshake until we see a real
  failure case.

## The LSP edit-application model

### What the client sends

```typescript
interface DidChangeTextDocumentParams {
  textDocument: VersionedTextDocumentIdentifier;
  contentChanges: TextDocumentContentChangeEvent[];
}

// Two shapes — the spec allows either even with incremental sync.
type TextDocumentContentChangeEvent =
  | { range: Range; rangeLength?: uint; text: string }  // incremental
  | { text: string };                                    // full document
```

Two rules from the spec that bite if you miss them:

1. **Order matters.** Multiple events in one notification apply
   in order. Each subsequent event's `range` is relative to the
   document state *after* the previous event was applied — NOT
   relative to the original. Get this wrong and the server's
   view silently corrupts.
2. **Range-less events are full replacement.** A
   `TextDocumentContentChangeEvent` without a `range` carries
   the entire new document text. Some clients send these
   periodically even with incremental sync advertised (e.g. on
   format-on-save). Handle both shapes.

### What the server does

```go
for _, change := range params.ContentChanges {
    if change.Range == nil {
        // Full-document replacement event.
        doc.Source = []byte(change.Text)
        continue
    }
    start, end, err := byteOffsets(doc.Source, *change.Range, doc.PositionEncoding)
    if err != nil {
        // Out-of-bounds / malformed range. Log + best-effort continue.
        s.log.Warn("didChange: invalid range; document may be out of sync",
            "uri", doc.URI, "err", err)
        continue
    }
    next := make([]byte, 0, len(doc.Source)-(end-start)+len(change.Text))
    next = append(next, doc.Source[:start]...)
    next = append(next, change.Text...)
    next = append(next, doc.Source[end:]...)
    doc.Source = next
}
doc.Version = params.TextDocument.Version
```

`byteOffsets` converts an LSP `Range` (line + character offsets)
into byte indices into `Source`. The character-encoding handling
is documented separately below.

### Discarding derived state

After applying edits, the document's derived state — parse
result, schema-validation findings, `SecretRange` table,
`DatasetRegion` table, completion caches — is stale. The
simplest correct policy: discard all of it on every edit, then
let the analyze pipeline rebuild it.

This matches what M2 already does (the AST is rebuilt on every
didChange because the source changed). What's new is that the
`Source []byte` mutates in place instead of being replaced
wholesale, and the rebuild is now scheduled (debounced) instead
of synchronous.

Position-shifting heuristics (try to keep old positions valid
by adjusting them by the edit's delta) are explicitly out of
scope. They're an optimization for editor-grade incremental
analyses (gopls does this for some derived state); protolsp's
analyses are cheap enough at the typical document size that
the rebuild is the right default.

## Position encoding

### The default mess

LSP 3.0–3.16 specifies `Position.character` as a UTF-16 code-
unit offset. The original justification was that the protocol
was designed alongside VS Code, which uses UTF-16 internally.
Almost every other language stores strings as UTF-8 (Go, Rust,
many editors). Result: every server has to convert.

LSP 3.17 added `positionEncodings` in `ClientCapabilities` and
`positionEncoding` in `ServerCapabilities` so the two can
negotiate UTF-8 / UTF-16 / UTF-32. Most modern clients support
the negotiation.

### What protolsp will do

In `initialize`:

1. Read `params.Capabilities.General.PositionEncodings`. May be
   empty (older client) → fall back to UTF-16.
2. If `"utf-8"` is in the list, advertise it in
   `ServerCapabilities.PositionEncoding` and store it on the
   server.
3. Otherwise advertise (and use) `"utf-16"`.

All position-to-byte conversions consult the negotiated
encoding. The conversion utility lives in
`internal/positions`:

```go
package positions

type Encoding int
const (
    UTF8  Encoding = iota
    UTF16
    UTF32  // future; not advertised
)

// Offset converts an LSP Position to a byte index into src.
// Returns an error when the position is out of range or
// references a malformed sequence in src.
func Offset(src []byte, p protocol.Position, enc Encoding) (int, error)
```

The UTF-16 path walks each line counting UTF-16 code units;
surrogate pairs (characters outside the BMP) count as two. The
UTF-8 path is much simpler — `Position.character` is already
the byte offset within the line.

We already have a `pxfPosToLSP` conversion (M2 ships it). The
new utility supersedes it; M2's helper can become a thin
wrapper around `positions.Offset` to keep callers working.

### What this saves

For ASCII-heavy `.pxf` documents (the common case — config-
shaped content with English identifiers), UTF-8 negotiation
turns position conversion into a constant-time byte index
into the line. The UTF-16 path's per-line walk is fast but
non-trivial; eliminating it on every cursor event is real.

For files with significant non-ASCII content (emoji in
comments, internationalized string literals), both paths walk
the line — but UTF-8 avoids the surrogate-pair edge cases that
UTF-16 introduces.

## Analyze debouncing

### The problem

Today's `analyze` runs synchronously inside `dispatchDidChange`.
A user typing at 100 ms cadence triggers 10 full analyses per
second. Each one parses the document, validates it against the
schema, publishes diagnostics, recomputes secret ranges,
recomputes dataset regions, etc.

### The fix

A small per-document timer. On `didChange`:

1. Apply the edit (synchronously — this is fast).
2. Schedule `analyze` to run after a debounce window (~150 ms
   from now). If a previous schedule exists, reset it.
3. Return from the `didChange` handler.

When the timer fires:

1. Run `analyze` on the current source.
2. Publish diagnostics.
3. Emit `protolsp/secretRangesChanged`,
   `protolsp/datasetRegionsChanged`, etc. for the rebuilt state.

The debounce window is tunable. 150 ms is a reasonable default —
fast enough that diagnostics appear "live" during pauses;
slow enough that sustained typing doesn't spin the validator.
Per-workspace override via `.protolsp.pxf` is a future option
if anyone asks for it.

### Implementation shape

```go
type Document struct {
    // ... existing fields ...
    analyzeTimer *time.Timer  // nil when no analyze is scheduled
}

func (s *Server) scheduleAnalyze(doc *Document) {
    if doc.analyzeTimer != nil {
        doc.analyzeTimer.Stop()
    }
    doc.analyzeTimer = time.AfterFunc(150*time.Millisecond, func() {
        s.runAnalyze(doc)
        doc.analyzeTimer = nil
    })
}
```

Race-free with `didClose`: closing a document stops any pending
timer before tearing down `Document` state.

Tests pin the coalescing behavior: 5 rapid `didChange` events
within 150 ms → exactly 1 analyze call. Cover the timer-fire-
after-close edge case with a sync.WaitGroup.

## Protocol surface changes

In `initialize` result:

```typescript
{
  capabilities: {
    textDocumentSync: {
      openClose: true,
      change: 2,  // TextDocumentSyncKind.Incremental (was 1 = Full)
      save: { includeText: false }
    },
    positionEncoding: "utf-8"  // or "utf-16" depending on client capability
  }
}
```

`textDocumentSync.change` flips from 1 (Full) to 2
(Incremental). `positionEncoding` becomes a real field whose
value depends on the client.

No other protocol changes. All existing custom methods
(`sourceContent`, `listMessages`, `registryStatus`,
`datasetRegions`, `secretRanges`, `chainStatus`, `layerInfo`,
the upcoming config-protocol additions) continue to work
unchanged — they consume `Document.Source` after the edits
have been applied, same as today.

## Subtleties

### Mid-edit invalid documents

A user typing `name = "x` (no closing quote yet) puts the
document in an invalid state. Today's `analyze` already
handles this via `LastGoodParse` — features fall back to the
last parseable AST while the parse is broken. That behavior is
unchanged here; we just apply the same approach against the
new source after each debounce window.

### Position invalidation between debounce window and analyze

The user types: edit applied at t=0, analyze scheduled for
t=150ms. If a `hover` request lands at t=50ms, what runs?

The hover handler is synchronous and uses the current `doc.Parsed`
(or `doc.LastGoodParse` if parsing failed in the last analyze).
That data is from BEFORE the recent edits. So the hover may
return positions that are slightly stale relative to the
current source.

This is acceptable: the staleness is bounded by the debounce
window (150 ms); the editor renders hover at the cursor's
current position regardless; the user gets meaningful data
even if the AST hasn't quite caught up. The alternative — force
synchronous analyze on every cursor event — defeats the
debounce.

### didOpen behavior

`didOpen` carries the full document text and runs analyze
**synchronously** (no debounce). The user expects diagnostics
on file open without delay. The debounce only kicks in for
edits within an already-open document.

### didClose behavior

`didClose` cancels any pending analyze timer and tears down
`Document` state. Tests cover the "edit, close before debounce
fires" race.

### External file changes

A file edited outside the editor (e.g. `git checkout`,
`sed -i`) doesn't go through `didChange`. It goes through
`workspace/didChangeWatchedFiles` (which we already subscribe
to for `.protolsp.pxf`). The behavior is the editor's call:
VS Code prompts the user to revert / overwrite; pxfed will
make its own UX choice. Either way, when the editor finally
sends `didOpen` for the reverted content, we treat it as fresh
state.

### Position-encoding negotiation fallbacks

If a client advertises `positionEncodings: ["utf-32"]` (which
nothing does today), we ignore the unknown values and pick the
highest-priority known value, falling back to UTF-16. Future
encodings (UTF-32) are easy to add when there's demand.

### Server-side document version

`VersionedTextDocumentIdentifier.version` is monotonically
increasing per the spec. Trust it. If we receive a `didChange`
with a version less than what's stored, log a warning and
continue — we'll be slightly out of sync but functional.
Recovery is a `didClose` + `didOpen` cycle.

## Cross-cuts

### With dataset-regions.md

This is the prerequisite for R4 (per-row validation on huge
regions). Once incremental sync lands, `didChange`'s
content-change events tell us exactly which byte ranges
shifted. R4 can use that to identify which rows of a
`@dataset` region were modified and validate only those rows.

### With secret-handling.md

`SecretRange` table rebuilds on every analyze, just like today.
No change. Layer 2's `protolsp/secretRangesChanged`
notification fires on the same schedule as analyze (debounced)
rather than on every keystroke.

### With config-protocol.md

`effectiveConfigChanged` fires when `.protolsp.pxf` is saved;
saving a config file via the editor triggers `didChange` →
debounced analyze → diff detection. The debounce window
applies here too, so a user editing `.protolsp.pxf` doesn't
spin up workspace reconfiguration on every keystroke.

### With workspace-config.md

Workspace discovery (nearest-ancestor walk) runs once at
`didOpen`. Edits to `.protolsp.pxf` go through the
workspace-config watch path, not through this incremental-sync
machinery directly.

## Phased delivery

| Phase | Scope | LoC est. |
|-------|-------|----------|
| **I1** | Flip server capabilities to advertise `TextDocumentSyncKindIncremental`. Implement `Document.Apply(changes)` with both full and incremental shapes. `internal/positions` package for byte-offset conversion (UTF-16 only initially). Tests: single-event apply, multi-event apply, out-of-bounds graceful handling, full-document fallback events. | ~500 |
| **I2** | Position-encoding negotiation. Advertise `positionEncodings: ["utf-8", "utf-16"]`; pick the first match. Implement UTF-8 path in `internal/positions`. Bench both paths on representative inputs. | ~200 |
| **I3** | Analyze debouncing. Per-document timer; coalesce rapid `didChange` events. Tests pin the coalescing behavior (5 events → 1 analyze) and the close-during-debounce race. | ~250 |

I1 alone is the win — wire cost drops immediately on any
client that supports incremental (VS Code, Monaco, Neovim,
Helix, Zed: all of them). I2 trims position-conversion
overhead on capable clients. I3 finishes the work, making the
debounce explicit instead of relying on analyze being
"fast enough."

The three phases can land in three independent PRs. I1 is the
foundational change and depends on nothing else; I2 and I3 can
be done in either order on top of I1.

## Open questions

1. **Debounce window default.** 150 ms is a guess. Other LSPs
   range from 100 ms (rust-analyzer's responsiveness setting)
   to 300 ms (some Java implementations). Pick a default, make
   it configurable via `.protolsp.pxf` (Tier 2 → optionally Tier
   3 via config-protocol) if anyone asks. **Lean**: 150 ms
   default, no config knob until requested.
2. **Should `didOpen` debounce too?** Probably not — the user
   expects immediate diagnostics on open. But for very large
   files (>10 MB), the initial parse could block the LSP
   handler for hundreds of ms. **Lean**: keep `didOpen`
   synchronous; if blocking on huge files becomes a real
   complaint, move analyze for `didOpen` onto a goroutine with
   progress reporting via `window/workDoneProgress`.
3. **Recovery when client and server fall out of sync.**
   Today's plan: log + best-effort continue. A user-facing
   "Reload window" command works as the manual fallback. If
   silent corruption is observed in practice, we could add a
   custom `protolsp/resyncRequired` notification ("close and
   reopen this file") to make the recovery explicit.
4. **Per-document edit history.** Storing the last N applied
   edits would let us implement "show me the recent edits to
   this file" as a debugging affordance. YAGNI for v1; flag as
   a possible future feature.
5. **Apply algorithm complexity at scale.** Naïve buffer
   rebuild is O(file size) per edit. For a 10 MB document with
   sustained typing, that's tens of ms per edit just on the
   apply step — measurable but not yet blocking. We can switch
   to a `bytes.Buffer`-backed approach (append-only with
   periodic compaction) if profiling shows the rebuilds
   dominate. **Lean**: ship naïve; revisit on profile.

## Decision log

| Date       | Decision                                                                                  | Why                                                                                          |
|------------|-------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------|
| 2026-05-16 | Switch to `TextDocumentSyncKindIncremental`                                                | Wire cost on every keystroke drops from O(file size) to O(edit size). Universal client support today. |
| 2026-05-16 | Negotiate `positionEncoding` with preference for UTF-8                                    | Eliminates conversion overhead on capable clients; falls back to UTF-16 cleanly.             |
| 2026-05-16 | Naïve copy-on-edit buffer rebuild                                                         | Simple + correct at the document sizes protolsp serves; optimize only with profiling evidence.|
| 2026-05-16 | Discard all derived state on every edit; let analyze rebuild                              | Position-shifting heuristics are an optimization for editor-grade reuse we don't need yet.   |
| 2026-05-16 | Analyze debouncing with a 150 ms window                                                   | Coalesces sustained typing; not so long that diagnostics feel laggy.                          |
| 2026-05-16 | `didOpen` stays synchronous (no debounce)                                                 | User expects immediate diagnostics on file open.                                              |
| 2026-05-16 | Out-of-sync recovery: log + best-effort; explicit resync is a future addition             | Manual reload is the fallback; no real failure mode observed yet.                            |
| 2026-05-16 | New `internal/positions` package owns offset conversion; supersedes M2's helper           | Centralizes the encoding-aware logic; helpers become thin wrappers.                          |

## Notes for future readers

- **The wire-cost win is by far the biggest single performance
  improvement available to protolsp today.** Get I1 in early;
  everything else stacks on top with smaller incremental gains.
- **Don't grow `Document.Apply` into a smart-merge function.**
  It applies LSP-spec edits to a byte buffer. If you find
  yourself adding semantic awareness (parsing the edit,
  rejecting "bad" edits, etc.), step back — the LSP client is
  authoritative for the document state.
- **The debounce window is the single tuning knob that affects
  perceived editor responsiveness.** Resist the urge to make it
  configurable until someone asks. A single well-chosen default
  removes a whole class of "tune your editor" support tickets.
- **The hover-during-debounce-window staleness is fine.**
  Users don't notice 150 ms of stale data; what they DO notice
  is editor lag when the LSP blocks. Optimize for the second.
