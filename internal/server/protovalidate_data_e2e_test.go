// End-to-end harness for Phase 1 protovalidate runtime
// validation. Mirrors the protocheck e2e harness but uses
// buf.validate annotations and serves buf/validate/validate.proto
// from the vendored copy under testdata/wellknown (the fork's
// compile pipeline consumes source only — a pre-linked descriptor
// can no longer be injected via SearchResult.Desc).
//
// Tag-free — the protovalidate side ships in both the OSS and
// trendvidia builds since protovalidate-go is Apache 2.0.
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
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

func compileProtovalidateFixture(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	const fixtureRel = "testdata/protovalidate"
	const fixtureMain = "user_constraints.proto"

	absRoot, err := filepath.Abs(fixtureRel)
	require.NoError(t, err)
	validatePath, err := filepath.Abs("testdata/wellknown/buf/validate/validate.proto")
	require.NoError(t, err)

	resolver := protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
		switch path {
		case fixtureMain:
			b, err := os.ReadFile(filepath.Join(absRoot, fixtureMain))
			if err != nil {
				return protocompile.SearchResult{}, err
			}
			return protocompile.SearchResult{Source: bytes.NewReader(b)}, nil
		case "buf/validate/validate.proto":
			b, err := os.ReadFile(validatePath)
			if err != nil {
				return protocompile.SearchResult{}, err
			}
			return protocompile.SearchResult{Source: bytes.NewReader(b)}, nil
		}
		return protocompile.SearchResult{}, protoregistry.NotFound
	})

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
	return files[0]
}

func installProtovalidateFixtureLookup(t *testing.T, srv *Server) {
	t.Helper()
	file := compileProtovalidateFixture(t)
	lookup := newFakeLookup()
	msgs := file.Messages()
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		lookup.add(dynamicpb.NewMessageType(md))
	}
	srv.Lookups().Swap(lookup, nil)
}

func findProtovalidateDiags(diags []protocol.Diagnostic) []protocol.Diagnostic {
	var out []protocol.Diagnostic
	for _, d := range diags {
		if d.Source == "buf/validate" {
			out = append(out, d)
		}
	}
	return out
}

func TestProtovalidateDataValidation_E2E_StringEmail(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installProtovalidateFixtureLookup(t, srv)

	src := `@type protolsp.test.protovalidate.User
email = "not-an-email"
age = 30
score = 50.0`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtovalidateDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 1, "exactly one buf/validate diagnostic — the bad email")
	assert.Equal(t, protocol.DiagnosticSeverityError, diags[0].Severity)
}

func TestProtovalidateDataValidation_E2E_Int32OutOfRange(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installProtovalidateFixtureLookup(t, srv)

	src := `@type protolsp.test.protovalidate.User
email = "alice@example.com"
age = 200
score = 50.0`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtovalidateDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 1, "exactly one buf/validate diagnostic — age over lte")
}

func TestProtovalidateDataValidation_E2E_CelExpression(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installProtovalidateFixtureLookup(t, srv)

	src := `@type protolsp.test.protovalidate.User
email = "alice@example.com"
age = 30
score = 200.0`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtovalidateDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 1, "exactly one buf/validate diagnostic — score CEL")
	assert.Equal(t, "score_range", diags[0].Code)
}

func TestProtovalidateDataValidation_E2E_MultipleViolations(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installProtovalidateFixtureLookup(t, srv)

	src := `@type protolsp.test.protovalidate.User
email = "not-an-email"
age = 200
score = 200.0`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtovalidateDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 3)
}

func TestProtovalidateDataValidation_E2E_AllValidEmitsNothing(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installProtovalidateFixtureLookup(t, srv)

	src := `@type protolsp.test.protovalidate.User
email = "alice@example.com"
age = 30
score = 75.5`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/user.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Empty(t, findProtovalidateDiags(notes[0].params.Diagnostics))
}

func TestProtovalidateDataValidation_E2E_MessageLevelCel(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	installProtovalidateFixtureLookup(t, srv)

	src := `@type protolsp.test.protovalidate.Transfer
from_account = "acc-1"
to_account = "acc-1"
amount = 100.0`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/transfer.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findProtovalidateDiags(notes[0].params.Diagnostics)
	require.Len(t, diags, 1)
	assert.Equal(t, "different_accounts", diags[0].Code)
}

func TestProtovalidateDataValidation_E2E_FixtureFQN(t *testing.T) {
	file := compileProtovalidateFixture(t)
	require.NotNil(t, file.Messages().ByName("User"))
	require.NotNil(t, file.Messages().ByName("Transfer"))
	require.Equal(t, protoreflect.FullName("protolsp.test.protovalidate.User"),
		file.Messages().ByName("User").FullName())
}
