// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package server implements protolsp's Language Server runtime — the
// piece that owns the document store, dispatches every JSON-RPC method
// the client sends, and runs the side-channel Connect-RPC sidecar pxfed
// uses for typed RPC access to the same state.
//
// Responsibilities, roughly in dispatch order:
//
//   - LSP lifecycle (initialize / initialized / shutdown / exit) +
//     workspace bring-up (root config load, schema-lookup setup,
//     chameleon chain discovery, proto-index build).
//   - Document sync (didOpen / didChange / didClose / didSave) backed
//     by an in-memory store with per-doc parsed AST and analysis
//     cache.
//   - Standard LSP features over .pxf and .proto:
//     diagnostics, hover, go-to-definition, references, document
//     symbol, formatting, code action, completion, semantic tokens,
//     rename. Proto-specific variants live in proto_*.go.
//   - Schema-aware features driven by the resolved Lookup
//     (registry-backed + proto_files-backed + internal). Field
//     resolution, hover/definition routing, completion, validation
//     diagnostics.
//   - Chameleon chain integration: per-doc layer info, merged-value
//     hover, env-source hints, active-env selector.
//   - pxf.Secret handling: secretRanges request, literal-secret
//     diagnostics + quick-fix, masking on the wire.
//   - Dataset regions: large-region detection + subscription so the
//     editor folds huge inline data without disabling LSP for the
//     whole document.
//   - Custom requests beyond the LSP spec (chainStatus, registryStatus,
//     getEffectiveConfig, etc.) — paralleled by Connect-RPC services
//     in rpc_*.go for typed-client consumers.
//   - Workspace-config file (.protolsp.pxf): four-tier effective-
//     config model (default → file → init-options → settings push)
//     resolved through internal/workspaceconfig.
//   - Proto-authoring features (phases 1–5): protocompile-driven
//     diagnostics, hover, go-to-definition, cross-file imports,
//     completion, buf format, buf lint.
//
// Dispatch entry point: Server.Handle (see Handle's method body for
// the per-method switch). Custom-method names live in const blocks at
// the top of the relevant feature file.
//
// Connect-RPC sidecar: started during initialize, advertised back to
// the client via InitializeResult.serverInfo.rpc. Service
// implementations live in rpc_*.go and share the Server's state via
// receiver methods.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/analysis"
	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/schema"
)

// Server is the LSP handler. One Server backs one client connection.
// It wraps the transport-free *analysis.Workspace (embedded — the
// former in-package engine) with everything
// jsonrpc2/Connect-RPC specific: the client conn, the dispatch
// switch, the RPC sidecar + its Subscribe registries, and the
// legacy-LSP-client notification gating.
type Server struct {
	*analysis.Workspace

	conn jsonrpc2.Conn // set via AttachConn before Handle runs; used for server→client notifications

	// legacyLspClients tracks which legacy LSP request methods
	// have been invoked this session. Used to gate the
	// corresponding *Changed notifications so RPC-only clients
	// (pxfed since pxfed#34, plus any client that skips the legacy
	// request methods) don't pay the doubled-traffic tax on
	// every edit. Each bool flips true on the first call to its
	// legacy request dispatcher and stays true until Server is
	// torn down. serverPublisher reads the bool before
	// s.conn.Notify; the RPC publish path is always fired
	// regardless.
	legacyLspClients struct {
		secretRanges    atomic.Bool
		datasetRegions  atomic.Bool
		effectiveConfig atomic.Bool
	}

	// initializedFollowup tracks the goroutine kicked off from the
	// `initialized` handler. Handle returns immediately from that
	// notification so the jsonrpc2 reader can deliver responses to
	// the nested conn.Calls the follow-up issues (registerCapability,
	// workspace/configuration). Tests use WaitInitialized to block
	// until the follow-up completes before asserting on client-side
	// effects. Production code never waits on it — the goroutine is
	// fire-and-forget; any error is logged.
	initializedFollowup sync.WaitGroup

	// configChangeFollowup tracks the goroutine kicked off from each
	// `workspace/didChangeConfiguration` notification. Same rationale
	// as initializedFollowup: dispatchDidChangeConfiguration MUST
	// return before its nested workspace/configuration
	// conn.Call so the single-goroutine jsonrpc2 reader is free to
	// route the response. Without that, the dispatcher deadlocks and
	// every subsequent LSP message (textDocument/didOpen, hover,
	// definition) silently queues unprocessed — the "secrets not
	// redacted + go-to-def broken after bw-session paste" bug.
	// Tests use WaitConfigChange after sending didChangeConfiguration
	// to make pull/apply assertions deterministic.
	configChangeFollowup sync.WaitGroup

	// rpc is the Connect-RPC sidecar exposing typed proto services
	// alongside the LSP custom methods. Nil until startRPC runs
	// (initialize allocates and starts it so the endpoint info can
	// ride along InitializeResult.serverInfo.rpc). Lifecycle is
	// bound to *Server — Close shuts it down alongside the lookup.
	rpc *rpcServer

	// secretsSubsOnce / secretsSubsInstance back the secretsSubs()
	// accessor in rpc_secrets_subscribe.go. The registry holds every
	// active SecretsService.Subscribe stream so notifySecretRanges
	// can fan its events out alongside the LSP notification.
	secretsSubsOnce     sync.Once
	secretsSubsInstance *secretsSubscribers

	// Sibling registries for the other two streaming services. Same
	// lazy-init pattern as secretsSubs; allocated only when the
	// sidecar actually serves a Subscribe call.
	datasetsSubsOnce     sync.Once
	datasetsSubsInstance *datasetsSubscribers

	effectiveConfigSubsOnce     sync.Once
	effectiveConfigSubsInstance *effectiveConfigSubscribers

	closed atomic.Bool
}

