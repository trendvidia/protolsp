//go:build protocheck

// End-to-end harness for the Phase 1 .pxf data validation.
// Compiles a fixture .proto carrying protocheck annotations,
// installs the resulting MessageTypes in the Server's lookup,
// opens .pxf documents through the standard didOpen pipeline,
// and asserts that protocheck violations surface as LSP
// diagnostics with source "protocheck".
//
// This closes the test-coverage gap the Phase 1 PR called out:
// the resolver + emitter were covered as units, but the full
// path through validate() + Validator.Validate against a
// descriptor that actually carries protocheck extensions was
// not exercised. The fixture .proto under
// testdata/protocheck/user_constraints.proto is the source of
// truth for the constraints; assertions below mirror it 1:1.
package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protocompile"
	"github.com/trendvidia/protocompile/reporter"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// compileProtocheckFixture compiles testdata/protocheck/
// user_constraints.proto against protocheck.proto and returns the
// resulting FileDescriptor. Standard imports
// (google/protobuf/descriptor.proto, etc.) are served by
// protocompile's built-in resolver; protocheck.proto is served
// from the vendored copy under testdata/wellknown (the fork's
// compile pipeline consumes source only — a pre-linked descriptor
// can no longer be injected via SearchResult.Desc).
//
// Because protocheck.proto is compiled from source, the compiled
// descriptor's protocheck extensions are typed by the per-compile
// registry — a DIFFERENT ExtensionType instance than protocheckpb's
// gencode, and protocheck-go's GetExtension calls look the gencode
// type up by identity. The descriptor is therefore re-anchored onto
// the global registry: FileDescriptorProto → marshal → unmarshal
// with GlobalTypes (extensions re-typed by gencode) → protodesc
// against GlobalFiles (protocheck.proto resolves to the gencode
// file). This is the same round-trip the pre-v1.1.0 harness used;
// injecting the gencode descriptor directly into the compiler
// (SearchResult.Desc) is what made it unnecessary before.
func compileProtocheckFixture(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	const fixtureRel = "testdata/protocheck"
	const fixtureMain = "user_constraints.proto"

	absRoot, err := filepath.Abs(fixtureRel)
	require.NoError(t, err)
	protocheckPath, err := filepath.Abs("testdata/wellknown/protocheck/protocheck.proto")
	require.NoError(t, err)

	resolver := protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
		switch path {
		case fixtureMain:
			b, err := os.ReadFile(filepath.Join(absRoot, fixtureMain))
			if err != nil {
				return protocompile.SearchResult{}, err
			}
			return protocompile.SearchResult{Source: bytes.NewReader(b)}, nil
		case "protocheck/protocheck.proto":
			b, err := os.ReadFile(protocheckPath)
			if err != nil {
				return protocompile.SearchResult{}, err
			}
			return protocompile.SearchResult{Source: bytes.NewReader(b)}, nil
		}
		return protocompile.SearchResult{}, protoregistry.NotFound
	})

	// Fail-loud reporter: any parse / link error is a fixture
	// bug, so we want it to surface immediately with file +
	// position context instead of being collapsed into the
	// generic "parse failed" Compile error.
	rep := reporter.NewReporter(
		func(e reporter.ErrorWithPos) error {
			t.Errorf("protocompile error: %s", e.Error())
			return nil
		},
		func(e reporter.ErrorWithPos) {
			t.Logf("protocompile warning: %s", e.Error())
		},
	)
	c := &protocompile.Compiler{
		Resolver:       protocompile.WithStandardImports(resolver),
		Reporter:       rep,
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	files, err := c.Compile(context.Background(), fixtureMain)
	require.NoError(t, err, "fixture compile must succeed for the e2e test to be meaningful")
	require.Len(t, files, 1)

	fdp := protodesc.ToFileDescriptorProto(files[0])
	raw, err := proto.Marshal(fdp)
	require.NoError(t, err)
	typed := &descriptorpb.FileDescriptorProto{}
	require.NoError(t, proto.UnmarshalOptions{Resolver: protoregistry.GlobalTypes}.Unmarshal(raw, typed))
	fd, err := protodesc.NewFile(typed, protoregistry.GlobalFiles)
	require.NoError(t, err, "re-anchoring the fixture onto the gencode registry must succeed")
	return fd
}

// installFixtureLookup compiles the fixture and registers each
// top-level message in a fakeLookup swapped into the Server. The
// FQN of the test messages is
// "protolsp.test.protocheck.{User,Transfer}".
func installFixtureLookup(t *testing.T, srv *Server) {
	t.Helper()
	file := compileProtocheckFixture(t)
	lookup := newFakeLookup()
	msgs := file.Messages()
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		lookup.add(dynamicpb.NewMessageType(md))
	}
	srv.Lookups().Swap(lookup, nil)
}

