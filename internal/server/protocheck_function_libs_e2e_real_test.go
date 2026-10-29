//go:build protocheck

// End-to-end coverage for the §9.4 function_libraries wiring
// : a protowire.config.textproto naming a function library
// makes the protocheck/v2 validator register the library's declared
// functions (lenient §9.2 verification), so data hitting a rule
// that calls a declared-but-unimplemented function surfaces a
// CodeFunctionUnimplemented finding — downgraded to Information,
// because the data isn't known to be wrong.
//
// The library fixture is compiled through the fork's incremental IR
// pipeline (the same lowering `pxf build` uses) so the descriptor
// carries the RFC-001 extensions protocheck's carrier index and
// function verification read: 1327 (annotation lists with
// Expression.calls) and 1328 (FileFunctions). The vendored
// annotation library under testdata/protocheck/proto/protowire/
// schema/v1/annotations.proto supplies the canonical @validate.
package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protocompile/fdp"
	"github.com/trendvidia/protocompile/incremental"
	"github.com/trendvidia/protocompile/incremental/queries"
	"github.com/trendvidia/protocompile/ir"
	"github.com/trendvidia/protocompile/report"
	"github.com/trendvidia/protocompile/source"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

const fnlibImportPath = "protolsp/test/fnlib.proto"

// fnlibSrc declares one function and one message whose field rule
// calls it. With no implementation registered, validating a Gadget
// triggers the lenient Unimplemented placeholder. The @validate
// deliberately carries no `code` override: §7 precedence puts a
// use-site code above the cause's, so an override would re-brand
// the placeholder's reserved protowire.function.unimplemented and
// the diagnostic downgrade wouldn't (and shouldn't) engage.
const fnlibSrc = `syntax = "proto3";

package protolsp.test.fnlib;

import "protowire/schema/v1/annotations.proto";

function is_special(value: string);

message Gadget {
  string name = 1
    @validate(is_special(this));
}
`

// compileRFC001Library compiles a protowire-syntax source through
// the fork's incremental pipeline and re-links it (plus deps) via
// protodesc so the carriers decode against the in-process gencode
// registry — the same recipe protocheck/v2's own conformance tests
// use.
func compileRFC001Library(t *testing.T, path, src string) protoreflect.FileDescriptor {
	t.Helper()
	const annotationsImport = "protowire/schema/v1/annotations.proto"
	annotations, err := os.ReadFile(filepath.Join("testdata/protocheck/proto", annotationsImport))
	require.NoError(t, err)

	opener := &source.Openers{
		source.NewMap(map[string]*source.File{
			path:              source.NewFile(path, src),
			annotationsImport: source.NewFile(annotationsImport, string(annotations)),
		}),
		source.WKTs(),
	}
	exec := incremental.New()
	sess := new(ir.Session)
	results, rep, err := incremental.Run(context.Background(), exec,
		queries.IR{Opener: opener, Session: sess, Path: path})
	require.NoError(t, err)
	if rep != nil {
		for _, d := range rep.Diagnostics {
			require.Greater(t, d.Level(), report.Error, "compile diagnostic: %s", d.Message())
		}
	}
	require.Len(t, results, 1)
	require.NotNil(t, results[0].Value)

	set := &descriptorpb.FileDescriptorSet{}
	inSet := map[string]bool{}
	var addWithDeps func(fdproto *descriptorpb.FileDescriptorProto)
	addWithDeps = func(fdproto *descriptorpb.FileDescriptorProto) {
		if inSet[fdproto.GetName()] {
			return
		}
		inSet[fdproto.GetName()] = true
		for _, dep := range fdproto.GetDependency() {
			if inSet[dep] {
				continue
			}
			if strings.HasPrefix(dep, "google/protobuf/") {
				known, err := protoregistry.GlobalFiles.FindFileByPath(dep)
				require.NoError(t, err)
				addWithDeps(protodesc.ToFileDescriptorProto(known))
				continue
			}
			depResults, _, err := incremental.Run(context.Background(), exec,
				queries.IR{Opener: opener, Session: sess, Path: dep})
			require.NoError(t, err)
			require.Len(t, depResults, 1)
			require.NotNil(t, depResults[0].Value)
			depProto, err := fdp.DescriptorProto(depResults[0].Value, fdp.IncludeSourceCodeInfo(true))
			require.NoError(t, err)
			addWithDeps(depProto)
		}
		set.File = append(set.File, fdproto)
	}
	fdproto, err := fdp.DescriptorProto(results[0].Value, fdp.IncludeSourceCodeInfo(true))
	require.NoError(t, err)
	addWithDeps(fdproto)

	reg, err := protodesc.NewFiles(set)
	require.NoError(t, err)
	fd, err := reg.FindFileByPath(path)
	require.NoError(t, err)
	return fd
}

// installFnlibLookup registers the compiled library's message types
// in a fakeLookup and swaps it into the server. The swap fires the
// lookup-changed rebuild trigger, so after this call the validator
// was rebuilt against whatever engine config is in effect.
func installFnlibLookup(t *testing.T, srv *Server) {
	t.Helper()
	file := compileRFC001Library(t, fnlibImportPath, fnlibSrc)
	lookup := newFakeLookup()
	msgs := file.Messages()
	for i := 0; i < msgs.Len(); i++ {
		lookup.add(dynamicpb.NewMessageType(msgs.Get(i)))
	}
	srv.Lookups().Swap(lookup, nil)
}