// AttachConn wires the client connection used to send server-initiated
// notifications (notably textDocument/publishDiagnostics). The
// transport calls this between New and starting the connection loop.
//
// Separated from the constructor so the Server is testable without a
// real jsonrpc2.Conn — tests pass a mock implementing the same
// interface.
func (s *Server) AttachConn(conn jsonrpc2.Conn) { s.conn = conn }

// Close releases the schema-lookup resolver if one was opened by
// initialize. Safe to call multiple times; safe to call when no
// lookup was configured. Called by the transport after the LSP
// connection terminates.
func (s *Server) Close() error {
	// Stop the workspace's pending debounce timers first so no
	// timer fires into a server that is going away.
	s.CancelBackground()
	// Shut down the Connect-RPC sidecar before tearing down
	// workspaces — its handlers reach into workspace state, so we
	// want every in-flight RPC drained before that state goes away.
	s.stopRPC()
	// Workspace.Close tears down sub-workspace lookups and the
	// schema-lookup manager (cancels any in-flight rebuild AND
	// closes the current lookup).
	return s.Workspace.Close()
}

// New creates a Server with an empty document store.
func New(log *slog.Logger) *Server {
	s := &Server{Workspace: analysis.New(log)}
	// Default outbound seam: LSP notifications (once a conn is
	// attached) + Connect-RPC stream fan-out. Embedders replace
	// this with their own Publisher via the analysis facade.
	s.SetPublisher(serverPublisher{s})
	return s
}

// dispatchSetBwSession handles the pxfed/setBwSession custom
// notification. Params shape: `{ "token": string }`. Empty
// token clears the override (returns to os.Getenv only).
//
// Notification (not a request), so no reply payload — the
// reload work runs asynchronously like the initialized
// follow-up does to avoid blocking the jsonrpc2 reader
// goroutine. Errors during reload log + drop; pxfed gets no
// completion signal but the next textDocument operation
// surfaces any persistent issue.
func (s *Server) dispatchSetBwSession(ctx context.Context, req rpcRequest) (any, error) {
	var params struct {
		Token string `json:"token"`
	}
	if err := protocol.Unmarshal(req.Params(), &params); err != nil {
		s.Logger().Warn("pxfed/setBwSession: bad params", slog.String("err", err.Error()))
		return nil, nil
	}
	go s.SetBwSession(context.Background(), params.Token)
	return nil, nil
}

// WaitInitialized blocks until the goroutine kicked off from
// `initialized` (registerDynamicCapabilities + pullEditorConfig +
// NotifyStaleSchemaIfApplicable + notifyInitOptionsDeprecation) has
// finished. Tests call this to make assertions deterministic; the
// production stdio loop never needs to wait — the goroutine is
// fire-and-forget by design.
func (s *Server) WaitInitialized() { s.initializedFollowup.Wait() }

