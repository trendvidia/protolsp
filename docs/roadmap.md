# Roadmap

Drawn from the 2026-05-18 audit (security + tech debt + tests +
docs). Priority tiers:

- **P0** — correctness or security; block hosted exposure / fix soon.
- **P1** — schedule next quarter; compounds if ignored.
- **P2** — hygiene; do when adjacent code changes.

## Status

The audit produced 29 actionable items across four tiers. As of
the rollup, **everything non-parked has shipped**. The only
remaining entries are the H-cluster hosted-platform blockers,
deferred until the hosted pivot starts.

| Cluster | Status |
|---|---|
| P0 — concurrency (C1–C6) | ✅ shipped (#60, #61, #65) |
| P0 — resource exhaustion (R1–R4) | ✅ shipped (#63, #67, #69) |
| P0 — hosted-platform (H1–H4) | ⏸ parked until hosted pivot |
| P1 — tech debt (T1–T5) | ✅ shipped (#75, #76, #77, #78, #79) |
| P1 — test coverage (TC1–TC8) | ✅ shipped (#80) |
| P1 — documentation (D1–D8) | ✅ shipped (#71, #73, #74) |
| P2 — hygiene (Z1–Z6) | ✅ shipped (#81) |

## P0 — correctness & security

### Concurrency

- [x] **C1. Synchronize `Server.fileConfig` / `chain` / `editorConfig`.** Done in #60 via `Server.stateMu` + snapshot/swap helpers.
- [x] **C2. Synchronize `Document` analysis fields.** Done in #61 via per-doc RWMutex.
- [x] **C3. Race test for `Server.Close` while a sub-workspace rebuild is in flight.** Pinned by `TestSubWorkspace_Close_RaceWithConcurrentReads` in #60.
- [x] **C4. Race test for `Documents.Update` racing with hover / completion.** Pinned by `TestDocuments_UpdateRaceWithConcurrentReads` in #61.
- [x] **C5. `overridesMu` snapshot → apply → snapshot atomicity.** Done in #60: lock held across the full window via `applyOverridesLocked` + `snapshotSectionsLocked`.
- [x] **C6. `ensureSubWorkspace` lazy-load race.** Done in #65: holds `workspacesMu` across the load. Regression check stashed the fix; the race test fires on the pre-fix code.

### Resource exhaustion

- [x] **R1. Bound chain / config / proto file reads.** Done in #63: new `internal/safeio` package + `MaxConfigFileBytes = 4 MiB` ceiling at all five `os.ReadFile` sites.
- [x] **R2. Per-frame limit on stdio transport.** Done in #67: `limitedFrameReader` enforces `MaxStdioFrameBytes = 4 MiB`; oversized `Content-Length` rejected before any body bytes are read. Headers capped at 64 KiB.
- [x] **R3. Timeout async registry dials.** Done in #63: `schema.Open` wraps the dial in `context.WithTimeout(30s)`; caller-supplied earlier deadlines still win.
- [x] **R4. Evict idle `subWorkspaces` entries.** Done in #69: reference-driven eviction via `maybeEvictSubWorkspace` from `dispatchDidClose`. Re-discovers on the next open.

### Hosted-platform blockers — gating hosted multi-tenant protolsp-ws (parked)

Park until a hosted platform actually starts running protolsp-ws as a
multi-tenant service. Listed here because the items are
**protolsp's own tech debt** — code lives in this repo — even
though the urgency comes from hosted use. Each gates safe public
exposure once a tenant boundary exists; nothing's blocking
desktop / single-tenant LSP use today.

- [ ] **H1. WebSocket auth + per-conn sandbox.** `cmd/protolsp-ws/main.go` accepts every Origin-matching connection with full local filesystem + process-env access. Block public exposure until bearer/JWT auth + a filesystem / env sandbox land.
- [ ] **H2. `${env.X}` allowlist.** `internal/workspaceconfig/parser.go` reads any env var; a malicious repo can exfiltrate `AWS_SECRET_ACCESS_KEY` via `registry.token`. Refuse in hosted mode or allowlist by prefix.
- [ ] **H3. Path-containment on chain layer reads.** `chain_hover.go`, `chain_required.go`, `field_refs_definition.go`, `chain/chain.go` all `filepath.Join(root, layer.ResolvedPath)` with no `Rel` check or `EvalSymlinks`. Both `path = "../../etc/passwd"` and `${env}` substitution (env name `../../..`) escape.
- [ ] **H4. Sub-workspace dial failure traps stale state.** `workspace.go`'s sub-workspace rebuild updates `lastRebuiltCfg` before the dial succeeds; the next rebuild with the same (bad) cfg diff-skips and never retries.

## P1 — tech debt

- [x] **T1. Split `Server` into composed managers.** Done across #77 (lookupManager), #78 (configState), #79 (workspaceManager). Server's field count went 26 → 21 → 17 → 13; dispatcher now reads as a thin coordinator over three managers + pipeline knobs.
- [x] **T2. De-duplicate `effectiveRegistryConfig`.** Done in #75: extracted `mergeRegistryConfig(settings, file)`; root + sub-workspace callsites delegate. Adds `registrySourceToConfig` for the workspaceconfig→schema type conversion at the boundary.
- [x] **T3. Unify URI ↔ path conversion.** Done in #75: three near-identical helpers (`workspaceRootPath`, `documentFilePath`, `watchedURIToPath`) consolidated into `uriToFilePath(uri.URI) (string, bool)`.
- [x] **T4. Unify `${...}` interpolation.** Done in #76: new `internal/interp` package owns the canonical scanner (`FindAll`, `ReplaceAll`); workspaceconfig + server consumers delegate with their own policy filters.
- [x] **T5. Replace `errors.New("…" + err.Error())` with `fmt.Errorf("…: %w", err)`.** Done in #75: two sites in `configureSchemaLookup`; restores the error chain for `errors.Is`.

## P1 — test coverage

- [x] **TC1. `schema.Open` dial-error + `Validate` partial-config branches.** Partial in #63 (dial-error); the Validate branches landed in #80 via `TestValidate_PartialConfigRejected` (table test, 4 cases).
- [x] **TC2. Sub-workspace chain-file watcher positive case.** Done in #80: `TestWatchedFiles_SubWorkspaceChainSpecChangeReloads`.
- [x] **TC3. Sub-workspace deletion via watcher.** Done in #80: `TestWatchedFiles_SubWorkspaceConfigDeleteClearsFileConfig`.
- [x] **TC4. `pullEditorConfig` failure preserves seed.** Done in #80: `TestConfiguration_PullFailurePreservesSeed`.
- [x] **TC5. `utf16ByteOffset` invalid UTF-8 path.** Originally added in #80; subsequently obviated in #81 when `utf16ByteOffset` itself was deleted (Z2). `positions.Offset`'s own test suite covers the equivalent path.
- [x] **TC6. `formatValueRepr` non-scalar branches.** Done in #80: `TestFormatValueRepr_AllValueKinds` (12-case table).
- [x] **TC7. `scanTupleDatasetEnd` unclosed-header + EOF branches.** Done in #80: `TestScanTupleDatasetEnd_UnclosedHeaderExtendsToEOF`.
- [x] **TC8. BOM / CRLF document inputs.** Done in #80 + migrated to `positions.Offset` in #81: `TestOffsetUTF16_CRLFSplitsAtLF` + `TestOffsetUTF16_BOMCountsAsOneCodeUnit`.

## P1 — documentation

- [x] **D1. README: enumerate all 9 custom LSP methods + 3 notifications.** Done in #71: table-format section with design-doc links per method.
- [x] **D2. README: add `.protolsp.pxf` section.** Done in #71: Configuration leads with the four-tier model + a concrete `.protolsp.pxf` example; `initializationOptions` reframed as deprecated legacy seed path.
- [x] **D3. CHANGELOG: backfill "Unreleased" with W / C / S / R / CP / I phases.** Done in #73: full breakdown of the post-0.1.0 feature lines + hardening sweep, each anchored to its design doc.
- [x] **D4. `protolsp-mvp.md:110` references `nhooyr.io/websocket`.** Done in #74: updated to `github.com/coder/websocket` (the community continuation after the upstream archive).
- [x] **D5. Resolve open question 1 in `protolsp-mvp.md`** (empty-`@type` policy). Done in #74: moved to a new Resolved questions subsection.
- [x] **D6. `chainStatusChanged` notification promised but not implemented.** Done in #74: stripped the claim from `chain_status.go`, `layer_info.go`, `chameleon-integration.md` with explanatory notes pointing at the live-reload + `effectiveConfigChanged` paths that cover the same surface.
- [x] **D7. `dataset.hugeThreshold` documented as configurable via initOptions but never wired.** Done in #74: doc reframed around the compile-time defaults; pointer added for the future `.protolsp.pxf` config shape.
- [x] **D8. `workspace-config.md` ProtoFilesSource schema mismatch.** Done in #74: annotated the proto sketch — today's `proto/protolsp/v1` ships only `paths`; `exclude` / `include` were design-only.

## P2 — hygiene

- [x] **Z1. Delete `ErrServerClosed` sentinel.** Done in #81: sentinel + the defensive `errors.Is` checks in both transports.
- [x] **Z2. Move or delete `utf16ByteOffset`.** Done in #81: deleted; tests migrated to `positions.Offset`.
- [x] **Z3. Refresh `protolsp-mvp.md` package layout.** Done in #81: replaced the stale 6-file sketch with a high-level orientation reflecting the post-T1 manager shape.
- [x] **Z4. `configureSchemaLookup(ctx, raw interface{})` round-trips JSON for no reason.** Done in #81: signature takes `json.RawMessage`; new `initOptionsFromInitializeParams` extracts the bytes via a side-unmarshal.
- [x] **Z5. Replace `time.Sleep` in flaky-on-slow-CI test sites with `assert.Eventually` / `assert.Never`.** Done in #81: two sites converted (`debounce_test.go`, `workspace_lookup_test.go`). Race-test sleeps (intentionally fixed-duration race windows) left alone.
- [x] **Z6. Decision-log entry for `params.RootURI` fallback removal.** Done in #81: `config-protocol.md` decision log row ties removal to the `initializationOptions` seed-removal cohort.

## Out of scope (parked)

- VS Code extension migration to `workspace/configuration` — lives in `protowire`, not here.
- pxfed "Show Effective Config" sidebar — pxfed work.