// findProtocheckDiags filters a notification's diagnostics to the
// "protocheck" source — the Phase 1 output. The full diagnostic
// list also includes structural / schema findings the test
// doesn't care about.
func findProtocheckDiags(diags []protocol.Diagnostic) []protocol.Diagnostic {
	var out []protocol.Diagnostic
	for _, d := range diags {
		if optValue(d.Source) == "protocheck" {
			out = append(out, d)
		}
	}
	return out
}

// hasDiagWithCode reports whether any diagnostic in the list
// carries the given ConstraintID in its Code field.
func hasDiagWithCode(diags []protocol.Diagnostic, code string) bool {
	for _, d := range diags {
		if c, ok := d.Code.(protocol.String); ok && string(c) == code {
			return true
		}
	}
	return false
}

func TestProtocheckDataValidation_E2E_StringEmail(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installFixtureLookup(t, srv)

	// `email = "not-an-email"` violates string.email; everything
	// else (age, score) is in bounds so the only diagnostic from
	// the protocheck source should be the email one.
	src := `@type protolsp.test.protocheck.User
email = "not-an-email"
age = 30
score = 50.0`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtocheckDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 1, "exactly one protocheck diagnostic — the bad email")
	assert.True(t, hasDiagWithCode(diags, "string.email"),
		"diagnostic carries the string.email constraint id")
	assert.Equal(t, protocol.DiagnosticSeverityError, diags[0].Severity)
}

func TestProtocheckDataValidation_E2E_Int32OutOfRange(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installFixtureLookup(t, srv)

	src := `@type protolsp.test.protocheck.User
email = "alice@example.com"
age = 200
score = 50.0`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtocheckDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 1, "exactly one protocheck diagnostic — age over lte")
	assert.True(t, hasDiagWithCode(diags, "int32.lte"))
}

func TestProtocheckDataValidation_E2E_CustomExpression(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installFixtureLookup(t, srv)

	// score=200 violates the GoVM expression
	// `this >= 0.0 && this <= 100.0`. The constraint's id is
	// "score_range" per the fixture.
	src := `@type protolsp.test.protocheck.User
email = "alice@example.com"
age = 30
score = 200.0`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtocheckDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 1, "exactly one protocheck diagnostic — score expression")
	assert.True(t, hasDiagWithCode(diags, "score_range"))
}

func TestProtocheckDataValidation_E2E_MultipleViolations(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installFixtureLookup(t, srv)

	// Three violations: bad email, age out of range, score out
	// of range. Confirms the validator surfaces all findings,
	// not just the first.
	src := `@type protolsp.test.protocheck.User
email = "not-an-email"
age = 200
score = 200.0`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtocheckDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 3)
	assert.True(t, hasDiagWithCode(diags, "string.email"))
	assert.True(t, hasDiagWithCode(diags, "int32.lte"))
	assert.True(t, hasDiagWithCode(diags, "score_range"))
}

func TestProtocheckDataValidation_E2E_AllValidEmitsNothing(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installFixtureLookup(t, srv)

	src := `@type protolsp.test.protocheck.User
email = "alice@example.com"
age = 30
score = 75.5`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Empty(t, findProtocheckDiags(notes[0].params.Diagnostics),
		"well-formed data emits zero protocheck diagnostics")
}

func TestProtocheckDataValidation_E2E_MessageLevelCrossField(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installFixtureLookup(t, srv)

	// from_account == to_account triggers the
	// (protocheck.message) check.
	src := `@type protolsp.test.protocheck.Transfer
from_account = "acc-1"
to_account = "acc-1"
amount = 100.0`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/transfer.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtocheckDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 1)
	assert.True(t, hasDiagWithCode(diags, "different_accounts"))
}

// TestProtocheckDataValidation_E2E_FieldPathPositionRange verifies
// that the FieldPath → pxf-position resolver puts the diagnostic
// at the offending field's line, not at the document origin. This
// is the integration assertion for resolveProtocheckPath beyond
// the existing unit tests.
func TestProtocheckDataValidation_E2E_FieldPathPositionRange(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installFixtureLookup(t, srv)

	// `age = 200` is on line 3 (1-based); the LSP diagnostic
	// should land at line 2 (0-based).
	src := `@type protolsp.test.protocheck.User
email = "alice@example.com"
age = 200
score = 50.0`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtocheckDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 1)
	assert.Equal(t, uint32(2), diags[0].Range.Start.Line,
		"diagnostic anchored to the `age = 200` line, not the document origin")
}

// guard against drift between the fixture's MessageType FQN and
// the test assertions above. If someone renames the package in
// user_constraints.proto without updating the tests, this fires
// before the @type lookup failure obscures the real cause.
func TestProtocheckDataValidation_E2E_FixtureFQN(t *testing.T) {
	file := compileProtocheckFixture(t)
	require.NotNil(t, file.Messages().ByName("User"))
	require.NotNil(t, file.Messages().ByName("Transfer"))
	require.Equal(t, protoreflect.FullName("protolsp.test.protocheck.User"),
		file.Messages().ByName("User").FullName())
}