// WaitConfigChange blocks until every dispatchDidChangeConfiguration
// follow-up goroutine has finished. Same fire-and-forget rationale as
// WaitInitialized; tests use it to await pull / apply completion
// before asserting on resulting state.
func (s *Server) WaitConfigChange() { s.configChangeFollowup.Wait() }

// Handle is the entry point used by jsonrpc2 to dispatch a single
// request or notification. The signature matches jsonrpc2.Handler so the
// stdio loop can install it directly.
func (s *Server) Handle(ctx context.Context, req *jsonrpc2.Request) (any, error) {
	return s.serve(ctx, req)
}

// rpcRequest is the part of *jsonrpc2.Request the handlers use.
type rpcRequest interface {
	Method() string
	Params() jsonrpc2.RawMessage
	IsCall() bool
}

func (s *Server) serve(ctx context.Context, req rpcRequest) (any, error) {
	// The request's own ctx is pooled and dies with this call, but
	// follow-up goroutines and debounced work keep ctx. Use the
	// connection's.
	result, err := s.handle(jsonrpc2.DetachContext(ctx), req)
	if err != nil && !req.IsCall() {
		// An error from a notification closes the jsonrpc2 connection.
		// One bad message must not disconnect the editor.
		s.Logger().Warn("notification failed", slog.String("method", req.Method()), slog.String("err", err.Error()))
		return nil, nil
	}
	return result, err
}

func (s *Server) handle(ctx context.Context, req rpcRequest) (any, error) {
	switch req.Method() {
	case protocol.MethodInitialize:
		return s.dispatchInitialize(ctx, req)
	case protocol.MethodInitialized:
		s.Logger().Info("client initialized")
		// `initialized` is a notification — clients do not wait for the
		// reply. The follow-up work below issues nested conn.Call()s
		// (client/registerCapability, workspace/configuration, etc.).
		// jsonrpc2 dispatches Handle on the reader goroutine, so making
		// those calls inline blocks the reader and the responses can
		// never arrive — every subsequent request deadlocks. Run the
		// follow-up on a fresh goroutine so the reader stays free.
		// The WaitGroup lets tests block on completion; production code
		// fires and forgets.
		s.initializedFollowup.Add(1)
		go func() {
			defer s.initializedFollowup.Done()
			s.registerDynamicCapabilities(ctx)
			// Pull editor settings (Tier 3) now that the handshake is
			// complete. Any values the client returns override the seed
			// derived from initializationOptions. Failure is non-fatal —
			// we keep running with the seed.
			if values, err := s.pullEditorConfig(ctx); err != nil {
				s.Logger().Warn("initial workspace/configuration pull failed; keeping initializationOptions seed",
					slog.String("err", err.Error()))
			} else if _, applyErr := s.applyEditorConfig(ctx, values); applyErr != nil {
				s.Logger().Warn("applying initial editor configuration",
					slog.String("err", applyErr.Error()))
			}
			s.NotifyStaleSchemaIfApplicable(ctx)
			s.notifyInitOptionsDeprecation(ctx)
		}()
		return nil, nil
	case protocol.MethodWorkspaceDidChangeConfiguration:
		return s.dispatchDidChangeConfiguration(ctx, req)
	case protocol.MethodWorkspaceDidChangeWatchedFiles:
		return s.dispatchDidChangeWatchedFiles(ctx, req)
	case protocol.MethodTextDocumentDidOpen:
		return s.dispatchDidOpen(ctx, req)
	case protocol.MethodTextDocumentDidChange:
		return s.dispatchDidChange(ctx, req)
	case protocol.MethodTextDocumentDidClose:
		return s.dispatchDidClose(ctx, req)
	case "pxfed/setBwSession":
		return s.dispatchSetBwSession(ctx, req)
	case protocol.MethodTextDocumentHover:
		return s.dispatchHover(ctx, req)
	case protocol.MethodTextDocumentCompletion:
		return s.dispatchCompletion(ctx, req)
	case protocol.MethodTextDocumentCodeAction:
		return s.dispatchCodeAction(ctx, req)
	case protocol.MethodTextDocumentDefinition:
		return s.dispatchDefinition(ctx, req)
	case protocol.MethodTextDocumentDocumentSymbol:
		return s.dispatchDocumentSymbol(ctx, req)
	case protocol.MethodTextDocumentFoldingRange:
		return s.dispatchFoldingRange(ctx, req)
	case MethodSemanticTokensFull:
		return s.dispatchSemanticTokensFull(ctx, req)
	case protocol.MethodTextDocumentFormatting:
		return s.dispatchFormatting(ctx, req)
	case protocol.MethodTextDocumentReferences:
		return s.dispatchReferences(ctx, req)
	case protocol.MethodTextDocumentRename:
		return s.dispatchRename(ctx, req)
	case MethodSourceContent:
		return s.dispatchSourceContent(ctx, req)
	case MethodListMessages:
		return s.dispatchListMessages(ctx, req)
	case MethodRegistryStatus:
		return s.dispatchRegistryStatus(ctx, req)
	case MethodEngineConfig:
		return s.dispatchEngineConfig(ctx, req)
	case MethodGetEffectiveConfig:
		return s.dispatchGetEffectiveConfig(ctx, req)
	case MethodSetOverrides:
		return s.dispatchSetOverrides(ctx, req)
	case MethodSetRuntimeViolations:
		return s.dispatchSetRuntimeViolations(ctx, req)
	case MethodSecretRanges:
		return s.dispatchSecretRanges(ctx, req)
	case MethodDatasetRegions:
		return s.dispatchDatasetRegions(ctx, req)
	case MethodChainStatus:
		return s.dispatchChainStatus(ctx, req)
	case MethodLayerInfo:
		return s.dispatchLayerInfo(ctx, req)
	case MethodConfigList:
		return s.dispatchConfigList(ctx, req)
	case MethodConfigGet:
		return s.dispatchConfigGet(ctx, req)
	case MethodConfigSet:
		return s.dispatchConfigSet(ctx, req)
	case protocol.MethodShutdown:
		s.Logger().Info("shutdown requested")
		s.closed.Store(true)
		return nil, nil
	case protocol.MethodExit:
		// Exit is a notification; jsonrpc2 expects no reply. Close the
		// underlying connection asynchronously so the dispatch loop's
		// next Read returns and the run goroutine exits cleanly.
		//
		// Returning ErrServerClosed here would route through jsonrpc2's
		// fail() path, which uses atomic.Value.Store — incompatible
		// with the *errors.errorString type a subsequent Read failure
		// (e.g. *websocket.CloseError) would have. Closing the conn
		// avoids that interaction entirely.
		s.Logger().Info("exit requested")
		if s.conn != nil {
			go func() { _ = s.conn.Close() }()
		}
		return nil, nil
	default:
		// Unknown calls get MethodNotFound; unknown notifications are
		// silently ignored (LSP allows servers to ignore unknown notifs).
		// jsonrpc2 models the two as different concrete types under the
		// Request interface — only *Call has an ID and expects a reply.
		if req.IsCall() {
			return nil, jsonrpc2.NewError(jsonrpc2.MethodNotFound,
				"method "+req.Method()+" not implemented")
		}
		return nil, nil
	}
}

