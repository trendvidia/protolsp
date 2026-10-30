//go:build protocheck

// End-to-end coverage for §7 catalog i18n: a
// protowire.config.textproto naming catalog_libraries makes
// protocheck violation messages render through the locale catalog
// matching the client's InitializeParams.locale, falling back to
// the spec message on any miss. Catalog files are watched: an
// on-disk edit re-derives the validator (the rebuild fingerprint
// folds catalog content) and open documents re-analyze.
//
// Reuses the function-library fixtures from
// protocheck_function_libs_e2e_real_test.go — the unimplemented-
// function violation is a convenient stable code to translate.
package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const catalogConfigWithLibs = `function_libraries: "protolsp/test/fnlib.proto"
catalog_libraries: "i18n/de.textproto"
`

const germanCatalog = `locale: "de"
entries {
  key: "protowire.function.unimplemented"
  value: "Funktion nicht implementiert"
}
`

func writeGermanCatalog(t *testing.T, dir, template string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "i18n"), 0o750))
	path := filepath.Join(dir, "i18n", "de.textproto")
	content := `locale: "de"
entries {
  key: "protowire.function.unimplemented"
  value: "` + template + `"
}
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// initializeWithLocale mirrors initializeWithWatchSupport but also
// sends the client's UI locale (LSP 3.16 InitializeParams.locale).
func initializeWithLocale(t *testing.T, workspaceDir, locale string) (*Server, *fakeConn) {
	t.Helper()
	srv, fc := newServerWithFakeConn(t)
	srv.SetWatchedFilesWindow(0)
	params := &protocol.InitializeParams{
		ClientInfo: protocol.ClientInfo{Name: "test"},
		Locale:     &locale,
		WorkspaceFoldersInitializeParams: workspaceFolders(
			protocol.WorkspaceFolder{URI: uri.File(workspaceDir), Name: "root"},
		),
		Capabilities: protocol.ClientCapabilities{
			Workspace: &protocol.WorkspaceClientCapabilities{
				DidChangeWatchedFiles: &protocol.DidChangeWatchedFilesClientCapabilities{
					DynamicRegistration: boolRef(true),
				},
			},
		},
	}
	call, err := newTestCall(jsonrpc2.NewNumberID(1), protocol.MethodInitialize, params)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, call))
	initCall, err := newTestCall(jsonrpc2.NewNumberID(2), protocol.MethodInitialized, nil)
	require.NoError(t, err)
	require.NoError(t, testHandle(srv, context.Background(), noopReply, initCall))
	srv.WaitInitialized()
	return srv, fc
}

func TestProtocheckCatalogI18n_E2E_LocaleRendersCatalogMessage(t *testing.T) {
	t.Setenv("PROTOWIRE_CONFIG", "")
	workspaceDir := t.TempDir()
	writeEngineConfig(t, workspaceDir, catalogConfigWithLibs)
	writeGermanCatalog(t, workspaceDir, "Funktion nicht implementiert")

	// de-DE negotiates onto the "de" catalog via the primary
	// language subtag.
	srv, fc := initializeWithLocale(t, workspaceDir, "de-DE")
	installFnlibLookup(t, srv)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/gadget.pxf", 1, gadgetPxf)))

	diags := lastProtocheckDiags(t, fc, "file:///work/gadget.pxf")
	require.Len(t, diags, 1)
	assert.Equal(t, "Funktion nicht implementiert", messageText(diags[0].Message),
		"violation message renders through the de catalog")
	assert.True(t, hasDiagWithCode(diags, "protowire.function.unimplemented"),
		"the code stays the engine's — only the message localizes")
	assert.Equal(t, protocol.DiagnosticSeverityInformation, diags[0].Severity,
		"severity classification keys on the code, not the rendered text")
}

func TestProtocheckCatalogI18n_E2E_NoLocaleFallsBack(t *testing.T) {
	t.Setenv("PROTOWIRE_CONFIG", "")
	workspaceDir := t.TempDir()
	writeEngineConfig(t, workspaceDir, catalogConfigWithLibs)
	writeGermanCatalog(t, workspaceDir, "Funktion nicht implementiert")

	// No locale in initialize → spec fallback message even though
	// a catalog is loaded.
	srv, fc := initializeWithLocale(t, workspaceDir, "")
	installFnlibLookup(t, srv)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/gadget.pxf", 1, gadgetPxf)))

	diags := lastProtocheckDiags(t, fc, "file:///work/gadget.pxf")
	require.Len(t, diags, 1)
	assert.Contains(t, messageText(diags[0].Message), "not implemented",
		"without a client locale the spec fallback message renders")
}

func TestProtocheckCatalogI18n_E2E_UnmatchedLocaleFallsBack(t *testing.T) {
	t.Setenv("PROTOWIRE_CONFIG", "")
	workspaceDir := t.TempDir()
	writeEngineConfig(t, workspaceDir, catalogConfigWithLibs)
	writeGermanCatalog(t, workspaceDir, "Funktion nicht implementiert")

	srv, fc := initializeWithLocale(t, workspaceDir, "pt-BR")
	installFnlibLookup(t, srv)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/gadget.pxf", 1, gadgetPxf)))

	diags := lastProtocheckDiags(t, fc, "file:///work/gadget.pxf")
	require.Len(t, diags, 1)
	assert.Contains(t, messageText(diags[0].Message), "not implemented",
		"a locale with no catalog renders the spec fallback")
}

func TestProtocheckCatalogI18n_E2E_CatalogEditTriggersRebuild(t *testing.T) {
	t.Setenv("PROTOWIRE_CONFIG", "")
	workspaceDir := t.TempDir()
	writeEngineConfig(t, workspaceDir, catalogConfigWithLibs)
	catalogPath := writeGermanCatalog(t, workspaceDir, "Funktion nicht implementiert")

	srv, fc := initializeWithLocale(t, workspaceDir, "de")
	installFnlibLookup(t, srv)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/gadget.pxf", 1, gadgetPxf)))
	require.Equal(t, "Funktion nicht implementiert",
		lastProtocheckDiags(t, fc, "file:///work/gadget.pxf")[0].Message)

	// Edit the catalog on disk and deliver the watched-file event.
	// The classifier matches the file against the loaded catalog
	// paths; the rebuild fingerprint folds catalog content, so the
	// same config re-derives with the new template.
	writeGermanCatalog(t, workspaceDir, "Bitte Implementierung registrieren")
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(catalogPath), Type: protocol.FileChangeTypeChanged},
		})))

	diags := lastProtocheckDiags(t, fc, "file:///work/gadget.pxf")
	require.Len(t, diags, 1)
	assert.Equal(t, "Bitte Implementierung registrieren", messageText(diags[0].Message),
		"catalog edit re-rendered the open document's diagnostics")
}

// Guard: unrelated .textproto edits must not trigger the
// engine-config cascade (the glob over-matches by design; the
// classifier is the precise half).
func TestProtocheckCatalogI18n_E2E_UnrelatedTextprotoIgnored(t *testing.T) {
	t.Setenv("PROTOWIRE_CONFIG", "")
	workspaceDir := t.TempDir()
	writeEngineConfig(t, workspaceDir, catalogConfigWithLibs)
	writeGermanCatalog(t, workspaceDir, "Funktion nicht implementiert")

	srv, _ := initializeWithLocale(t, workspaceDir, "de")
	installFnlibLookup(t, srv)
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/gadget.pxf", 1, gadgetPxf)))
	baseline := srv.AnalyzeCount()

	other := filepath.Join(workspaceDir, "unrelated.textproto")
	require.NoError(t, os.WriteFile(other, []byte(`foo: "bar"`), 0o600))
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(other), Type: protocol.FileChangeTypeCreated},
		})))

	assert.Equal(t, baseline, srv.AnalyzeCount(),
		"an unrelated textproto event must not re-analyze open documents")
}
