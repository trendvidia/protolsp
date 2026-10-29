// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

// writeFakeBufForLint stages a fake `buf` shell script tuned for
// the analyzeProto lint pass — narrower than the format-side fake
// (it only needs to handle `buf lint`). Branches on
// BUFTOOL_TEST_MODE:
//
//	"lint-clean" — empty stdout, exit 0
//	"lint-findings" — two JSON findings (one pascal-case, one snake-case), exit 100
//	"lint-compile-only" — one COMPILE-typed finding (build error), exit 100
//	"lint-fail" — exit 1 with a stderr message
//
// Identical shape to buftool_test.go's helper; duplicated here so
// the server-package tests don't have to depend on an internal
// _test package.
func writeFakeBufForLint(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-buf.sh")
	const script = `#!/bin/sh
if [ "$1" != "lint" ]; then
  echo "fake-buf: unsupported subcommand: $1" 1>&2
  exit 2
fi
case "${BUFTOOL_TEST_MODE:-lint-clean}" in
  lint-clean) : ;;
  lint-findings)
    cat <<'JSON'
{"path":"/tmp/fake.proto","start_line":1,"start_column":1,"end_line":1,"end_column":1,"type":"PACKAGE_DEFINED","message":"Files must have a package defined."}
{"path":"/tmp/fake.proto","start_line":2,"start_column":9,"end_line":2,"end_column":16,"type":"MESSAGE_PASCAL_CASE","message":"Message name \"badName\" should be PascalCase."}
JSON
    exit 100
    ;;
  lint-compile-only)
    cat <<'JSON'
{"path":"/tmp/fake.proto","start_line":4,"start_column":8,"end_line":4,"end_column":8,"type":"COMPILE","message":"import \"missing/file.proto\": file does not exist"}
JSON
    exit 100
    ;;
  lint-fail) echo "fake-buf: lint blew up" 1>&2; exit 1 ;;
  *) exit 3 ;;
esac
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// publishedProtoDiagnostics pulls the last publishDiagnostics
// notification's diagnostic slice from the fakeConn. Tests use it
// to assert that lint findings have been appended.
func publishedProtoDiagnostics(t *testing.T, fc *fakeConn) []protocol.Diagnostic {
	t.Helper()
	calls := fc.calls()
	require.NotEmpty(t, calls, "expected at least one publishDiagnostics call")
	return calls[len(calls)-1].params.Diagnostics
}

func TestProtoLint_FindingsAppearAsWarnings(t *testing.T) {
	t.Setenv("PROTOLSP_BUF_PATH", writeFakeBufForLint(t))
	t.Setenv("BUFTOOL_TEST_MODE", "lint-findings")

	srv, fc := newServerWithFakeConn(t)
	srv.SetBufLint(true)

	// Source itself is irrelevant — the fake buf binary returns
	// fixed findings regardless. Real source goes through
	// protocompile separately; we just need the document to land
	// on the analyze path.
	const src = `syntax = "proto3";

message badName {
  string FieldName = 1;
}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/lint.proto", 1, src)))

	diags := publishedProtoDiagnostics(t, fc)
	bufFindings := 0
	for _, d := range diags {
		if d.Source == "buf/lint" {
			bufFindings++
			assert.Equal(t, protocol.DiagnosticSeverityWarning, d.Severity,
				"lint findings render as warnings, not errors")
		}
	}
	assert.Equal(t, 2, bufFindings, "both fake findings surface as buf/lint diagnostics")
}

func TestProtoLint_CompileFindingsAreFiltered(t *testing.T) {
	// protocompile already publishes build / import errors with
	// source "protolsp/proto" — adding buf's COMPILE-typed findings
	// would double the gutter noise. The filter lives in
	// appendBufLintDiagnostics.
	t.Setenv("PROTOLSP_BUF_PATH", writeFakeBufForLint(t))
	t.Setenv("BUFTOOL_TEST_MODE", "lint-compile-only")

	srv, fc := newServerWithFakeConn(t)
	srv.SetBufLint(true)

	const src = `syntax = "proto3";
package foo;
import "missing/file.proto";
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/lint.proto", 1, src)))

	diags := publishedProtoDiagnostics(t, fc)
	for _, d := range diags {
		assert.NotEqual(t, "buf/lint", d.Source,
			"COMPILE findings from buf are filtered (protocompile already reports them)")
	}
}

func TestProtoLint_SilentWhenBufMissing(t *testing.T) {
	// Point the env var at nowhere → buftool reports ErrBufNotFound
	// → analyzeProto skips silently (no log churn, no error
	// diagnostic). The protocompile pass still runs as normal.
	t.Setenv("PROTOLSP_BUF_PATH", "/nonexistent/buf-binary")

	srv, fc := newServerWithFakeConn(t)
	srv.SetBufLint(true)

	const src = `syntax = "proto3";
package foo;
message Foo {}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/lint.proto", 1, src)))

	diags := publishedProtoDiagnostics(t, fc)
	for _, d := range diags {
		assert.NotEqual(t, "buf/lint", d.Source,
			"missing buf produces no lint diagnostics")
	}
}

func TestProtoLint_DisabledTogglePreventsInvocation(t *testing.T) {
	// Even with a working fake buf, the toggle off means
	// appendBufLintDiagnostics never runs. Important for keeping
	// existing proto tests insulated.
	t.Setenv("PROTOLSP_BUF_PATH", writeFakeBufForLint(t))
	t.Setenv("BUFTOOL_TEST_MODE", "lint-findings")

	srv, fc := newServerWithFakeConn(t)
	// bufLintEnabled stays false (test helper default).

	const src = `syntax = "proto3";
message badName {}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/lint.proto", 1, src)))

	diags := publishedProtoDiagnostics(t, fc)
	for _, d := range diags {
		assert.NotEqual(t, "buf/lint", d.Source,
			"toggle-off path skips the lint pass entirely")
	}
}