// initOptions mirrors the workspace's initializationOptions JSON. Only
// the registry block is read in M3; future milestones may add other
// configuration keys here.
type initOptions struct {
	Registry *schema.Config `json:"registry,omitempty"`
}

func (s *Server) dispatchInitialize(ctx context.Context, req rpcRequest) (any, error) {
	var params protocol.InitializeParams
	if err := protocol.Unmarshal(req.Params(), &params); err != nil {
		return nil, jsonrpc2.NewError(jsonrpc2.ParseError,
			"unmarshalling InitializeParams: "+err.Error())
	}
	s.Logger().Info("initialize",
		slog.String("client_name", clientName(&params)),
		slog.String("client_version", clientVersion(&params)),
	)
	// Capture client capabilities so initialized + didChangeConfiguration
	// know which standard LSP mechanisms to use.
	s.SetClientCapabilities(readClientCapabilities(&params))
	// I2 of incremental-sync: negotiate position encoding. The
	// LSP 3.17 field lives at capabilities.general.positionEncodings,
	// read via a side unmarshal targeted at just that path.
	s.SetPositionEncoding(negotiatePositionEncoding(json.RawMessage(req.Params())))
	// Capture configureSchemaLookup's result; defer logging until
	// after proto_files / chain have had their pass below. Otherwise
	// every registry-less workspace gets a scary
	// "schema validation disabled" WARN even when proto_files is
	// providing complete schema coverage — and the same warning
	// fires for actual no-schema-source workspaces, which means
	// users can't tell the two cases apart.
	registrySchemaErr := s.configureSchemaLookup(ctx, initOptionsFromInitializeParams(json.RawMessage(req.Params())))
	// Capture workspace root for go-to-definition's path resolution.
	// Newer clients use WorkspaceFolders (multi-root); single-root
	// clients use RootURI. RootURI is deprecated in LSP 3.6 in favor
	// of WorkspaceFolders, but the spec requires servers to keep
	// accepting it for older clients.
	//
	// We pick the first WorkspaceFolder we can convert to a file
	// path. monaco-vscode-api occasionally injects synthetic
	// `inmemory://` / `untitled://` folders when the host hasn't
	// pushed a real folder into the IWorkspaceService — taking those
	// blindly would set workspaceRoot to a URI no on-disk operation
	// can use. Falling through to RootURI in that case keeps the
	// LSP client working when the host sets RootURI via
	// `clientOptions.workspaceFolder` even though the global
	// workspaceFolders state isn't updated.
	var rootURI uri.URI
	folders, _ := params.WorkspaceFolders.Get()
	for _, f := range folders {
		candidate := f.URI
		if _, ok := URIToFilePath(candidate); ok {
			rootURI = candidate
			break
		}
	}
	if rootURI == "" && params.RootURI != nil { //nolint:staticcheck // intentional fallback for pre-3.6 single-root clients
		rootURI = *params.RootURI //nolint:staticcheck
	}
	if rootURI != "" {
		s.Workspaces().SetWorkspaceRoot(rootURI)
	}
	// The editor's UI locale (LSP 3.16). Selects which §7 catalog
	// protocheck violation messages render through; absent means
	// spec fallback messages.
	if params.Locale != nil && *params.Locale != "" {
		s.SetClientLocale(*params.Locale)
	}
	// Discover + parse .protolsp.pxf (workspace-config W1). Loaded
	// AFTER initializationOptions are processed so the schema-lookup
	// rebuild below sees the merged Tier 2 (file) + Tier 3 (seed)
	// state. Failure is soft — fileConfig stays nil and we continue
	// with whatever editor settings supplied.
	s.LoadWorkspaceFileConfig()
	// W2/W3: build the proto_files lookup BEFORE the registry
	// rebuild so the dial goroutine can read it via
	// SnapshotProtoFiles and wire it in as a WithFallback parent.
	s.RebuildRootProtoFilesLookup()
	if fc := s.SnapshotFileConfig(); fc != nil && fc.HasContent {
		s.RebuildLookupFromEffectiveConfig(ctx)
	}
	// Chameleon chain discovery (C1). Runs after fileConfig is in
	// place so LoadWorkspaceChain has the Chameleon block to read
	// from. Soft failure: chain stays nil on any error and the LSP
	// behaves as if chain mode is off.
	s.LoadWorkspaceChain()
	// Proto-authoring phase 3: build the import-path index so
	// cross-file .proto hover + go-to-def can resolve from a
	// workspace's buf.yaml + buf.lock (or the naive fallback
	// when neither is present). Soft failure inside LoadProtoIndex
	// keeps the index non-nil even when bufconfig parsing fails —
	// downstream handlers tolerate a nil index too.
	s.LoadProtoIndex()
	// With the root and lookups in place, swap the startup
	// (descriptor-less) protocheck validator for one built against
	// the effective §9.4 engine config — its function_libraries now
	// resolve. No-op in the OSS build; fingerprint-diffed otherwise.
	s.RebuildProtocheckValidator(ctx)

	// Schema-source status. Three outcomes:
	//   - Registry succeeded                   → silent (the normal
	//                                            happy path).
	//   - Registry failed, proto_files works   → INFO; the workspace
	//                                            still has schemas
	//                                            from the local source.
	//   - Neither source has anything          → WARN; parse-only mode,
	//                                            user should hear about it.
	// lookupMgr.Snapshot() returns proto_files when no registry lookup
	// is installed, so a non-nil snapshot means SOMETHING is serving
	// schemas. The internal lookup (chameleon + workspace_config) is
	// always available but doesn't count — registry-less workspaces
	// shouldn't think they have schema validation for their own types.
	if registrySchemaErr != nil {
		if s.Lookups().Snapshot() != nil {
			s.Logger().Info("registry schema source unavailable; serving schemas from proto_files",
				slog.String("reason", registrySchemaErr.Error()))
		} else {
			s.Logger().Warn("schema validation disabled — no registry and no proto_files configured",
				slog.String("reason", registrySchemaErr.Error()))
		}
	}

	syncIncremental := protocol.TextDocumentSyncKindIncremental
	openClose, changeKind := true, syncIncremental
	result := &initializeResult{
		Capabilities: protocol.ServerCapabilities{
			TextDocumentSync: &protocol.TextDocumentSyncOptions{
				OpenClose: &openClose,
				Change:    &changeKind,
			},
			// M4: hover with field type + provenance. The provider has
			// no extra options; presence of the field flips the
			// capability on for the client.
			HoverProvider: protocol.Boolean(true),
			// Post-MVP: field-name completion. Clients invoke
			// completion as the user types identifier characters,
			// which is the right UX for "suggest field names" — but
			// `@` is not an identifier character, so RFC-001
			// annotation-use completion in .proto files needs
			// it as an explicit trigger. Trigger on `{` and
			// `\n` could be added later for nested-block affordances.
			CompletionProvider: &protocol.CompletionOptions{
				TriggerCharacters: []string{"@"},
			},
			// Post-MVP: quick fixes for unknown-field diagnostics.
			// CodeActionKinds advertises which kinds we produce so
			// clients can grey out menu items we'll never return.
			CodeActionProvider: &protocol.CodeActionOptions{
				CodeActionKinds: []protocol.CodeActionKind{protocol.CodeActionKindQuickFix},
			},
			// Post-MVP: jump to a field's defining `.proto` source
			// at the position recorded in its SourceLocations.
			// Resolved against the workspace root the client sent in
			// initialize.
			DefinitionProvider: protocol.Boolean(true),
			// Document outline: hierarchical symbol tree built from
			// the parsed AST (@type / directives / @dataset / @proto
			// / blocks / fields). Powers the editor's outline view
			// + breadcrumbs and replaces vendored tokenizer-based
			// outline implementations.
			DocumentSymbolProvider: protocol.Boolean(true),
			// General block folding: standard pull-based
			// textDocument/foldingRange over all block structure in
			// both .pxf documents (nodes / children / state /
			// lifecycle blocks + directives) and .proto schemas
			// (message / enum / service / oneof / rpc / extend
			// bodies, the import group, comment blocks). Pure
			// structure — the @dataset regions push stays layered on
			// top for its row-count / auto-collapse enrichment.
			FoldingRangeProvider: protocol.Boolean(true),
			// Schema-driven semantic highlighting (LSP 3.16+).
			// Layers ON TOP of the editor's syntactic highlighter
			// — we emit tokens for schema-meaningful spans
			// (directive heads, field names, values) so clients
			// can mark deprecated fields, surface internal vs.
			// user-domain schemas, and color enum values.
			SemanticTokensProvider: &protocol.SemanticTokensOptions{
				Legend: SemanticTokensLegend(),
				Range:  protocol.Boolean(false),
				Full:   protocol.Boolean(true),
			},
			// Canonical PXF formatting via protowire-go's
			// FormatDocument. Client formatting options
			// (insertSpaces, tabSize) are advisory only — the
			// formatter mirrors gofmt's "one true layout"
			// approach so tooling agrees across IDE / CLI / CI.
			DocumentFormattingProvider: protocol.Boolean(true),
			// Workspace-scoped references + rename. Both walk
			// the open-document set and match dotted paths
			// (db.host) — clicking on a field in one chameleon
			// layer surfaces every layer that overrides it.
			// On-disk indexing deferred.
			ReferencesProvider: protocol.Boolean(true),
			RenameProvider:     protocol.Boolean(true),
			PositionEncoding:   protocol.PositionEncodingKind(s.PositionEncoding().String()),
		},
		ServerInfo: &serverInfo{
			Name:    "protolsp",
			Version: serverVersion(),
		},
	}
	// Spin up the Connect-RPC sidecar and advertise its address +
	// auth token in serverInfo so clients can connect without
	// out-of-band coordination. Failure is non-fatal: protolsp keeps
	// serving LSP and clients fall back to the legacy custom-method
	// surface.
	if endpoint, err := s.startRPC(); err != nil {
		s.Logger().Warn("rpc sidecar disabled", "err", err.Error())
	} else {
		result.ServerInfo.RPC = endpoint
	}
	return result, nil
}

