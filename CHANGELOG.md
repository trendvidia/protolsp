# Changelog

protolsp follows [SemVer](https://semver.org/). 0.x means the LSP
feature surface is settled but custom-method shapes
(`protolsp/sourceContent`, `protolsp/listMessages`) may still adjust
without a major bump.

## [Unreleased]

### Fixed

- **Format document works on v1.2 `.proto` files** (#9). It used to run
  `buf format`, which cannot parse `@validate`, `@required`, `function`
  or `type`, so formatting did nothing on those files. On some v1.2 files
  buf did succeed but printed them wrong: `@http(...)` came back as
  `http (...)`, and an `annotation` declaration lost its parameter list.
  Formatting now uses protocompile's own printer, with its buf-compatible
  preset, and no longer needs `buf` installed (`buf lint` still does).
  - Every annotation use and comment is kept. On a real v1.2 schema, all
    8 `@validate`, 3 `@required` and 62 comment lines survive. Annotation
    uses longer than 100 columns wrap, one argument per line.
  - Files without v1.2 syntax come out as `buf format` 1.74 prints them.
    In a corpus run over 3,552 `.proto` files, every non-fixture file
    differed from buf only where buf mangles v1.2 syntax.
  - If the printer's output would lose any word, number, string or
    comment word of the file, nothing is formatted and the reason is
    logged. No file in that corpus run triggers it.

### Added

- `analysis.FormatProto` formats `.proto` source passed in directly, for
  embedders whose live editor buffer can be newer than the Workspace's
  copy of the document.

### Changed

- **protolsp no longer needs the protobuf fork.** It moves to protoregistry
  v0.74.0, which keeps the namespaced registries in its own `nsregistry`
  package, and the `replace` for `google.golang.org/protobuf` is gone.
  `go install …@latest` now works, and modules importing protolsp can
  remove the `replace` they copied. **Breaking for library users:**
  `schema.WithFallback` and `schema.ProtoFilesParents` now use
  `*nsregistry.NamespacedFiles` and `*nsregistry.NamespacedTypes`.
- Dependencies moved to their latest releases. `cel-go` now comes from
  its new module path, `cel.dev/cel-go` (v0.33.1), which protovalidate
  v1.4.0 requires.
- **`go.lsp.dev` protocol, jsonrpc2 and uri moved to v1.0.1** (#15), a
  rewrite of those libraries generated from the LSP 3.18 specification.
  Editors see the same messages: a scripted session of 45 messages
  across hover, completion, rename, quick fixes, non-ASCII edits and
  `protolsp/*` methods is byte-identical before and after. Two things
  differ.
  - `protolsp/setOverrides` now sends its config-changed notification
    before its response, not after.
  - `analysis.SemanticTokensProvider` is gone, because
    `protocol.SemanticTokensOptions` replaces it. `analysis` types that
    wrap protocol fields changed with the library.
- **protocompile v0.35.1 → v0.36.0**, for the printer fixes above
  (trendvidia/protocompile#258, #273, #274). It also brings the parser's
  new diagnostic for a run of tokens that start no declaration.
- **`proto/pxf` is now an alias of `github.com/trendvidia/protos/pxf`**
  and registers nothing itself. protocompile v0.36.0 links that package,
  and two registrations of `pxf/annotations.proto` made every protolsp
  binary panic at startup. The package's exported names are unchanged.
  `buf generate` now skips `pxf/`, so it cannot bring the duplicate back.

## [0.23.1] – 2026-10-29

### Changed

- **protowire v1.12.0 → v1.13.0**, the module protolsp reads the schemas a
  document's `@proto` may import from. Those files change in comments only
  (`pxf/bignum.proto` describes the BigFloat precision suffix). It also
  brings protocompile v0.35.1 (from v0.34.0) and grpc v1.84.0. No change
  an editor can see: the test suites pass unchanged, with and without
  `-tags protocheck`.

## [0.23.0] – 2026-10-29

### Added

- **Types defined by the document's own `@proto` are resolved**.
  The draft makes an embedded schema authoritative, but protolsp read
  `@proto` only for symbols and highlighting, so a self-describing
  document got no validation and no Secret masking. Now a document's
  `@proto` directives are compiled when it is analyzed, and its types are
  looked up before the workspace registry's. That gives such a document,
  with or without a registry configured: body type errors and unknown
  fields, `@dataset` cell checks, and Secret masking.
  - All four shapes compile as protowire's `pxf` CLI compiles them:
    source, named (`@proto t.v1.Trade { ... }`), anonymous (bound to the
    next typeless `@dataset`, in document order), and descriptor (a
    FileDescriptorSet).
  - Imports resolve only from the draft's list, never from disk or a
    registry: the other `@proto` directives, five canonical
    schemas (`pxf/annotations.proto`, `pxf/bignum.proto`,
    `pxf/secret.proto`, `sbe/annotations.proto`,
    `envelope/v1/envelope.proto`, decided on protowire#369) and eleven
    `google/protobuf/` well-known types (protowire#388). Anything else,
    such as `docs/v1/topic.proto`, `google/protobuf/compiler/plugin.proto`
    or the `protowire/X` spelling, is reported as `pxf validate` reports
    it. protowire's `testdata/proto-imports` fixtures are copied into
    `analysis/testdata` and run as a test.
  - New diagnostics, source `protolsp/proto`: a `@proto` that does not
    compile, reported on the line that caused it; an anonymous `@proto`
    with no typeless `@dataset` after it; and a typeless `@dataset` with
    no anonymous `@proto` before it. Two identical definitions of one name
    are one type, and diverging ones are an error. An error inside an
    indented triple-quoted body lands on its own line too.
  - A `@proto` that does not compile takes only its own types with it:
    the other directives' types, and the Secrets they declare, still
    apply while it is being edited.
  - The compiled schema is reused until a `@proto` or the anonymous
    bindings change, so typing elsewhere in the document does not
    recompile it.
  - Hover on a `@dataset` of an in-document type resolves it, and says it
    is defined in the document's `@proto`.
  - A message about an anonymous `@proto` row type calls it "the
    anonymous @proto row type", not by the name protolsp gives it.

### Fixed

- **The `@dataset` cell check agrees with the decoder**. It looked a
  column up as one field name and matched a cell's literal against the
  field's protobuf kind alone, so it disagreed with the decoder both ways.
  Measured against protowire's `pxf validate`:
  - Valid documents it flagged: every dotted column (`unknown column
    "db.pw"`, live since protowire-go v1.10.0 made them parse); an integer
    in a `double` or `float` column; a message type in its short form (a
    wrapper as its inner literal, `pxf.Secret` as a string, `pxf.BigInt` as
    an integer, `pxf.Decimal` / `pxf.BigFloat` as a number); a keyed
    repeated field in its block form.
  - Invalid ones it missed: a string in a `bytes` column, bytes in a
    `string` column, `null` on a plain scalar, repeated or map field, a
    scalar in a repeated or map column, a list or scalar in a message
    column; an identifier anywhere but an enum (or `inf` / `nan` in a
    float); a list element or map value of the wrong kind, or `null`; a
    map entry written `k = v`.

  A dotted column is now checked against the leaf field its path names. A
  path the binder rejects (a segment naming no field, a path through a
  repeated, map or scalar field, or into a literal-form type) is reported
  at the directive, worded as the binder words it. A mismatch says what
  the field takes (`expects a list`, `expects string or a block`), and one
  inside a list or map is reported at the element or entry. A message
  block's contents are still left to the decoder.

- **A `@dataset` row the decoder rejects gets a diagnostic**. The
  cell check decides from a cell's literal kind, so it never looked inside
  a message block, and it left value checks to the decoder. A row whose
  cells all pass that check is now bound with protowire-go's
  `pxf.BindRow`, and its error goes on the cell it names. That covers
  what a block holds (`{ x = 1 }` for a string `x`, an unknown field, a
  keyed entry without a block, also inside list elements and map values),
  a name the enum lacks, a map key its key type cannot parse, and an int
  out of range. The decoder stops at its first error, so a row gets at
  most one such diagnostic. A row the kind check already faults is not
  bound. On the largest region that is still validated (999 rows, 93 KB)
  validation takes about 3.3 ms, up from 0.08 ms; parsing that document
  takes 0.95 ms.

- **`pxf.Secret` values in `@dataset` rows are masked**. The secret
  walk ran only over a document's body, and only after its `@type` check,
  so a dataset document (which has no `@type`) recorded no secret at all:
  editors showed every cell in plain text, and no diagnostic or hover was
  redacted for one. Rows whose type resolves are now walked too. A cell
  binds as `<column> = <cell>` would in a body, so every column form is
  covered:
  - a Secret column with a string cell (`"x"`) or a block cell
    (`{ value = "x" hint = "h" }`);
  - a dotted column to the value (`pw.value`, `db.pw.value`). `pw.hint`
    and `pw.fingerprint` stay visible, as in a body;
  - a column into a sub-message holding a Secret (`db.pw`, or `db` with a
    block cell);
  - list and map cells.

  A cell's path is `row<N>.<field path>`, counting rows from 1
  (`row2.db.pw`), so the literal-secret quick fix gives each row its own
  env-var name (`ROW2_DB_PW`). A dataset bound to an in-document `@proto`
  is walked too (above).
- **The body walk takes every value form a Secret can appear in.** It
  recorded only `pw = "x"` and `pw { ... }`. Now it also records
  `pw = { value = "x" }`, a message written as `db = { pw = "x" }`, list
  elements (`keys = ["a", { value = "b" }]`, paths `keys.0`, `keys.1`)
  and map values (`tenants = { "acme": "k" }`, path `tenants.acme`).

### Changed

- **protowire-go v1.10.0 → v1.12.0**. What an editor now sees
  from the decoder:
  - `null` on an `optional` scalar is accepted and clears it
    (protowire-go#182), where protolsp reported an error. The `@dataset`
    cell check follows the same rule, so a `null` cell in an `optional`
    scalar column is no longer reported.
  - A singular field bound twice in one body is an error
    (protowire-go#184), and so is a map key bound twice (protowire-go#191).
  - A `pxf.BigFloat` literal may carry a precision suffix,
    `0.10000000000000001p53` (protowire#406, protowire-go#194), which is
    what `pxf.Marshal` now writes. It parses, formats unchanged, and is
    highlighted with its suffix; hover shows it too. A suffix on any other
    field, in a body or a `@dataset` cell, is reported with the decoder's
    message.
- **protocheck-govm v0.1.0 → v0.2.0** (`-tags protocheck` builds): built
  against protocheck v2.7.0, so the module graph no longer names the
  deleted v2.4.0 tag. No behaviour change.
- **protocheck v2.5.0 → v2.7.0** (`-tags protocheck` builds). protocheck
  was re-created as a public repository without its earlier git tags. A
  clean tagged build that fetches trendvidia modules from git (`GOPRIVATE`,
  as CI does) failed with `unknown revision v2.5.0`; through the public Go
  proxy, which keeps its cached copy, v2.5.0 still resolved. Visible change: a `check` rule on a message-typed field (a
  Timestamp, a nested message) used to be skipped and always pass; with
  the expression engine protolsp configures, it now reports a violation
  saying the engine cannot evaluate it. Rules on message-valued map
  entries are evaluated too.
- **protowire-go pinned at v1.10.0** (from v1.5.0), which parses the
  `@dataset` forms above: list and block cells (protowire#341) and dotted
  columns (protowire#342). It also brings protocompile v0.34.0. Behaviour
  an editor can now see from the decoder: `null` on a plain scalar field is
  a schema error (protowire#339); `(pxf.default)` and `(pxf.required)`
  apply inside list elements, map values and `Any` bodies (protowire#352);
  and an `Any` block may open with `@type = "..."` (protowire#354).
- **protolsp depends on protowire v1.12.0** for `protowire.BundledProto`,
  the canonical schemas an `@proto` may import. It also raises
  grpc to v1.83.2 and golang.org/x/net to v0.58.0, through protowire's
  requirements.
- **`DatasetRegion.resolvable` is specified for a typeless `@dataset`**
 . A region bound to an anonymous `@proto` is sent with an empty
  `type_fqn` and `resolvable` true: its row type is known, but the name
  protolsp gives it is internal. The field comments in
  `protolsp/v1/rpc/datasets.proto` and on `analysis.DatasetRegion` now
  say so. Nothing on the wire changed.

## [0.22.0] – 2026-09-07

### Changed

- **protocompile pinned at v0.33.0**, the tagged release that supersedes
  the `trendvidia`-branch pseudo-version carrying protocompile#212. What
  the tag adds for an editor: a numeric literal whose value has a
  billion digits no longer hangs the analyze pass (protocompile#210).
  Measured on the same document, `int64 f = 1 @default(1e999999999)`
  and `double f = 1 @default(1e-999999999)` did not return within ten
  seconds on the previous pin and return in about 20 ms on this one,
  the first with the compiler's out-of-range diagnostic. `pxf.BigFloat`,
  `pxf.Decimal` and `pxf.BigInt` defaults are bounded by what the wire
  and the decoder accept (protocompile#217, protocompile#216), and the
  vendored `descriptor.proto` comments describe RFC-001 §5.4
  (protocompile#213). No protolsp code changes.

## [0.21.0] – 2026-09-07

### Added

- **`@validate` rules are checked at edit time against RFC-001 §5.4,
  with a quick-fix for a misspelled function**. protocompile
  now parses every expression argument against the normative
  expression language (protocompile#211): a syntax error, a call that
  is neither a visible `function` declaration nor a §5.4 builtin, and
  a builtin with the wrong arity are compile errors, and they arrive
  through the existing compile pass as `protolsp/proto` diagnostics
  anchored on the callee name. protolsp adds the part only an editor
  can: for an unresolved call it ranks the visible declared functions
  by edit distance, appends `did you mean "matches"?` to the message,
  and offers `Replace "mathces" with "matches"` as a quick-fix — bare
  for same-package functions, fully qualified for imported ones (the
  distance-zero case: `is_email(this)` written where only
  `lib.is_email` resolves). Declared-function arity, unknown methods
  on `this` and syntax errors keep the compiler's message alone.

### Changed

- **protocompile pinned at v0.32.1-0.20260907104127-8112f1d753b2**
  (the `trendvidia` branch head carrying protocompile#212), up from
  v0.25.0. Two consequences: `protolsp/engineConfig` now reports an
  empty `engine` when no configuration file sets one — RFC-001 §9.4
  reserved the field (protowire#282) and retired the `cel` default —
  and `@validate` rules that use field selection (`is_email(this.value)`)
  are compile errors, since §5.4 has no field selection: a rule over a
  message's fields is a declared function called with `this`. The
  generated protovalidate SDK is pinned explicitly in `go.mod` because
  its version strings sort by protobuf-go prefix before date, and the
  older SDK protocompile requires would otherwise win and drop the
  types protovalidate v1.2.0 needs.

### Fixed

- **`buf/validate/cel` diagnostics compile against protovalidate's own
  library**. The schema-side analyzer built a bare cel-go
  environment with only `this` declared, so every rule using a
  protovalidate function — `this.isEmail()`, `this.isHostname()`,
  `this.lowerAscii()`, `this < now` — was reported as an undeclared
  reference in the editor and then validated fine at run time. Rules
  now compile against `protovalidate/cel.NewLibrary()` (the strings
  extension, the `now` variable, the `is*` / `unique` / `getField`
  functions, the bytes overloads, cross-type numeric comparison),
  built once and extended per rule with the typed `this`, so what the
  runtime accepts is silent and what it rejects (`math.greatest`,
  optional syntax) is reported. If the library ever fails to load
  the analyzer goes silent rather than falling back to the bare
  environment.

### Added

- **`this` is typed in PSE engine-expression checks** (advances
  the issue; its syntax-error and measured-inventory items stay
  open). The `protowire/cel` pass now binds `this` to the
  annotated declaration's type wherever RFC-001 §6.2 and §6.4 define
  it unambiguously — predeclared scalars to their CEL scalar,
  wrappers unwrapped, `Timestamp`/`Duration` native, `repeated` and
  `map` fields as the collection on a field-level rule, type aliases
  to their concrete base through a chain — and reports every
  checker finding, not just unresolved names: `this.startsWith("a")`
  on an `int32` field, `this.foo` on a string, a comprehension over a
  scalar. Enums, `Any`, other message-typed fields and message-level
  rules keep a dyn `this`, so nothing about their shape is asserted.
  An alias's rule keeps the alias's binding on a `repeated` field
  using it (§6.4 validates per element). Parse errors remain silent.

- **Unresolvable names in PSE engine expressions are diagnosed**
 . A `@validate` rule — any argument bound to an
  `expression`-typed annotation parameter — naming a function that
  is neither a declared `function` nor a builtin of the configured
  engine now gets an error on the name (`unresolved function
  "mathces": … did you mean "matches"?`), with a quick-fix per
  nearby declared function (same-package names bare, imported ones
  fully qualified). protocompile presumes an unresolvable name is an
  engine builtin (RFC-001 §8.1), so a misspelled function compiled
  clean and silently never ran. The engine resolves through the same
  §9.4 loader as `protolsp/engineConfig`; under `cel` (the default)
  each fragment compiles against cel-go's full surface — standard
  library and macros, every extension library, optional types — plus
  `now()` and the declared functions the compiler resolved, and only
  unresolved references and builtin overload mismatches are
  reported. Declared-function arity stays protocompile's finding
  (never reported twice), syntax errors stay silent, and every other
  engine (`starlark`, `go`, unknown, or a malformed config) reports
  nothing from this pass at all — pinned, so it cannot turn into
  false positives on a project protolsp cannot compile for. Source
  `protowire/cel`.

## [0.20.0] – 2026-08-02

### Added

- **Workspace declared-name export**. New
  `analysis.Workspace.DeclaredNames()` accessor returns the
  workspace's declared RFC-001 name sets — annotation, type
  (aliases plus top-level messages/enums), and function names, each
  bare and package-qualified — so editor hosts can feed a line-based
  declared-name highlighter (bellui x/editor's `DeclaredNames`
  middle path) without re-deriving declaredness from token text.
  Sources: workspace-local indexed files, open documents'
  keystroke-fresh ASTs, their indexed imports, and — for imports the
  index doesn't hold — the schema lookup's compiled RFC-001 carrier
  options. New `protoindex.Index.LocalImportPaths()` scopes the
  index walk to workspace-local files so the export never
  force-parses the BSR module cache.

## [0.19.1] – 2026-07-26

### Changed

- **protocheck expression engine pinned to its first release tag**
  (was a pseudo-version of the then-untagged main). Same code plus
  its own protocheck/v2 requirement raised to v2.4.0, matching what
  MVS already resolved. No behavior change.

## [0.19.0] – 2026-07-26

### Added

- **§7 catalog i18n for protocheck data diagnostics** (
  unblocked by protowire#194 / protowire v1.7.0). The engine
  config's `catalog_libraries` are loaded through protocompile
  v0.23.0's shared `catalogs` reference loader (text-format
  `protowire.schema.catalog.v1.Catalog`, paths relative to the
  config file) whenever the validator is (re)built, registered on
  the engine per §7, and used to render violation messages in the
  client's `InitializeParams.locale` — BCP 47 negotiation is
  case-insensitive exact match, then the primary language subtag,
  then any catalog sharing it; any miss falls back to the spec
  message. Catalog files are watched (the engine-config glob widens
  to `**/*.textproto`; the classifier matches only the loaded
  catalog set, so unrelated textproto edits are dropped), and the
  rebuild fingerprint folds catalog *content*, so an on-disk
  translation edit re-renders open documents' diagnostics without
  an LSP restart. Load failures degrade to fallback messages with a
  warning — never disable validation.

- **"Go to rule" links on protocheck data diagnostics**.
  Findings produced by RFC-001 carrier rules carry the v2
  `SourceRef`/`Source` enrichment; protolsp now resolves it through
  the same URI precedence go-to-definition uses (proto_files local
  path → workspace file → registry virtual doc → internal) and
  renders it as `DiagnosticRelatedInformation` ("constraint declared
  here") anchored at the declaration site — a violation on a `.pxf`
  value links back to the `@validate`/`@required` that imposed it.
  protocheckpb `check` rules carry no source map and render without
  a link, unchanged.

- **`protolsp/engineConfig`**. New custom request returning
  the workspace's effective RFC-001 §9.4 engine configuration —
  engine, function/catalog libraries, default execution mode,
  recursion depth, strict validation — plus which precedence tier
  supplied it (`discovered` / `env` / `flag` / `defaults`) and the
  resolved file path. Resolution goes through protocompile's shared
  `engineconfig` loader (nearest `protowire.config.textproto` walking
  up from the workspace root, `PROTOWIRE_CONFIG`, built-in defaults),
  the same loader `pxf build` uses — so the editor and the build tool
  can never disagree about the effective config. Resolution is per
  request against current disk state; a malformed config is reported
  in the response's `error` field per §9.4's no-silent-fallback rule,
  never a request failure.

- **`@sensitive(class)` support** (protowire#163). Two
  halves:
  - **Class-name completion.** RFC-001 §6.7 pins the sensitivity
    taxonomy as *open and org-defined* — no fixed enum — so the only
    candidate source is the workspace's own usage. Inside the
    argument parens of a use resolving to
    `protowire.schema.v1.sensitive` (fully qualified, resolved
    through imports, or imported under the canonical
    `protowire/schema/v1/annotations.proto` path), completion offers
    every class already used by a `sensitive` annotation across the
    current buffer and the proto index: bare names inside the class
    string literal, quoted outside it. A user annotation that
    happens to be named `sensitive` keeps its ordinary argument
    completion.
  - **Reserved-prefix diagnostic** — via the dep bump, not custom
    code: protocompile v0.21.0 rejects `class` values that are
    `"protowire"` or a dotted extension, so the compile pass now
    surfaces the §6.7 MUST-reject as a positioned `protolsp/proto`
    error (closing the LSP-side gap protolsp would otherwise have
    had to flag while trendvidia/protocompile#123 was open).

### Changed

- **Embedded engines migrated to the protocheck v2 seam** . The Phase 1 data validator moves from
  protocheck v1.2.0 to `protocheck/v2` v2.4.0 with expressions
  evaluated through the RFC-001 §9.4 `ExpressionEngine` seam.
  Fallout across the surface:
  - **`function_libraries` are honored.** The validator is rebuilt
    against the effective §9.4 engine config — on initialize, on
    config edits (including a new `**/protowire.config.textproto`
    file watch), and on schema-lookup swaps, fingerprint-diffed so
    unchanged config is free. Library import paths resolve through
    the schema lookup and feed `WithDescriptors`, so RFC-001 carrier
    rules (`@validate`/`@required`) on library types now validate
    `.pxf` data, and a rule calling a declared-but-unimplemented
    function surfaces the reserved `protowire.function.unimplemented`
    code — downgraded to Information, since the data isn't known to
    be wrong. `default_mode` and `strict_validation` are deliberately
    not honored: the LSP always collects all violations and never
    registers function implementations.
  - **v2 violation model.** Findings carry the structured path's
    canonical string, the stable violation `code`, the spec fallback
    message, and `for_key` — map-key violations render the same
    ` (key)` suffix as `Report.Format` and the runtime overlay.
  - **Phase 0 unsigned binding fixed.** The expression type-checker
    bound `uint32/uint64/fixed32/fixed64` as signed ints while the
    runtime engine binds them as `uint`; the two now agree (a uint
    field compared against a negative literal is a Phase 0 type
    error). Phase 0 stays on a direct expression compile — the seam
    is evaluate-only and the checker needs `CompileError` offsets —
    but its binding rules are pinned to the runtime engine's.

- **Function hover docs from `FunctionDecl.options`**.
  Hovering a function resolved through a compiled carrier (registry /
  descriptor-only imports, including calls inside opaque engine
  expressions) now renders the documentation-bearing bracket options
  protocompile v0.20.0+ lowers into `FunctionDecl.options`: a
  `*Deprecated.*` marker, the `description` paragraph in the
  doc-comment slot, and the `error_code`. Non-doc options
  (`openapi_map` and friends) stay out of the card; pre-v0.20.0
  carriers render unchanged.

- **Runtime-violation overlay: `sourceRef` join + `forKey` rendering**.
  `protolsp/setRuntimeViolations` violations now carry the
  two fields protowire's `EnrichedViolation` gained for exactly this
  join (protowire#161/#125), so producers forward Report violations
  without re-keying:
  - `sourceRef {file, descriptorPath}` — the RFC-001 §8.3.1 rule
    identity. When present it is the join key (the positional
    `descriptorPath` is ignored); the `file` half is suffix-matched
    path-boundary-aware against the document's source-map file
    (producers carry module-relative artifact paths, protolsp compiles
    open documents under absolute filenames), and a mismatch drops the
    violation rather than joining another file's ordinals.
  - `forKey` — marks a map-entry *key* violation. Key and value
    violations of the same entry-level rule join to the same anchor,
    so the flag renders as a ` (map key)` message suffix.

  Both fields are optional; existing producers are unaffected.

- **protocompile bumped v0.17.0 → v0.20.0**. Three releases in
  one hop; two of them re-key the descriptor paths the runtime-violation
  overlay (`protolsp/setRuntimeViolations`) joins on:
  - **v0.18.0**: alias-expanded `AnnotationList` entries emit in RFC-001
    §6.4 evaluation order (base→derived), so `elementPath[fqn#ordinal]`
    ordinals for multi-link alias chains re-key — ordinal 0 is now the
    base-most link's rule.
  - **v0.19.0**: map *value*-type alias rules key on the map field's own
    path; the pre-v0.19 synthetic `…Entry.value[…]` emission is gone.
    *Key*-type alias rules stay on the synthetic entry's key field.

  protolsp holds no persisted descriptor paths — the source map is
  re-derived from the current IR on every analyze pass, so the overlay
  joins on the new shapes automatically once producers re-derive theirs.
  New regression tests pin the v0.20.0 contract end-to-end: alias-chain
  ordinal anchors, map value/key alias paths, bare-path
  `TYPE_REFINEMENT` entries, and that stale old-shape keys miss (drop
  from the publish) rather than mis-join. Also picks up v0.17.1's
  declaration-site locations on the 50401–50403 carriers and v0.20.0's
  populated `FunctionDecl.options` + `EnrichedViolation.for_key` /
  `source_ref` bindings (feature work tracked separately).

- **protocompile bumped v0.20.0 → v0.21.0.** Brings the
  reserved-prefix rejection above and fully-qualified
  `TypeDecl.base_type_fqn` in the 50403 carrier (bare in-package
  bases like `Email` now render as their FQN in carrier hover
  cards). No descriptor-path changes.

- **protocompile bumped v0.21.0 → v0.22.0.** Message literals as list
  elements in annotation arguments now compile (`@meta([Item{…}])`,
  RFC-001 §8.1), and broken or cyclic type-alias bases are diagnosed
  at the declaration site even when unreferenced — sources that
  previously compiled while carrying unreferenced broken aliases now
  error, positioned on the alias itself (spec-correct tightening).

- **protocompile bumped v0.22.0 → v0.23.0.** Picks up the
  `catalogs` loader; no compiler-surface change.

- **protowire-go bumped v1.3.0 → v1.3.1** (the validation-seam
  release). Additive decoder-side API; no changes to any surface
  protolsp uses.

## [0.18.2] – 2026-07-23

### Changed

- **Expression engine bumped** (the `eval.Uint` release). Adds a
  lossless unsigned 64-bit binding type, so protobuf
  `uint64`/`fixed64` fields no longer have to be narrowed to `int`
  (wrapping negative above `math.MaxInt64`) when bound into
  `(protocheck.field)` / `(protocheck.message)` expressions. Also picks
  up the cross-VM boxed-integer extraction race fix for pooled
  evaluation. No bytecode-format bump; no transitive dependency
  changes. Full suite verified with and without `-tags protocheck`.

## [0.18.1] – 2026-07-23

### Changed

- **Expression engine bumped** (its security-review release line).
  protolsp only uses the engine for compile-time expression
  checking (`(protocheck.field)` / `(protocheck.message)` diagnostics);
  that package is byte-identical across the two tags, and the
  security work applies to parts of the engine protolsp never
  reaches. `CompileError`
  offset/length pinning verified unchanged by the existing
  `-tags protocheck` analysis tests. Transitively pulls minor
  `golang.org/x/*` security bumps.

## [0.18.0] – 2026-07-18

### Added

- **General block folding**. A standard pull-based
  `textDocument/foldingRange` provider now covers block structure in
  every file protolsp handles, so editors can collapse to read
  high-level structure:
  - **`.pxf`**: one fold range per multi-line block (`children`, `state`,
    `lifecycle`, nested nodes, map / list values) and per multi-line
    directive block (e.g. `@header … { … }`).
  - **`.proto`**: a fold per `message` / `enum` / `service` / `oneof` /
    `extend` / `rpc` body (recursively, including nested types), plus the
    leading import group (`kind: imports`) and comment blocks
    (`kind: comment` — multi-line block comments and runs of adjacent
    line comments).

  Single-line blocks aren't foldable. This is pure structure — no
  enrichment (row counts, huge/auto-collapse hints, summaries), which
  stays in the specialized `@dataset` regions push
  (`protolsp/datasetRegionsChanged`), layered on top unchanged.

## [0.17.1] – 2026-07-18

### Fixed

- **Bundled `proto/pxf/annotations.proto` refreshed with `(pxf.key)`**
 . The bundled annotations file (and its generated
  `annotations.pb.go`) defined only `(pxf.required) = 50000` and
  `(pxf.default) = 50001`; it was missing `(pxf.key) = 50002`, the
  extension protowire#116 added for keyed repeated fields. Runtime keyed
  detection was unaffected (`pxf.KeyField` reads field 50002 directly),
  but a consumer resolving `import "pxf/annotations.proto"` from
  protolsp's bundled include path hit an unknown extension when compiling
  a keyed schema. The bundled copy now matches protowire's canonical
  version.

### Tests

- **Keyed outline/diagnostics verified against AppViewer's published
  document schema** (appviewer#331). A hermetic regression test
  compiles AppViewer's structural `document.proto` from disk through the
  real `schema.OpenProtoFiles` (protocompile) path — resolving its
  `import "pxf/annotations.proto"` against protolsp's own bundled
  `proto/` tree — and, over a real `@type appviewer.ui.v1.Screen`
  document, asserts protolsp treats `Node.children` as keyed
  (`(pxf.key)="id"`), outlines each child by its node id, and flags a
  duplicate node id (`protolsp/keyed`). Resolving against the bundled
  annotations proto (rather than a vendored fixture copy) makes this the
  regression guard for the bundled-annotations fix.

## [0.17.0] – 2026-07-18

### Added

- **Keyed repeated fields** (protowire#116). A `repeated <Message>`
  field carrying the `(pxf.key)` schema option now gets first-class
  editor semantics across every surface, driven purely from the resolved
  schema — no per-app configuration:
  - **Outline**: entries of a keyed field appear as named symbols keyed
    by their entry name (e.g. a UI document's `children` block outlines
    by element name), nested recursively against the element message.
  - **Diagnostics** (source `protolsp/keyed`, via
    `pxf.KeyedDiagnostics`): duplicate entry names, empty keys, explicit
    key-field assignments that disagree with their entry name, and quoted
    entry names outside a keyed field — surfaced live while editing. Entry
    names and their bodies are no longer mis-reported as unknown fields
    on the parent, and the strict decoder's keyed error is suppressed
    from `protolsp/schema` to avoid double-reporting.
  - **Completion / hover**: inside a keyed entry's body, completion and
    hover resolve against the element message (not the parent's fields);
    at entry-name position completion offers nothing (the key is
    free-form); hovering an entry name reports the element type.
  - **Rename**: renaming a keyed entry rewrites the entry name token —
    quoting a non-identifier key and unquoting an identifier-safe one to
    match `pxf fmt` canonicalization — and retargets any redundant
    explicit key assignment inside the entry atomically. The rename
    propagates across the workspace by the entry's identity (its keyed
    collection path + key), rewriting every occurrence — chain layers
    that define or override the same entry — over the same file corpus
    (open buffers + on-disk `.pxf`, sub-workspace boundaries, truncation)
    the path-based field rename walks. Dispatch accepts non-identifier
    keys for keyed entries while still requiring a valid identifier for
    ordinary field renames.
  - Requires protowire-go v1.3.0 (keyed descriptor helpers + decode
    diagnostics + canonicalizer).

## [0.16.0] – 2026-07-17

### Added

- **Runtime-violation overlay**. The new
  `protolsp/setRuntimeViolations` custom notification feeds RFC-001
  `(file, descriptor_path)`-keyed runtime violations into the
  diagnostics publish path. Params:
  `{ textDocument: { uri }, violations: [{ descriptorPath, message }] }`
  — full replace per document (an empty list clears). Each publish
  re-resolves the stored keys through the document's canonical
  `protowire.schema.v1` source map and renders hits as
  `Information` diagnostics with source `protolsp/runtime`, so
  editors can color / filter the overlay independently of
  `protolsp/proto` and `buf/lint`. Violations survive re-analysis
  while the mapped construct exists, drop from publishes when its
  key stops resolving (edited away, or the file no longer compiles),
  and reappear when it resolves again — the store only changes on
  the next notification.

- **RFC-001 completion + navigation**. File-scope
  completion now offers the `type` / `function` / `annotation`
  declaration keywords; a cursor after `@` (registered as a
  completion trigger character) completes the annotation names
  declared in the current file and — package-qualified — in its
  imports, with a typed qualifier narrowing to that package's bare
  names. Hover on a `@name` use site (leading, trailing, or the `@`
  sigil itself) and on an `annotation` declaration's own name renders
  a declaration card with the parameter signature and leading doc
  comment; go-to-definition on a use site jumps to the declaration,
  in-file or through imports. Resolution mirrors the message/enum
  type-reference resolver: in-file first, then imported files with
  dotted qualifiers matched against the imported package; registry://
  previews stay scoped to in-file targets.

- **RFC-001 type/function navigation + descriptor fallback**. Hover and go-to-definition now cover the remaining
  RFC-001 constructs: `type` aliases resolve everywhere type
  references live (field types, alias values, function/annotation
  parameter types) and `function` declarations resolve from parsed
  annotation arguments — in-file, cross-file through imports, and in
  registry previews. References into imports the workspace index
  doesn't hold resolve through the schema lookup: served source is
  fetched and parsed when available (full doc-comment cards, exact
  ranges), else the compiled descriptor's protowire.schema.v1
  carrier options (file-level type_decls / functions /
  annotation_decls, each with its declaration-site SourceLocation)
  render signature cards and registry:// jump targets. Pre-v1.2
  descriptors carry no extensions and degrade to the AST-only
  behavior.

- **Remote fallback for classic message/enum references**.
  A field type naming a message or enum in an import the workspace
  index doesn't hold (uncached BSR deps, registry-only files) now
  hovers and jumps through the same two-layer schema-lookup fallback
  Phase C built for RFC-001 declarations: served source is fetched
  and parsed when available (doc-comment cards, exact name-token
  ranges), else the compiled descriptor itself answers —
  `fd.Messages()`/`fd.Enums()` find the declaration and its
  SourceCodeInfo supplies the declaration-site range. Tracked
  namespaces yield registry:// jump targets; untracked origins hover
  but decline to jump; a name neither layer holds keeps degrading to
  "no hover / no definition".

- **RFC-001 annotation-aware diagnostics + opaque-fragment
  navigation**. The annotation-validation diagnostics the
  compile pass emits — unknown annotation, wrong argument type,
  missing required argument (bare, empty-parens, and partial-list
  forms), and arg-count mismatches with both counts — are pinned
  with exact source ranges, including uses that resolve through
  imports (positions land in the importing file). Function calls
  inside opaque engine-expression fragments (an `expression`-typed
  annotation argument, where the capture has no AST value nodes) now
  hover and go-to-definition: the analyze pass retains the IR's
  resolved call sites, and a cursor on the callee name resolves the
  function in-file, through imports, or via the remote descriptor
  fallback. The analyze pass also retains the file's canonical
  protowire.schema.v1 source map; the new `RFC001SourceRange`
  primitive resolves a violation's `(file, descriptor_path)` key to
  the anchor token's editor range — the integration point for a
  future runtime-violation overlay (transport deferred).

- **Outer-qualified nested type references**. Field types
  naming a nested message or enum through its enclosing message —
  `pkg.Outer.Inner`, in-package `Outer.Inner`, and fully-qualified
  `.pkg.Outer.Inner` — now hover and jump across all three
  resolution layers (the local protoindex resolver, the registry
  served-source fallback, and the descriptor-only remote fallback),
  via C++-style scope peeling shared by all of them. Previously the
  qualifier had to equal a file's package exactly, so these forms
  resolved to nothing.

- **Reference-site scope resolution**. Names are
  interpreted against the cursor's enclosing scopes — package plus
  enclosing messages, innermost first — reproducing protoc's
  resolution order. `Middle.Inner` written inside `Outer` (where
  `Middle` nests in `Outer`) resolves; a bare name shadowed across
  scopes binds the nearest-enclosing declaration instead of the
  pre-order-first one; and scope-relative qualifiers work
  (`demo.v1.Bar` — or `@demo.v1.checked` — written in package
  `acme.demo.v1` / `acme.app.v1`). Annotation and function
  references follow the same rules, for navigation and `@`
  completion alike.

- **Completion parity for nested types**. Type
  completion emits exactly the reference shapes the resolver binds,
  through the resolver's own machinery: nested types complete under
  the shortest name valid at the cursor (`Inner` inside its
  enclosing message, `Outer.Inner` elsewhere, with shadowed names
  keeping their full path), imported nested types carry the full
  package + enclosing-message path, and dot completion resolves
  qualifiers through the cursor's scopes (`Middle.` inside `Outer`
  offers Middle's children). Package-prefix qualifiers offer
  segment continuations, so `acme.` walks `demo` → `v1` → the
  package's types across the current file and same-package imports.

### Changed

- **Name matching follows protoc scoping strictly**.
  Two long-standing leniencies are gone: bare names (types,
  annotations, functions) no longer resolve against imports in
  unrelated packages, and candidates no longer match declarations at
  arbitrary nesting depth (`pkg.Foo` binding `pkg.Bar.Foo`).
  References protoc rejects now yield no hover/definition, and
  colliding names across scopes can no longer bind the wrong
  declaration. proto2 groups participate as scopes — a group
  declares a message named after it — including groups declared
  inside extend blocks, whose bodies are transparent to scope walks
 .

### Fixed

- **Navigation ranges honor the negotiated position encoding.**
  `.proto` hover/definition ranges and compile-diagnostic squiggles
  were always emitted in UTF-16 code units; under a client-negotiated
  `utf-8` encoding, columns after any non-ASCII character landed off
  by the UTF-8/UTF-16 width difference. Span conversion now counts
  columns in the negotiated unit.

- **Request positions honor the negotiated position encoding**
  (the input-side twin of the range fix above). Incoming
  `.proto` positions — hover, definition, completion scope, and
  opaque-fragment call-site lookups — were always interpreted as
  UTF-16 code units; under a client-negotiated `utf-8` encoding, a
  cursor after any non-ASCII character on the line converted to the
  wrong byte offset and resolved the wrong node (or nothing).
  `protoast.File.OffsetIn` now converts in the negotiated unit, and
  every position-taking request threads the workspace encoding
  through.

- **Expression-diagnostic ranges honor the negotiated position
  encoding** (closing out the position-encoding sweep). The
  protocheck and protovalidate schema analyzers and the shared
  `nodeRange` helper still emitted fixed UTF-16 columns; under a
  client-negotiated `utf-8` encoding, a diagnostic on a line with a
  non-ASCII prefix squiggled the wrong columns. All three paths now
  convert through the workspace encoding, and the last fixed-UTF-16
  forms (`protoast.File.Range`, `protoast.SpanRange`) are gone —
  every range conversion must state its encoding.

## [0.15.0] – 2026-07-16

### Changed

- **protocompile bumped v0.16.0 → v0.17.0.** Two RFC-001 wins:
  annotation use sites that leave a no-default parameter unbound now
  diagnose ("missing required argument … ", protocompile#94) — pinned
  in the malformed-construct test table — and the protowire.schema.v1
  carrier gencode (SourceMap, Annotation, extension vars) is now
  published at the public import path
  `github.com/trendvidia/protocompile/gen/protowire/schema/v1`
  (protocompile#95), unblocking protolsp's future source-map
  consumption.

### Added

- **Semantic tokens for `.proto` documents**. The
  semantic-token layer now covers the RFC-001 schema-extension
  constructs stock editor grammars don't know: `type` / `function` /
  `annotation` declaration keywords highlight as `keyword`, and
  `@name` annotation-use sites (leading, trailing, and unbound) as
  `decorator` — a new legend entry appended at index 8. Classic
  proto2/proto3 syntax intentionally stays with the editor's own
  grammar layer, and `.pxf` token output is unchanged. Fed from the
  retained per-document AST, so requests never re-parse. Tests pin
  the positioned diagnostics the compile pipeline emits for malformed
  RFC-001 constructs.

## [0.14.0] – 2026-07-16

### Changed

- **protocompile bumped v0.15.0 → v0.16.0** (fixes protocompile#89:
  the root Compiler routes positioned diagnostics through its legacy
  Reporter again, with continue-on-nil and partial results).
  `schema/protofiles` drops the two-phase compile workaround and goes
  back to one reporter-collected batch compile. The editor-squiggle
  path (`analyzeProto`) intentionally stays on the report-native
  `queries.IR` pipeline — it yields full start..end spans where the
  reporter delivers point positions.

- **protocompile swapped from upstream `bufbuild/protocompile` v0.14.1
  to the `trendvidia/protocompile` fork v0.15.0.** The fork carries the
  RFC-001 protowire schema extensions (`type` / `function` /
  `annotation` declarations and `@annotation(...)` use sites), so
  editors no longer see spurious parse / compile errors on RFC-001
  grammar. The fork deleted the legacy parse-to-AST API in favor of
  the arena-based AST that upstream had been incubating under
  `experimental/`, so every AST consumer was migrated: the new
  `protoast` package bundles the (AST, indexed source, path) triple
  the navigation features share, `protoindex.Index.Lookup` returns
  `*protoast.File`, and the position → node walk now runs on byte
  offsets with UTF-16 LSP column conversion. Compiler-driven
  diagnostics (root `Compiler` + `reporter`) were untouched — the
  fork keeps that surface intact.
- **protowire-go bumped v1.0.0 → v1.2.2** (additive pxf changes:
  tolerant parse mode, span tracking / rewriter, editing helpers).

## [0.13.0] – 2026-07-10

### Fixed

- **registry:// resolution now honors per-sub-workspace registries
 .** In a multi-root workspace, a `registry://` preview opened
  from a sub-workspace resolved its content and cross-file jumps against
  the ROOT lookup — `LookupForNamespace` never consulted the
  sub-workspace's own registry (the W4 machinery). A sub-workspace whose
  registry differs from the root's could fetch the wrong file or miss a
  cross-file target the sub's lookup would have satisfied. Resolution
  now routes by the preview's sub-workspace, mirroring `LookupForDoc`:
  a sub that declares its own schema source resolves against it (no root
  fallback, per the W4 isolation guarantee); everything else stays on
  the root. The cross-file AST cache is keyed by workspace dir so two
  sub-workspaces sharing a namespace don't collide, and a sub-workspace
  lookup swap now clears that cache (the root manager already did).

  New engine surface for embedders: `OpenShadowProtoForWorkspace`
  records the preview's sub-workspace on the shadow document, and
  `SourceContentForWorkspace` / a `workspaceDir` field on the
  `protolsp/sourceContent` request + `GetSourceContent` RPC scope the
  bytes fetch. The pre-existing root-scoped `OpenShadowProto` /
  `SourceContent` keep their behavior (`workspaceDir == ""`), so
  single-root callers need no change.

## [0.12.0] – 2026-07-10

### Added

- **Cross-file hover cards inside registry:// .proto previews
  (goed#122).** The hover companion to 0.11.0's cross-file
  go-to-definition. Hovering a field's type that's declared in another
  registry file now renders that type's declaration card (message/enum
  + defining file + doc comment), resolved through the preview's
  namespace lookup — the same path go-to-definition uses. In-file and
  on-disk cross-file hover are unchanged.

## [0.11.0] – 2026-07-10

### Added

- **Cross-file go-to-definition inside registry:// .proto previews
  (goed#119).** Follow-up to the in-file navigation added in 0.10.0. A
  cursor on a field's type (or an `import` directive) that resolves to
  a declaration in ANOTHER registry file now jumps there, returning a
  `registry://` Location so the jump lands in another read-only preview
  rather than an editable disk file. The preview's imports resolve
  through the same schema lookup the preview came from
  (`LookupForNamespace` → `GetSource`), and ranges come from parsing the
  served source — reliable across all origins, including live
  registries whose published descriptors omit `source_code_info`.
  Parsed imported ASTs are memoized per lookup epoch (cleared on any
  lookup swap) so hover-underline checks and clicks don't re-fetch.
  On-disk `.proto` files keep their existing on-index cross-file
  resolution unchanged.

## [0.10.0] – 2026-07-10

### Added

- **Read-only registry:// .proto previews are now navigable — hover +
  go-to-definition (goed#26).** `Workspace.OpenShadowProto(uri, source)`
  tracks a lookup-backed `.proto` source (a `registry://` schema
  preview) as a read-only "shadow document": it parses the source into
  an AST but runs none of the analyze pipeline and publishes no
  diagnostics (the buffer is immutable and its URI is not a
  client-editable file). `HoverFor`/`DefinitionFor` route
  `registry://…​.proto` URIs to the proto AST handlers alongside on-disk
  `.proto` files, so hovering a field's type or Cmd/Ctrl-clicking it
  resolves within the preview. `IsRegistryProtoURI` exposes the routing
  predicate; `IsProtoURI` stays file-only so the diagnostics pipeline
  is unaffected. In-file go-to-definition now stamps the request URI
  (the registry buffer) instead of re-deriving it from the parsed
  filename, which a relative registry path can't produce; on-disk
  behavior is unchanged (the two URIs are identical there).
  Cross-file/cross-registry navigation is out of scope — the proto
  index that backs cross-file resolution holds on-disk files only.

## [0.9.0] – 2026-07-04

### Fixed

- **Layers that omit `source =` now bind to the declared `default`
  source during expansion.** The runtime implicitly resolves
  sourceless layer entries against a source named `default` when one
  is declared — pxfed's chameleon template ships exactly that shape —
  but `Expand`/`ResolveOrder` left their `ResolvedPath` without the
  fs root (`env/dev.pxf` instead of `layers/env/dev.pxf`). Layer
  classification then fell through to the permissive fs-root
  fallback: `IsLayer=true` but no bound environment, strategy, or
  ancestry, silently disabling merged-value hover, chain-aware
  required-field weighting, and `${field.path}` cross-layer
  definition. `LayerInstance.Source` now carries the resolved name,
  so an env-kind `default` source also lights up the env-source
  affordances for sourceless layers. Chains with no declared
  `default` source keep the bare-path behavior.
- **Watched-files events for chain layer files now re-analyze open
  documents.** `classifyWatchedEvents` dropped anything
  outside the config + chain-spec + .proto + buf paths, so an
  on-disk edit to a layer file (`layers/base.pxf`, external editor,
  git checkout, generator) never re-ran the analyze pipeline — but
  chain-aware required-field diagnostics on open leaves are computed
  from the union of fields set across resolve-order
  siblings/ancestors read from disk, so they went stale in both
  directions (a fixed `missing required` never cleared; a removed
  ancestor field left the leaf looking clean). A `.pxf` event that
  matches an expanded `LayerInstance.ResolvedPath` exactly, or sits
  under a declared fs source's root (the same containment
  `ComputeLayerInfo` falls back to), for the root or any
  sub-workspace chain, now fans out to `reanalyzeOpenDocs`. No state
  reload — layer contents are read fresh at analyze time. Workspaces
  with no loaded chain skip the check entirely.

### Changed

- **`Publisher.LookupChanged` now carries `workspaceDir` and fires
  for sub-workspace lookups.** The 0.8.0 hook covered only
  the root `LookupManager`; sub-workspace lookups install through
  `WorkspaceState` and fired nothing, so per-workspace status
  surfaces were back to polling for exactly the transitions the lookup-change
  notification had eliminated. The signature is now
  `LookupChanged(ctx, workspaceDir string)` — `""` for the root
  workspace's lookup, the sub-workspace directory otherwise — and
  every sub-workspace transition fires: async dial install, failed
  dial (state unchanged, refresh signal), config-cleared teardown,
  and eviction/shutdown. The nil→nil-silent rule applies to both.
  **Breaking for Publisher implementors adopting 0.8.0**: implement
  the two-argument form directly and skip the zero-arg one.

## [0.8.0] – 2026-07-03

### Added

- **Schema-browsing recipes exported as `analysis.Workspace` methods
 **: `RegistryStatus()`, `ListMessages(ctx, prefix)`, and
  `SourceContent(ctx, uri)` — the composed operations behind
  `protolsp/registryStatus`, `protolsp/listMessages`, and
  `protolsp/sourceContent` (dedupe + prefix filter + origin
  attribution + sort; `_internal_` namespace routing). The LSP
  dispatchers and the RPC sidecar are now thin shims over them, so
  in-process embedders share one implementation instead of
  re-deriving the recipes from lookup primitives. `SourceContent`
  errors expose `errors.Is` targets (`ErrNotRegistryURI`,
  `ErrNoSchemaLookup`) so callers can map error classes without
  string matching.

- **`Publisher.LookupChanged(ctx)` — lookup-transition hook**
  so embedders can stop polling registry status. Fires after a
  schema-lookup install or teardown completes (async rebuild
  install, proto_files rebuild, config-cleared teardown, shutdown
  `Close`) and after a rebuild dial fails (state unchanged — treat
  it as a refresh signal, not a delta; re-read `RegistryStatus`).
  Nil→nil swaps are silent. Fires on the mutating goroutine,
  including rebuild-dial and debounce goroutines; must not block.
  **Breaking for Publisher implementors** (pre-1.0): add the method
  — a no-op preserves today's behavior, `NopPublisher` embeds
  cleanly.

## [0.7.0] – 2026-07-03

### Changed

- **`analysis.DefaultFor` and `analysis.WalkMessages` unexported**,
  finishing the API curation. Both were kept exported only
  because server-scaffolded tests still referenced them; those tests
  now assert through the effective-config RPC surface / a local AST
  walk. Neither symbol was part of the documented embedder core.

- **`analysis.DiagnosticsFor` now takes the document read lock
  internally**, matching the package doc's promise that
  feature queries lock internally. It previously read `doc.mu`-
  guarded state bare, silently requiring the caller to hold
  `doc.RLock`. Embedders that call it inside their own
  `doc.RLock()` window must drop that lock — recursive RLock
  deadlocks against a queued writer. The threading-model package
  doc now also spells out the two deliberate exceptions:
  `CodeActionsFor` still needs the caller's RLock (its edits must
  stay consistent with the diagnostics passed in), and the `Set*`
  knobs are setup-time only.

### Fixed

- **buf lint no longer flags every packaged file with
  `PACKAGE_DIRECTORY_MATCH`.** `buftool.Lint` staged the buffer
  as a bare `input.proto`, so directory- and filename-based rules
  judged a synthetic layout: any file with a package declaration got a
  false warning the moment buf was installed. The buffer now stages at
  its include-path-relative location under a temp module root (derived
  from the proto index) and the root is linted. `buftool.Lint` gained a
  `relPath` parameter; empty or non-local values keep the old staging.

- **Workspace imports no longer squiggle as compile errors.**
  `analyzeProto`'s protocompile resolver only knew the document
  itself plus the standard `google/protobuf/*` well-knowns, so every
  workspace-local `import "x/y.proto"` — and every type it supplied —
  surfaced as an error even when the proto index resolved it fine for
  hover/definition/completion. The compile pass now resolves imports
  through the same buf.yaml-driven include paths
  (`SnapshotProtoIndex`), recursively; imports the index can't supply
  (or any non-self import when no index is built) still error
  verbatim.

## [0.6.0] – 2026-07-03

### `analysis` API curation

Breaking pre-1.0 cleanup of the surface the export campaign
created. Embedders (goed) are unaffected — the embedder core
documented in the package comment is unchanged.

- **Workspace/rebuild bookkeeping unexported behind accessors.**
  `WorkspaceManager.SubWorkspaces`/`WorkspacesMu` and the
  `RebuildMu`/`RebuildCancel`/`LastRebuiltCfg` trios on
  `WorkspaceState` and `LookupManager` are no longer exported —
  correct use required holding the right mutex around a bare
  map/field poke, and several callers read them unlocked.
  Replacements: `Workspace.SubWorkspaceByDir` (existing), new
  `Workspace.SubWorkspaceCount`, `WorkspaceState.LastRebuiltConfig`,
  and `WorkspaceState.RebuildInFlight`, all internally locked.
- **~30 pure helpers unexported; their unit tests moved in-package.**
  The helpers were only exported because their tests lived in
  `internal/server` (`applyOneChange`, `byteIndexAt`,
  `resolveFieldPath`, `refTokenAtCursor`, `extractValueAtPath`,
  `formatValueRepr`, `splitProtoQualifier`, the protocheck /
  protovalidate data-diagnostics family, and friends). The covering
  tests now live in `analysis/*_test.go`.
- **Unused exports dropped**: `NewDocuments`,
  `Workspace.TearDownSubWorkspace`, `Documents.URIs`,
  `LookupManager.InvalidateBaseline`/`SwapProtoFiles`, and the
  engine-internal `Document` fields (`DeprecatedUses`,
  `ProtocheckFindings`, `ProtovalidateFindings`, `ProtoAST`).
- **Package doc now states the API tiers and the threading model**
  (embedder core vs shell surface; which entry points to serialize;
  the Publisher contract), and `DiagnosticsFor`/`CodeActionsFor`
  spell out their redaction/locking obligations.
- Deliberately still exported: the roadmap-bound embedder surface
  (`DetectDatasetRegions`, `Build`/`ParseRegistryURI`,
  `FindWorkspaceDirFor`, `PxfPosToLSP`, the diagnostic `Data`
  payload codecs, `EditorConfig`, `LayerInfoResult`), plus
  `PositionShape`/`ProtocolRangeShape` (field types of the exported
  `IncrementalChange`) and `WalkMessages`/`DefaultFor` (still
  referenced by server-scaffolded tests; `DefaultFor` now has an
  in-package test so its demotion is a follow-up one-liner).

## [0.5.0] – 2026-07-03

First tagged release of the public `analysis` package:
embedders can pin `github.com/trendvidia/protolsp@v0.5.0` instead
of tracking a pseudo-version off main. The facade was shaken out
against goed's in-process embedding (trendvidia/goed#8), which
surfaced the two data races and the definition-URI fix below.

### Public analysis package (rung 3)

- **`analysis` package** — the engine graduated out of
  `internal/server` into the importable
  `github.com/trendvidia/protolsp/analysis`. The former in-package
  `engine` type is now `analysis.Workspace`: construct with
  `analysis.New(logger)`, install push callbacks via
  `SetPublisher`, feed content through `Documents()`, and drive
  hover / completion / definition / code actions / secret ranges /
  dataset regions / chain status / effective config directly — no
  jsonrpc2, no Connect-RPC (the only `go.lsp.dev` deps are the
  `protocol`/`uri` value types). In-process embedders (goed) get
  the exact pipeline the LSP runs: same debounce, same workspace
  discovery, same secret handling.
- **Embedder/test knob surface** on `Workspace`:
  `SetDebounceWindow`, `SetWatchedFilesWindow`, `SetBufLint`,
  `SetMaxReferencesFiles`, `SetHugeRegionBytes/Rows`,
  `SetProtoIndexCacheRoot`, `SetPositionEncoding`, `SetBwSession`,
  `AnalyzeCount`, plus `Lookups()`/`Config()`/`Workspaces()`
  accessors and a split teardown (`CancelBackground` + `Close`).
- **`ConfigPathForWorkspace`** now returns plain errors wrapping
  `analysis.ErrNoRootWorkspace` / `analysis.ErrUnknownWorkspaceDir`;
  the JSON-RPC and Connect-RPC shells map them onto their own error
  codes (wire behavior unchanged).
- `internal/server` keeps the LSP dispatch, configuration pull, and
  RPC sidecar as a thin shell over the embedded `*analysis.Workspace`;
  existing LSP + Connect-RPC clients see no behavior change.
- **Exported manager types**: `Lookups()`, `Config()`, and
  `Workspaces()` now return the exported `*LookupManager`,
  `*ConfigState`, and `*WorkspaceManager` (and sub-workspace state is
  `WorkspaceState`), so embedders can store them in fields and pass
  them around, not just chain method calls.
- **Per-workspace env resolution**: `analysis.New` no longer
  installs the Workspace's `${env.X}` resolver into the process-global
  `workspaceconfig.SetEnvLookup` — the resolver now rides along each
  parse via the new `workspaceconfig.ParseWithEnv(content, getenv)`,
  so two Workspaces in one process keep independent BW_SESSION
  overrides. `workspaceconfig.Parse` is unchanged (still uses the
  global default) for single-instance callers.

### Embedder shakeout fixes (goed#8)

- **Data race: config/chain classification vs watched-files
  reanalyze**. `RunAnalyzeAndMaybeLiveReload`'s tail read
  `doc.Parsed` with no doc lock while the watched-files debounce
  goroutine reset it under `doc.mu`. `IsWorkspaceConfigDoc` /
  `IsChainSpecDoc` now snapshot `Parsed` under RLock, covering
  every caller — LSP shell and embedders alike.
- **Data race: workspace-dir discovery**. `doc.WorkspaceDir`
  was written by lazy workspace discovery on the reanalyze path
  while read unlocked across the engine. The field is now
  self-synchronized (atomic). **Breaking:** the exported
  `Document.WorkspaceDir` field became `WorkspaceDir()` /
  `SetWorkspaceDir()` accessors.
- **Go-to-definition lands on the local working copy for
  proto_files-backed schemas**. Descriptors compiled from a
  local `proto_files` source resolved to read-only
  `registry://_proto_files_/...` previews because descriptor paths
  are search-path-relative and the workspace-root probe missed.
  The compile's descriptor-path → disk-path attribution (new
  `schema.ProtoFilesLocalPath`) now resolves them exactly, routed
  per-doc so sub-workspaces resolve against their own sources.
  Resulting contract: `file://` ⇒ local working copy, editable;
  `registry://` ⇒ genuinely remote, read-only via `sourceContent`.

## [0.4.0] – 2026-06-03

Multi-feature release. Two new editor capabilities ship
feature-complete: **`.proto` authoring** (cross-file resolution,
completion, format, lint, live invalidation — phases 3–5 of
`docs/design/proto-authoring.md`) and the **protocheck
integration** (schema-side expression validation + runtime .pxf
constraint checking). The Connect-RPC custom-method sidecar
covering Registry / Schema / Chameleon / Secrets / Datasets /
EffectiveConfig / Config also lands, with workspace-config flatten
+ Get/Set/List RPCs on top.

### `.proto` authoring (phases 3–5)

- **Workspace `.proto` file index**. `internal/protoindex`
  walks workspace `.proto` files and exposes them via a
  package-aware index for cross-file resolution. Backs every
  feature below.
- **Cross-file resolver + import jump**. `go-to-definition`
  walks `import "foo/bar.proto"` references to the imported file
  and lands on the named declaration.
- **Package-qualified resolution**. Names like
  `acme.demo.v1.Trade` resolve to the right `.proto` even when the
  cursor file doesn't import it directly; BSR descriptor cache
  test pinned alongside.
- **Live invalidation on disk changes**. Watched-file
  notifications invalidate the protoindex + downstream resolvers
  so hover / definition stay correct after `git pull` rewrites
  the workspace.
- **textDocument/completion for .proto** (phase 4a).
  Field names, message names, and dot-completions for qualified
  references inside `.proto` files.
- **Format-on-request via buf**. `textDocument/formatting`
  shells out to `buf format` and returns the diff. Falls through
  cleanly when `buf` isn't installed.
- **buf lint diagnostics**. `analyzeProto` appends
  `buf lint` findings as `Warning`-severity diagnostics with
  source `buf/lint`. Gated on `bufLintEnabled`; tests run with it
  off.

### protocheck integration

- **Phase 0 — schema-side expression validation**. Walks
  the .proto AST for `(protocheck.field|message)` aggregate
  options carrying `check: [{ expression: "…" }]` and type-checks
  each expression. Diagnostics surface as
  `protocheck/expression`.
- **Phase 0.5 — nested-message field access**. Message-
  level expressions now resolve dotted access into message-typed
  fields (`origin.city != destination.city`). Per-call-path cycle
  detection + 4-hop depth cap; no semantic linker required.
- **Phase 1 — runtime .pxf data validation**. After
  `pxf.UnmarshalDescriptor`, the per-Server `*protocheck.Validator`
  runs against the message and each violation's `FieldPath` maps
  to a `pxf.Position` via the parsed AST.
  `protocheckDataDiagnostics` renders the findings under source
  `protocheck`. Handles dotted descent (`address.zip_code`) and
  list-indexed (`items[3].sku`).
- **Phase 1.5 — end-to-end fixture harness**. Compiles a
  fixture `.proto` carrying protocheck annotations through the
  full lookup → unmarshal → Validate pipeline.
- **protocheck v1.1.0 bump**. Picks up upstream
  dynamicpb-interop fix in protocheck and the
  `*eval.CompileError` position API in its expression engine. Drops the
  `defer recover()` bandaid + the e2e harness's FDP
  marshal/unmarshal workaround that v1.0.x needed.
- **Per-column diagnostic ranges**. Phase 0 diagnostics
  anchor to the offending token inside the literal via
  `*eval.CompileError.Offset` + `Length`. Whole-literal fallback
  for the three cases that don't map cleanly (no usable position
  info, backslash escapes in the raw literal, invalid NodeInfo).

### Chameleon — per-env recipient editing

- **`Environment.key_name` exposed through chain status**.
  Chain panel can render which age recipient encrypts each env.
- **`SetEnvironmentKeyName` RPC**. Persistent on-disk edit
  of `chain.pxf` to set / clear an environment's `key_name` field.
- **`SetActiveEnv` RPC + `active_environment` field**.
  Session-scoped active environment for hover / effective-config
  flows; resets to `default_environment` on empty input.
- **chameleon v0.22.1 sync**. Adds the `key_name` field to
  the chain spec schema; enables the above.

### Connect-RPC custom-method sidecar

- **Schemas for the custom-method surface** (early in the range).
  Registry / Schema / Chameleon / Secrets / Datasets /
  EffectiveConfig / Config services replace the bespoke JSON
  notifications.
- **Sidecar boot from `initialize`** with the ConfigService
  wired up.
- **Full service wiring** for Registry / Schema / Chameleon /
  Secrets / Datasets / EffectiveConfig.

### Workspace config

- **Flattened workspace_config schema** + **Get/Set/List config
  RPCs** for the custom-method surface. Editors edit settings
  via typed RPC instead of bespoke JSON.

### Test coverage

Eight test-coverage closure PRs covering: unit gaps
(workspace close, schema cache, exit, interceptor), functional
gaps (redaction, dataset, config, watch), integration gaps
(stdio cap, tripwire, bind fail, chain reload), buf.lock
mid-session deps add, alternative-path follow-ups.

### Fixes

- **Hover: resolve fields inside array-literal elements**.
  Hover on a field inside `items: [{...}]` now resolves
  correctly.
- **`buftool` kill-grace window**. Subprocess teardown no
  longer hangs past the context deadline under `-race`.
- **`Close` vs pending analyze timer**. `Server.Close`
  now iterates open documents and cancels per-doc
  `analyzeTimer`s; was a real bug caught by new test coverage.
- **`initialized` reader-deadlock fix**. `dispatchInitialized`
  follow-up runs on a goroutine so nested `conn.Call`s aren't
  blocked by the reader.
- **`reloadSubWorkspace` chain-hover redact**. Use
  `redact.Placeholder` constant.

### Documentation

- **Package + type doc surface refresh**. Every exported package and significant type carries a
  doc-comment with usage shape.

### Dependencies

- `github.com/trendvidia/protocheck` v1.0.0 → **v1.1.0**.

Full diff: https://github.com/trendvidia/protolsp/compare/v0.3.0...v0.4.0

## [0.3.0] – 2026-05-19

Workspace-config completion release. The W-cluster from
`docs/design/workspace-config.md` reaches feature-complete: the
remaining phases (W2 `proto_files` source, W3 multi-source
composition) shipped, and the previously-deferred W4b
per-workspace lookup and W6 chameleon-block wiring are
documented as already-shipped (the comments + phase table were
stale relative to the code).

### Schema sources

- **`proto_files` schema source** (W2). Workspaces can now
  declare `schema_sources { proto_files { paths = [...] } }` in
  `.protolsp.pxf` and get schema-aware editing without standing
  up a protoregistry. New `internal/schema/protofiles.go` walks
  the declared paths, compiles via `bufbuild/protocompile`, and
  exposes the result as a `schema.Lookup` backed by
  `protoregistry.NamespacedFiles` + `NamespacedTypes`. The
  source kind composes with the existing `registry { ... }`
  block via W3 below. Compile failures surface as
  `Error`-severity diagnostics on `.protolsp.pxf` anchored at
  the `proto_files` block so the user lands close to the source
  declaration when jumping to the diagnostic.
- **Multi-source composition** (W3). `schema.Open` now
  takes variadic `OpenOption`; the new `schema.WithFallback`
  threads through to `protoregistry/client.WithFallback` at
  dial time. Both root and sub-workspace dial paths plumb the
  workspace's `proto_files` `NamespacedFiles` / `Types` in as a
  fallback parent on the registry resolver — unresolved names
  walk transparently from the registry into the local
  descriptor set, no separate-lookup routing dance. Build order
  is now proto_files-first so the dial goroutine always reads a
  populated snapshot.

### Fixes (in passing)

- **`reloadSubWorkspace` no longer leaks the prior sub-workspace
  registry conn**. The old `workspaceState` was discarded
  without closing its `LookupCloser`; a `.protolsp.pxf` edit
  that rewired the address leaked a gRPC connection each time.
  Tear-down now precedes the swap-in. Same change closes the
  new `ProtoFilesCloser` slot.

### Documentation

- **W-phase status table reflects reality**
  (`docs/design/workspace-config.md`). W1–W6 now annotated as
  shipped with pointers to the implementation. The W6 row
  notes that the planned `initializationOptions.chameleon.*`
  deprecation nudge was dropped — the entry point was decided
  against per the `chameleon-integration.md` decision log
  (2026-05-16) and never shipped, so there's nothing to
  deprecate.
- **Roadmap drops the W2 parked entry**
  (`docs/roadmap.md`). The "Out of scope (parked)" list no
  longer references protocompile / `proto_files`; the
  H-cluster (hosted-platform blockers) remains the only
  parked work that lives in this repo.

### Dependencies

- `github.com/bufbuild/protocompile` promoted from indirect to
  direct.

## [0.2.0] – 2026-05-19

Major feature release covering the v0.1.0 → v0.2.0 window. Six
post-MVP feature lines landed first, each with its own per-phase
design doc under `docs/design/`. The 2026-05-18 audit drove a
concurrency / resource-bounding hardening sweep against
everything shipped above, tracked against `docs/roadmap.md`.
The 2026-05-19 LSP-surface fill-in then added the editor
features that hosted IDE integration needs (outline, semantic
tokens, formatting, references, rename) and put TLS on the
registry dial.

### LSP feature surface

- **Incremental document sync** (I1). Negotiates
  `TextDocumentSyncKindIncremental` in `initialize`; `didChange`
  applies LSP content-change events in order against the
  previously-stored source. Position-encoding negotiation in I2
  (`utf-8` / `utf-16`); analyze debouncing in I3 coalesces rapid
  edits into one pipeline run.
- **Chameleon chain support** (C1–C7). Discovers + parses the
  workspace's `chain.pxf`; classifies open documents as chain
  layers (ONCE / PER_ENV) and emits chain-aware required-field
  diagnostics (no false positives on partial leaf files).
  `${field.path}` references get unresolvable / cycle
  diagnostics, completion, and go-to-definition (deepest
  layer setting the path wins, schema fallback otherwise).
  Merged-value hover renders each layer's contribution in the
  env's resolve order. `pxf.default` redundancy hints +
  env-source hover hints.
- **Dataset region support** (R1–R4). Line-based + AST-integrated
  scanner detects `@dataset` blocks with row counts, encrypted
  flag, and Huge classification. Region-aware hover renders type
  info on `@dataset` directive lines. Per-row validation drives
  per-cell diagnostics for huge regions without re-validating
  the entire dataset on every edit.
- **Secret redaction** (S1–S4). `pxf.Secret`-typed values are
  scrubbed from diagnostics, hover, completion details, and code
  actions before reaching the wire. S2 surfaces the per-document
  range list to editors so they can mask in the editor view.
  S3 emits Warning diagnostics on literal-secret assignments
  with a "rewrite as `${env.X}`" quick-fix. S4 enforces a
  configurable `security.inline_secrets` policy
  (`allow` / `warn` / `refuse`).
- **Outline + breadcrumbs**. `textDocument/documentSymbol`
  returns a hierarchical `DocumentSymbol` tree built from the
  cached parse: `@type` directive, free-form `@<name>` directives,
  `@dataset` blocks (with columns as children), `@proto`, and the
  typed body's `Assignment` / `Block` / `MapEntry` entries. Each
  symbol carries the protobuf kind in `Detail` when the descriptor
  resolves.
- **Schema-driven semantic highlighting**.
  `textDocument/semanticTokens/full` layers tokens on top of the
  editor's syntactic tokenizer: directive heads (macro), dotted
  type URLs (namespace), field names with `deprecated` +
  `defaultLibrary` modifiers, block names (struct), string /
  number / bool / null / enum values. Stable 8-type + 2-modifier
  legend; indices are wire-stable across releases (appending is
  safe, reordering would break clients).
- **Canonical formatting**. `textDocument/formatting`
  delegates layout to `pxf.FormatDocument` so LSP and CLI produce
  identical output. Empty edit slice when the buffer is already
  canonical (so format-on-save doesn't dirty unchanged files); nil
  when the doc hasn't parsed (don't clobber mid-edit state). Client
  `FormattingOptions` are advisory only — PXF formatting is
  gofmt-style, one canonical layout.
- **References + rename**. Workspace-scoped via
  `Document.WorkspaceDir` — clicking on a field in one chameleon
  layer surfaces every layer that touches the same dotted path
  (e.g. `db.host`). Rename validates the new identifier and emits
  `InvalidParams` on bad input. Open-document only; filesystem
  index deferred.

### Custom LSP methods

Seven new methods + three notifications since 0.1.0:

- **`protolsp/registryStatus`** (M5 follow-up): `{configured,
  stale}` so editors can render a "Schema: live / stale" status
  bar indicator. Stale-on-startup also fires a one-time
  `window/showMessage` warning at `initialized`.
- **`protolsp/getEffectiveConfig`** (CP2): four-tier-resolved
  view of every known config section with provenance source
  (`default` / `file` / `settings` / `override`). `revealSecrets`
  flag opts out of name-pattern redaction (`*.token`,
  `*.password`, `*.secret`).
- **`protolsp/setOverrides`** + **`protolsp/effectiveConfigChanged`**
  (CP3): install Tier 4 session-scoped overrides and push the
  changed subset back to subscribed clients.
- **`protolsp/secretRanges`** + **`protolsp/secretRangesChanged`**
  (S2): per-document ranges of every `pxf.Secret` value plus a
  notification fired when re-analyze changes the set.
- **`protolsp/datasetRegions`** +
  **`protolsp/datasetRegionsChanged`** (R1): per-document
  `@dataset` regions + change notification.
- **`protolsp/chainStatus`** (C1): workspace chain summary —
  environments, ancestors, errors.
- **`protolsp/layerInfo`** (C2): per-document chain layer
  classification (`IsLayer`, `Strategy`, `Environment`).

### Configuration

- **`.protolsp.pxf` workspace config file** (W1). `@type
  protolsp.v1.WorkspaceConfig` PXF file at the workspace root
  configures the registry connection, the chameleon chain
  spec, and the secret-handling policy. `${env.X}`
  interpolation reads from the LSP process's environment so
  tokens stay out of committed files.
- **Multi-root workspace discovery** (W4 + W4b). Each open
  document maps to its nearest-ancestor `.protolsp.pxf` via a
  cached `Document.WorkspaceDir`. Sub-workspaces that declare
  their own `registry` block get their own independently
  rebuilt schema lookup; sub-workspaces without one fall
  through to the root.
- **File watching + live reload** (W5). `.protolsp.pxf` and
  `chain.pxf` edits hot-reload via
  `workspace/didChangeWatchedFiles` plus a live-reload-from-
  edit path that doesn't require save. Sub-workspace changes
  route to the affected workspace via the watcher event
  classifier.
- **Four-tier config resolution** (CP1–CP5). Default → File →
  Settings (`workspace/configuration`) → Override
  (`setOverrides`); higher tiers win. `workspace/configuration`
  + `workspace/didChangeConfiguration` are wired with dynamic
  capability registration. `initializationOptions` becomes the
  Tier 3 seed when the client doesn't implement
  `workspace/configuration`; legacy clients receive a one-time
  deprecation `window/showMessage` at `initialized` when they
  use the seed path AND could migrate.
- **Async lookup rebuild with diff + cancellation**. Registry
  config changes (settings, file edit, override) trigger a
  background `schema.Open` dial; the running goroutine is
  cancelled if a fresher change arrives. Dial errors keep the
  prior lookup in place. 30s default timeout (R3).
- **TLS for the registry dial**. `schema.Config` gains a
  `TLS *TLSConfig` field (`ServerName`, `CACertPath`,
  `InsecureSkipVerify`). Nil keeps the historical insecure
  transport (fine for loopback / docker-compose); non-nil enables
  TLS via the new upstream `client.WithTransportCredentials`
  option, preserving `Dial`'s cache-fallback path. A
  misconfigured `CACertPath` surfaces as a build error from
  `Open` *before* the dial.

### Hardening (post-audit)

- **Concurrency safety** (C1–C6). `Server.stateMu` (RWMutex)
  guards `fileConfig` / `chain` / `editorConfig` /
  `seededRegistryFromInitOptions` via snapshot+swap helpers
  mirroring the existing `snapshotLookup` / `swapLookup` pair.
  Per-`Document` RWMutex serializes `analyze` writes against
  handler reads. `overridesMu` now spans the full
  snapshot→apply→snapshot window for atomic multi-section
  override commits. `Server.Close` cascades teardown to every
  loaded sub-workspace's lookup; `ensureSubWorkspace`'s
  lazy-load now holds `workspacesMu` so a concurrent
  `subWorkspaceForDoc` reader can't observe a half-loaded
  struct. Race tests pin each contract.
- **Bounded I/O** (R1, R3). The new `internal/safeio` package's
  `ReadFileLimit` enforces a 4 MiB ceiling on every config /
  chain / per-env layer read site. `schema.Open` wraps the
  registry dial in `context.WithTimeout(30s)` so a stalled
  endpoint can't leak the async-rebuild goroutine.
- **Bounded LSP frames** (R2). New `limitedFrameReader` between
  `os.Stdin` and the jsonrpc2 framing parser enforces
  `MaxStdioFrameBytes = 4 MiB`; rejects oversized
  `Content-Length` before any body bytes are read. Headers
  also capped at 64 KiB. Matches the websocket transport's
  `SetReadLimit` default.
- **Sub-workspace eviction** (R4). Reference-driven: a
  sub-workspace persists while any of its docs is open and
  evicts via `maybeEvictSubWorkspace` from `dispatchDidClose`
  the moment the last one closes. Re-opens trigger a fresh
  discover-and-load via `ensureSubWorkspace`. Shared
  `tearDownSubWorkspace` helper used by both eviction and
  `Server.Close`.
- **Lint debt drained**. `golangci-lint run ./...` at 0
  findings after dropping the dead `errConfigUnsupported`
  sentinel, deleting the never-read `curLineStart` variable
  + its orphaned `lineStartByte` helper, applying De Morgan
  to a block-comment scanner check, and silencing the
  `params.RootURI` SA1019 with a documented `//nolint`
  directive (LSP spec requires servers to keep accepting
  the deprecated field).

### Documentation

- **`docs/roadmap.md`** captures every gap surfaced by the
  2026-05-18 audit (concurrency, resource exhaustion,
  hosted-platform blockers, tech debt, test coverage, doc
  drift, hygiene), sized to single-PR units.
- **Per-feature design docs** added: `workspace-config.md`,
  `config-protocol.md`, `chameleon-integration.md`,
  `dataset-regions.md`, `secret-handling.md`,
  `incremental-sync.md`.
- **README** now leads with the four-tier config model
  + a concrete `.protolsp.pxf` example, enumerates all 9
  custom LSP methods + 3 notifications in a table with
  design-doc links per method, and links every per-feature
  design doc from the Design section.
- **`docs/design/platform-architecture.md` moved out of this
  repo.** The document is platform-scope (hosted runtime, JIT
  sandbox, tool synthesis, hosted IDE, control plane) and
  outgrew its temporary home in protolsp.

### Changed

- **Bump `github.com/trendvidia/protoregistry` to v0.72.0**.
  v0.71.0 (namespace hierarchy, `WithServerChain`,
  `RangeMessages`, `Find*WithOrigin`, `GetSource`, `WithDiskCache`)
  promoted from a pseudo-version pin, then v0.72.0 picked up the
  new `client.WithTransportCredentials` option that powers the TLS
  dial path above.

## [0.1.0] – 2026-05-16

Initial public release. Ships the M1–M6 MVP plus the post-MVP
feature work that landed in lockstep with the v1.0 spec line.

### LSP feature surface

- **Document sync + parse diagnostics** (M1, M2). `didOpen` /
  `didChange` / `didClose` over stdio with full-document sync;
  parse errors via `pxf.Parse` with 1-indexed → 0-indexed position
  conversion. `Documents` cache survives mid-edit invalid states
  via `LastGoodParse` so downstream features keep working while the
  user is typing.
- **Schema-aware diagnostics** (M3). `internal/schema.Open` wraps
  `protoregistry/client.New` with `WithServerChain` so an
  organization's namespace hierarchy resolves automatically.
  `analyze` chains parse → `pxf.UnmarshalDescriptor` (with
  `DiscardUnknown` so type-mismatch errors don't duplicate unknown-
  field findings); first error wins. Empty registry config degrades
  cleanly to parse-only diagnostics.
- **Hover** (M4). Line-match heuristic over the AST resolves field
  → `FieldDescriptor`; markdown renders field name, type label
  (with `repeated` / `map<>` cardinality), defining file, and the
  proto source's leading doc comment when available. Namespace
  provenance via `FindFileByPathWithOrigin` shows `Defined in
  <path> (namespace <id>)` when the resolver can attribute it.
- **WebSocket transport** (M5). `cmd/protolsp-ws` accepts upgrades
  via `coder/websocket`, exposing the same handler over one
  `server.Server` per connection. `--addr` / `--path` / `--origins`
  / `--insecure-skip-origin-verify` / `--max-message-bytes` flags
  plus a `/healthz` endpoint. Closes the conn directly on exit
  (avoiding a latent `atomic.Value` type-mismatch panic in jsonrpc2
  v0.10.0).
- **Completion**. Field-name completion against the bound message
  with AST-aware nested-block context (brace-counted enclosure);
  message-type completion on `@type` directives via the SDK's
  `RangeMessages`; enum-value completion on the right-hand side of
  enum-typed assignments. No trigger characters — IDE invokes on
  identifier input.
- **Code actions**. Hint diagnostics with `Tags: [Deprecated]` for
  every use of a `[deprecated = true]` field; Error diagnostics
  with a `Data` payload identifying each unknown field plus a
  "Remove field" quick-fix that deletes the offending line.
  `CodeActionKinds: [QuickFix]` advertised in `initialize`.
- **Go-to-definition**. `textDocument/definition` for field
  assignments, block names, and `@type` FQNs. URI precedence:
  local `file://` when the file exists on disk (working-copy
  semantics win), `registry://<namespace>/<file-path>` when the
  resolver attributes the file to a tracked namespace (editor
  fetches bytes via `protolsp/sourceContent`), blind `file://` as
  last resort.

### Custom LSP methods

- **`protolsp/sourceContent`**: fetch source bytes for a
  `registry://` URI. Editor content providers consume this to
  render registry-only `.proto` files as read-only virtual
  documents.
- **`protolsp/listMessages`**: enumerate every message type the
  bound resolver knows, deduped + sorted, with optional `prefix`
  filter. Pairs each entry with its contributing namespace. Lets
  editors implement schema-browser panels without opening their
  own protoregistry connection.

### Documentation

- `docs/design/protolsp-mvp.md` — phased delivery plan and design
  decisions (positions, parser integration, transport choices).
- `docs/design/platform-architecture.md` — broader platform context.

### Dependencies

- `github.com/trendvidia/protoregistry@v0.70.2-0.20260516040307-8518acd5bc65`
  (`Resolver.GetSource` + `FindFileByPathWithOrigin` +
  `RangeMessages`).
- `github.com/trendvidia/protowire-go@v1.0.0` (pxf v1 grammar).
- `go.lsp.dev/protocol`, `go.lsp.dev/jsonrpc2`, `coder/websocket`.