func writeEngineConfig(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "protowire.config.textproto")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

const gadgetPxf = `@type protolsp.test.fnlib.Gadget
name = "widget"`

// lastProtocheckDiags returns the protocheck-source diagnostics of
// the most recent publish for the given URI.
func lastProtocheckDiags(t *testing.T, fc *fakeConn, docURI string) []protocol.Diagnostic {
	t.Helper()
	notes := fc.calls()
	require.NotEmpty(t, notes)
	for i := len(notes) - 1; i >= 0; i-- {
		if string(notes[i].params.URI) == docURI {
			return findProtocheckDiags(notes[i].params.Diagnostics)
		}
	}
	t.Fatalf("no publish for %s", docURI)
	return nil
}

func TestProtocheckFunctionLibs_E2E_UnimplementedFunctionInformation(t *testing.T) {
	t.Setenv("PROTOWIRE_CONFIG", "")
	workspaceDir := t.TempDir()
	writeEngineConfig(t, workspaceDir, `function_libraries: "`+fnlibImportPath+`"`+"\n")

	srv, fc := initializeWithWatchSupport(t, workspaceDir)
	// The swap-triggered rebuild resolves the config's library
	// through the fresh lookup (miss → hit flips the fingerprint).
	installFnlibLookup(t, srv)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/gadget.pxf", 1, gadgetPxf)))

	diags := lastProtocheckDiags(t, fc, "file:///work/gadget.pxf")
	require.Len(t, diags, 1, "the is_special call hits the lenient §9.2 placeholder")
	assert.True(t, hasDiagWithCode(diags, "protowire.function.unimplemented"),
		"finding carries the reserved CodeFunctionUnimplemented")
	assert.Equal(t, protocol.DiagnosticSeverityInformation, diags[0].Severity,
		"unimplemented-function findings are informational, not errors")
	assert.Contains(t, diags[0].Message, "protolsp.test.fnlib.is_special",
		"fallback message names the unimplemented FQN")

	// Carrier rules embed a source map, so the finding links
	// back to the @validate declaration in the library schema.
	require.Len(t, diags[0].RelatedInformation, 1,
		"carrier-rule findings carry a 'go to rule' link")
	rel := diags[0].RelatedInformation[0]
	assert.True(t, strings.HasSuffix(string(rel.Location.URI), "/"+fnlibImportPath),
		"link resolves the SourceRef file through the definition precedence, got %s", rel.Location.URI)
	assert.Positive(t, rel.Location.Range.Start.Line,
		"declaration-site range anchors past line 0 (the rule isn't on the first line)")
	assert.Equal(t, "constraint declared here", rel.Message)
}

func TestProtocheckFunctionLibs_E2E_NoConfigNoCarrierRules(t *testing.T) {
	t.Setenv("PROTOWIRE_CONFIG", "")
	workspaceDir := t.TempDir() // no protowire.config.textproto

	srv, fc := initializeWithWatchSupport(t, workspaceDir)
	installFnlibLookup(t, srv)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/gadget.pxf", 1, gadgetPxf)))

	diags := lastProtocheckDiags(t, fc, "file:///work/gadget.pxf")
	assert.Empty(t, diags,
		"without function_libraries the validator indexes no carrier rules for the library's messages")
}

func TestProtocheckFunctionLibs_E2E_ConfigWatchTriggersRebuild(t *testing.T) {
	t.Setenv("PROTOWIRE_CONFIG", "")
	workspaceDir := t.TempDir()

	srv, fc := initializeWithWatchSupport(t, workspaceDir)
	installFnlibLookup(t, srv)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/gadget.pxf", 1, gadgetPxf)))
	require.Empty(t, lastProtocheckDiags(t, fc, "file:///work/gadget.pxf"),
		"no engine config yet — no carrier rules")

	// Drop the config on disk and deliver the watched-file event
	// (window 0 → synchronous rebuild + reanalyze).
	configPath := writeEngineConfig(t, workspaceDir, `function_libraries: "`+fnlibImportPath+`"`+"\n")
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		fileEventCall(t, []*protocol.FileEvent{
			{URI: uri.File(configPath), Type: protocol.FileChangeTypeCreated},
		})))

	diags := lastProtocheckDiags(t, fc, "file:///work/gadget.pxf")
	require.Len(t, diags, 1, "config creation rebuilt the validator and re-analyzed the open doc")
	assert.True(t, hasDiagWithCode(diags, "protowire.function.unimplemented"))
}

func TestProtocheckFunctionLibs_E2E_UnresolvableLibraryKeepsValidation(t *testing.T) {
	t.Setenv("PROTOWIRE_CONFIG", "")
	workspaceDir := t.TempDir()
	writeEngineConfig(t, workspaceDir, `function_libraries: "no/such/library.proto"`+"\n")

	srv, fc := initializeWithWatchSupport(t, workspaceDir)
	installFixtureLookup(t, srv) // the protocheckpb User/Transfer fixture

	src := `@type protolsp.test.protocheck.User
email = "not-an-email"
age = 30
score = 50.0`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	diags := lastProtocheckDiags(t, fc, "file:///work/user.pxf")
	require.Len(t, diags, 1,
		"a library that doesn't resolve must not disable validation")
	assert.True(t, hasDiagWithCode(diags, "string.email"))
	assert.Equal(t, protocol.DiagnosticSeverityError, diags[0].Severity)
}