// initializeResult is protocol.InitializeResult with protolsp's
// serverInfo, which carries the Connect-RPC discovery payload.
type initializeResult struct {
	Capabilities protocol.ServerCapabilities `json:"capabilities"`
	ServerInfo   *serverInfo                 `json:"serverInfo,omitempty"`
}

// serverInfo mirrors protocol.ServerInfo + the Connect-RPC discovery
// payload (rpc). Editors that don't know about `rpc` ignore the
// extra key; editors that do (pxfed v0.x+) read it to connect to the
// sidecar over loopback.
type serverInfo struct {
	Name    string       `json:"name"`
	Version string       `json:"version,omitempty"`
	RPC     *rpcEndpoint `json:"rpc,omitempty"`
}

// negotiatePositionEncoding extracts client-advertised position
// encodings from the raw initialize params and picks the best
// match via positions.Negotiate. The LSP 3.17 field lives at
// capabilities.general.positionEncodings, read here from the raw
// JSON. Missing field → UTF-16
// default.
func negotiatePositionEncoding(raw json.RawMessage) positions.Encoding {
	if len(raw) == 0 {
		return positions.UTF16
	}
	var probe struct {
		Capabilities struct {
			General struct {
				PositionEncodings []string `json:"positionEncodings"`
			} `json:"general"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return positions.UTF16
	}
	return positions.Negotiate(probe.Capabilities.General.PositionEncodings)
}

// initOptionsFromInitializeParams extracts the
// `initializationOptions` field from an `initialize` request's
// raw JSON params, returning its bytes ready for direct unmarshal.
// Avoids the marshal-then-unmarshal round trip the previous
// `interface{}` signature forced — protocol.InitializeParams
// types InitializationOptions as `interface{}`, so json.Unmarshal
// hydrates it as map[string]any, which we'd have to re-marshal
// before re-parsing into our typed initOptions shape.
//
// Returns nil when the field is absent or null. Parse errors
// flow through as nil too; configureSchemaLookup treats
// "no options" identically.
func initOptionsFromInitializeParams(raw json.RawMessage) json.RawMessage {
	var probe struct {
		InitializationOptions json.RawMessage `json:"initializationOptions"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	return probe.InitializationOptions
}

// configureSchemaLookup parses the workspace registry config and opens
// a Lookup. Empty / absent config is a clean no-op (schema validation
// stays off). A dial failure returns an error to the caller; the
// caller logs and continues without validation.
//
// Tests that have already set s.lookup directly (bypassing this code
// path) keep their fake — we only overwrite when raw config is
// present.
func (s *Server) configureSchemaLookup(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var opts initOptions
	if err := json.Unmarshal(raw, &opts); err != nil {
		return fmt.Errorf("unmarshalling initializationOptions: %w", err)
	}
	if opts.Registry == nil {
		return nil
	}
	// Seed Tier 3 with the initializationOptions registry block. When
	// the client supports workspace/configuration (CP1), its returned
	// values may override these at initialized time; until then this
	// is the authoritative config.
	seed := s.SnapshotEditorConfig()
	seed.Registry = *opts.Registry
	s.SwapEditorConfig(seed)
	s.SetSeededFromInitOptions(true)
	lookup, closer, err := schema.Open(ctx, *opts.Registry)
	if err != nil {
		return err
	}
	if lookup == nil {
		return nil
	}
	if oldCloser := s.Lookups().Swap(lookup, closer); oldCloser != nil {
		// Initialize is single-shot; in practice oldCloser is nil
		// here. Closing defensively (cheap, safe) in case a future
		// caller path re-enters this code path.
		_ = oldCloser.Close()
	}
	// Track the config we built FROM so the async rebuild's diff
	// check has a baseline. Without this, a subsequent
	// RebuildLookupAsync that computes the same effective config
	// would still see the manager's zero-value baseline and
	// trigger a redundant dial.
	s.Lookups().RecordBaseline(*opts.Registry)
	s.Logger().Info("schema validation enabled",
		slog.String("registry", opts.Registry.Address),
		slog.String("namespace", opts.Registry.Namespace),
	)
	return nil
}

func clientName(p *protocol.InitializeParams) string {
	if p.ClientInfo.Name == "" {
		return "unknown"
	}
	return p.ClientInfo.Name
}

func clientVersion(p *protocol.InitializeParams) string {
	v, _ := p.ClientInfo.Version.Get()
	return v
}
