# Boundary contracts

Non-obvious contracts the rest of protolsp has to honor when calling into a library or another internal component. Every rule below corresponds to at least one bug we've already paid for; the goal is to not pay for it again.

When adding a handler, a new transport, a new schema source, or a new use of `conn`, read this file first. When you discover a new rule, add a section here rather than burying it in a code comment three layers deep.

---

## jsonrpc2: handlers must not block on `s.conn.Call`

**Rule.** Any LSP handler that needs to issue a server→client request (`workspace/configuration`, `client/registerCapability`, etc.) must spawn a goroutine and return `reply(...)` before the call goes out.

**Why.** `go.lsp.dev/jsonrpc2`'s `conn.Go(ctx, handler)` runs **a single reader goroutine** that does both message reading *and* handler dispatch (`conn.go:188-220` — no `AsyncHandler`, no separate response router). `conn.Call` sends a request and then waits on a channel that **only that same reader goroutine can fill** (via `case *Response` in `run()`). Inline call ⇒ the goroutine waits for a response it would itself have had to route. Hard deadlock. Every subsequent LSP message queues in the OS pipe unprocessed; the failure mode is silent on stderr and the symptom reaches a user as "feature stopped working" some time later.

**How to apply.** Mirror `dispatchInitialize`'s `initialized` follow-up: `s.someFollowup.Add(1)` → `go func() { defer s.someFollowup.Done(); ... }()` → `return reply(ctx, nil, nil)`. Expose a `WaitX` method so tests can drain the pending work before asserting.

**Tripwires.**
- `internal/transport/handler_warn.go` wraps the dispatcher and emits a Warn log when any handler takes >5 s. Visible in the trace panel the first time it triggers.
- `internal/transport/dispatcher_smoke_test.go` is a real-transport regression: `didChangeConfiguration` followed by `didOpen` must produce `publishDiagnostics` within 3 s. Fails the moment a new handler regresses.

**Bug paid for.** A jsonrpc2 dispatcher deadlock: the `didChangeConfiguration` follow-up ran inline, until it moved to a goroutine.

---

## go.lsp.dev/uri: `URI.Filename()` panics on non-file schemes

**Rule.** Before calling `URI.Filename()` (or any path-derivation helper that ends up calling it), gate on `isFileURI(u)`.

**Why.** Editors open `registry://`, `inmemory://`, `untitled://`, and similar virtual URIs to surface read-only previews. `go.lsp.dev/uri`'s `Filename()` only handles `file://` and **panics** on anything else. A non-file URI flowing through the wrong code path takes down the dispatcher goroutine.

**How to apply.** Every `dispatchDid{Open,Change,Close}` already guards. New handlers that touch a URI must do the same. `isProtoURI(u)` already implies `isFileURI(u)`.

**Bug paid for.** A panic in `didOpen` on a non-file URI, and the same guard extended to `didChange` and `didClose`.

---

## lookupManager: baseline diff-skip is silent

**Rule.** Any code path that knows a previously-tried registry dial outcome may now succeed (even though the registry *config* hasn't changed) must call `s.lookupMgr.InvalidateBaseline()` before kicking the rebuild.

**Why.** `BeginRebuild` short-circuits when the incoming cfg equals `lastRebuiltCfg`. That's the right behavior for the common case (don't re-dial when only non-registry fields of `.protolsp.pxf` changed). It's the *wrong* behavior when an out-of-band signal (BW_SESSION arriving, an auth credential rotating, the registry server starting up) flips the dial outcome without touching the config struct. Without invalidation, the failed dial stays sticky for the session and every downstream feature (secret detection, schema go-to-def) silently no-ops.

**How to apply.** Search for `s.lookupMgr.InvalidateBaseline()` — the existing call site in `SetBwSession` is the model.

**Bug paid for.** A Bitwarden session change that never re-dialed the registry, until it invalidated the lookup baseline.

---

## Server-initiated traffic requires `AttachConn` before `conn.Go`

**Rule.** `srv.AttachConn(conn)` must run before `conn.Go(...)`.

**Why.** Server-initiated traffic (`publishDiagnostics`, `window/showMessage`, the `workspace/configuration` pulls) all dial through `s.conn.Notify` / `s.conn.Call`. If `conn.Go` starts the dispatcher before `AttachConn`, an incoming `didOpen` can race ahead, fire `analyze → publishDiagnostics`, and dereference a nil `s.conn`.

**How to apply.** Existing transports already do this; the order is intentional. Any new transport should keep the pattern.

**Bug paid for.** Caught at design time, never landed — but the comment in `internal/transport/stdio.go:73` exists because the race was nontrivially possible.

---

## Document store: don't read `doc.Parsed` without `doc.mu.RLock`

**Rule.** Read-side handlers (`hover`, `definition`, `completion`, `documentSymbol`, etc.) must hold `doc.mu.RLock` for the duration of their read of `doc.Parsed` / `doc.SecretRanges` / `doc.LayerInfo`. Writers (`analyze`) hold `doc.mu.Lock` for the duration of the analyze pipeline.

**Why.** The analyze pipeline mutates these fields in place. A reader that grabs `doc.Parsed` without locking can see a half-populated entries slice or a stale FQN from a previous version.

**How to apply.** The lock acquisition lives at the top of the handler. `validate()` documents this in its docstring; new handlers should keep that discipline.

---

## When you hit a new boundary surprise

Add the section. Keep it short — rule, why, how to apply, the bug that paid for it. Examples from real bugs we've already fixed are the most useful part; abstract rules without a concrete failure mode age out fast.
