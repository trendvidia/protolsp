# Boundary contracts

Non-obvious contracts the rest of protolsp has to honor when calling into a library or another internal component. Every rule below corresponds to at least one bug we've already paid for; the goal is to not pay for it again.

When adding a handler, a new transport, a new schema source, or a new use of `conn`, read this file first. When you discover a new rule, add a section here rather than burying it in a code comment three layers deep.

---

## jsonrpc2: handlers must not block on `s.conn.Call`

**Rule.** Any LSP handler that needs to issue a server→client request (`workspace/configuration`, `client/registerCapability`, etc.) must spawn a goroutine and return before the call goes out.

**Why.** `go.lsp.dev/jsonrpc2`'s `conn.Go(ctx, handler)` runs each handler **inline on the read loop** unless it is wrapped with `AsyncHandler`, which this server does not do: requests must run in order. `conn.Call` sends a request and then waits for a response that **only that same read loop can deliver**. Inline call ⇒ the goroutine waits for a response it would itself have had to route. Hard deadlock. Every subsequent LSP message queues in the OS pipe unprocessed; the failure mode is silent on stderr and the symptom reaches a user as "feature stopped working" some time later.

**How to apply.** Mirror `dispatchInitialize`'s `initialized` follow-up: `s.someFollowup.Add(1)` → `go func() { defer s.someFollowup.Done(); ... }()` → `return nil, nil`. Expose a `WaitX` method so tests can drain the pending work before asserting.

**Tripwires.**
- `internal/transport/handler_warn.go` wraps the dispatcher and emits a Warn log when any handler takes >5 s. Visible in the trace panel the first time it triggers.
- `internal/transport/dispatcher_smoke_test.go` is a real-transport regression: `didChangeConfiguration` followed by `didOpen` must produce `publishDiagnostics` within 3 s. Fails the moment a new handler regresses.

**Bug paid for.** A jsonrpc2 dispatcher deadlock: the `didChangeConfiguration` follow-up ran inline, until it moved to a goroutine.

---

## jsonrpc2 v1: what a handler may keep, and what it may return

**Rule.** A handler must not keep its `ctx` or `req` after it returns, and must not return an error for a notification. `Server.serve` enforces both for everything under it: it swaps in `jsonrpc2.DetachContext(ctx)` and logs notification errors instead of returning them. Code that receives a context from the connection itself, like a `Stream`, must detach it before passing it to anything that outlives the call.

**Why.** jsonrpc2 pools the request and its context and recycles both when the handler returns. An error from a notification closes the connection, so one malformed message would disconnect the editor.

**How to apply.** New handlers go under `Server.handle` and get this for free. New `Stream` implementations mirror `wsStream.Write`.

**Tripwires.** `TestServe_NotificationErrorDoesNotReachConnection`, and the websocket tests under `-race`, which panic with `cannot create context from nil parent` if a recycled context is watched.

**Bug paid for.** `coder/websocket` keeps watching a write's context after the write returns, so passing the request context through crashed the server. The `internal/transport` tests caught it during the v1 migration.

---

## go.lsp.dev/protocol v1: which encoder handles what

**Rule.** Connections use `lspcodec.Codec`. LSP types go through `protocol.Marshal` and `protocol.Unmarshal`; everything else stays on `encoding/json`. Don't embed a protocol type in a struct to add a field.

**Why.** Sealed-interface unions need the protocol codec to decode. Its fallback for other types is json/v2, which treats `omitempty` and nil slices differently, so `protolsp/*` results would change shape. And `encoding/json` on a protocol type depends on the Go version: from Go 1.27 it honors the type's json/v2 hooks, so an embedded protocol type hides the outer struct's extra fields, while on Go 1.26 it ignores them and can't decode unions. Tests go through `testCodec` for the same reason. CI builds with the Go version in `go.mod`, so run the suite on that version too, not only on a newer local one.

**Tripwires.** `internal/lspcodec/codec_test.go` and `TestPositionEncoding_AdvertisedInInitializeResult`.

**Bug paid for.** The wrapper that added `positionEncoding` to `ServerCapabilities` silently lost the field on the wire during the v1 migration. A before/after comparison of server output caught it.

---

## go.lsp.dev/uri: `URI.FsPath()` is meaningless on non-file schemes

**Rule.** Before calling `URI.FsPath()` (or any path-derivation helper that ends up calling it), gate on `isFileURI(u)`.

**Why.** Editors open `registry://`, `inmemory://`, `untitled://`, and similar virtual URIs to surface read-only previews. `go.lsp.dev/uri`'s `FsPath()` returns a path for those too, so a non-file URI flowing through the wrong code path reads or watches a location that has nothing to do with the document. Before v1 this call panicked instead, so the mistake is now silent.

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
