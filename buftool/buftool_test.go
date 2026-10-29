// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package buftool

import (
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeFakeBuf stages a shell script at <dir>/fake-buf.sh that
// mimics the slice of `buf` behavior the production code invokes.
// Behavior switches on the BUFTOOL_TEST_MODE env var so each test
// can pin a scenario without writing a separate script:
//
//	"format-uppercase" — emit the input file's contents upper-cased to stdout
//	"format-passthrough" — emit the input file's contents verbatim
//	"format-fail" — exit 1 with a stderr message
//	"format-slow" — sleep 30s before passthrough (triggers ctx.DeadlineExceeded)
//	"lint-clean" — emit nothing, exit 0
//	"lint-findings" — emit two JSON findings to stdout, exit 100
//	"lint-compile" — emit a COMPILE-typed finding (build error), exit 100
//	"lint-fail" — exit 1 with a stderr message, no stdout
//
// Returns the absolute path; tests point PROTOLSP_BUF_PATH at it.
func writeFakeBuf(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-binary tests require a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-buf.sh")
	const script = `#!/bin/sh
# fake buf — supports just enough of the surface to exercise buftool.
sub="$1"
shift
case "$sub" in
  format)
    file="$1"
    case "${BUFTOOL_TEST_MODE:-format-passthrough}" in
      format-uppercase)
        tr '[:lower:]' '[:upper:]' < "$file"
        ;;
      format-passthrough)
        cat "$file"
        ;;
      format-fail)
        echo "fake-buf: simulated failure" 1>&2
        exit 1
        ;;
      format-slow)
        sleep 30
        cat "$file"
        ;;
      *)
        echo "fake-buf: unknown format mode" 1>&2
        exit 3
        ;;
    esac
    ;;
  lint)
    # Args are "--error-format=json <file>". We ignore the
    # specific args here — the test mode picks behavior.
    case "${BUFTOOL_TEST_MODE:-lint-clean}" in
      lint-clean)
        # Clean lint → no stdout, exit 0.
        :
        ;;
      lint-findings)
        cat <<'JSON'
{"path":"/tmp/fake.proto","start_line":2,"start_column":9,"end_line":2,"end_column":16,"type":"MESSAGE_PASCAL_CASE","message":"Message name \"badName\" should be PascalCase."}
{"path":"/tmp/fake.proto","start_line":3,"start_column":10,"end_line":3,"end_column":19,"type":"FIELD_LOWER_SNAKE_CASE","message":"Field name \"FieldName\" should be lower_snake_case."}
JSON
        exit 100
        ;;
      lint-compile)
        cat <<'JSON'
{"path":"/tmp/fake.proto","start_line":4,"start_column":8,"end_line":4,"end_column":8,"type":"COMPILE","message":"import \"missing/file.proto\": file does not exist"}
JSON
        exit 100
        ;;
      lint-fail)
        echo "fake-buf: lint blew up" 1>&2
        exit 1
        ;;
      *)
        echo "fake-buf: unknown lint mode" 1>&2
        exit 3
        ;;
    esac
    ;;
  *)
    echo "fake-buf: unsupported subcommand: $sub" 1>&2
    exit 2
    ;;
esac
`
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

func TestResolve_PrefersEnvVar(t *testing.T) {
	const fake = "/tmp/this-path-doesnt-need-to-exist"
	t.Setenv(envVarName, fake)
	got, err := Resolve()
	require.NoError(t, err)
	assert.Equal(t, fake, got,
		"env var wins even when the file doesn't exist on disk — exec invocation surfaces missing-file at call time")
}

func TestResolve_FallsBackToPathLookup(t *testing.T) {
	// Clear the override + an empty PATH so LookPath has nothing to
	// find. We only assert "returns ErrBufNotFound" — whether a host
	// system has buf installed is irrelevant under an empty PATH.
	t.Setenv(envVarName, "")
	t.Setenv("PATH", "")
	_, err := Resolve()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBufNotFound),
		"missing-buf surfaces via ErrBufNotFound sentinel")
}

func TestFormat_PassthroughViaFakeBinary(t *testing.T) {
	fake := writeFakeBuf(t)
	t.Setenv(envVarName, fake)
	t.Setenv("BUFTOOL_TEST_MODE", "format-passthrough")

	src := []byte(`syntax = "proto3"; package foo;`)
	got, err := Format(context.Background(), src)
	require.NoError(t, err)
	assert.Equal(t, string(src), string(got),
		"passthrough mode confirms the temp-file staging + stdout capture round-trip")
}

func TestFormat_TransformViaFakeBinary(t *testing.T) {
	fake := writeFakeBuf(t)
	t.Setenv(envVarName, fake)
	t.Setenv("BUFTOOL_TEST_MODE", "format-uppercase")

	src := []byte("hello\n")
	got, err := Format(context.Background(), src)
	require.NoError(t, err)
	assert.Equal(t, "HELLO\n", string(got),
		"fake-binary transformation lands in the captured output")
}

func TestFormat_ReturnsErrBufNotFoundWhenMissing(t *testing.T) {
	t.Setenv(envVarName, "")
	t.Setenv("PATH", "")
	_, err := Format(context.Background(), []byte("syntax = \"proto3\";"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBufNotFound))
}

// TestFormat_RespectsContextDeadline covers the audit gap: a
// caller-supplied short deadline must surface as a context error
// from Format rather than blocking the LSP dispatcher for the full
// 30s a real-but-stuck buf could take. The fake-buf "format-slow"
// mode sleeps 30s; we hand it a 100ms context and assert the call
// returns within a tight window with a context error in the chain.
func TestFormat_RespectsContextDeadline(t *testing.T) {
	fake := writeFakeBuf(t)
	t.Setenv(envVarName, fake)
	t.Setenv("BUFTOOL_TEST_MODE", "format-slow")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Format(ctx, []byte("syntax = \"proto3\";"))
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 3*time.Second,
		"Format must honor the caller's context deadline; took %s", elapsed)
	// The error chain should include context.DeadlineExceeded so
	// callers can distinguish "buf hung" from "buf returned a real
	// failure". The wrapper text may be anything; what matters is
	// the underlying cause survives errors.Is.
	assert.True(t,
		errors.Is(err, context.DeadlineExceeded) ||
			strings.Contains(err.Error(), "deadline") ||
			strings.Contains(err.Error(), "signal: killed"),
		"deadline-exceeded path must surface a context error or kill-signal: %v", err)
}

func TestFormat_PropagatesBufFailure(t *testing.T) {
	fake := writeFakeBuf(t)
	t.Setenv(envVarName, fake)
	t.Setenv("BUFTOOL_TEST_MODE", "format-fail")

	_, err := Format(context.Background(), []byte("anything"))
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrBufNotFound),
		"a buf-side failure is NOT ErrBufNotFound — callers distinguish missing-binary from runtime errors")
	assert.True(t, strings.Contains(err.Error(), "simulated failure"),
		"stderr is folded into the error so callers can log it: %v", err)
}

func TestLint_CleanReturnsNoFindings(t *testing.T) {
	fake := writeFakeBuf(t)
	t.Setenv(envVarName, fake)
	t.Setenv("BUFTOOL_TEST_MODE", "lint-clean")

	findings, err := Lint(context.Background(), []byte("syntax = \"proto3\";"), "")
	require.NoError(t, err)
	assert.Empty(t, findings,
		"exit 0 + empty stdout → no findings, not a parse error")
}

func TestLint_ParsesJSONFindings(t *testing.T) {
	fake := writeFakeBuf(t)
	t.Setenv(envVarName, fake)
	t.Setenv("BUFTOOL_TEST_MODE", "lint-findings")

	findings, err := Lint(context.Background(), []byte("anything"), "")
	require.NoError(t, err,
		"exit 100 with parseable JSON is a successful invocation (findings exist)")
	require.Len(t, findings, 2)
	// First finding: message-name pascal case
	assert.Equal(t, "MESSAGE_PASCAL_CASE", findings[0].Type)
	assert.Equal(t, 2, findings[0].StartLine)
	assert.Equal(t, 9, findings[0].StartColumn)
	assert.Equal(t, 2, findings[0].EndLine)
	assert.Equal(t, 16, findings[0].EndColumn)
	assert.Contains(t, findings[0].Message, "badName")
	// Second: field-name snake case
	assert.Equal(t, "FIELD_LOWER_SNAKE_CASE", findings[1].Type)
	assert.Equal(t, 3, findings[1].StartLine)
}

func TestLint_CompileTypeIsPreservedForCallerToFilter(t *testing.T) {
	// COMPILE findings (build errors) are part of the same JSON
	// stream. buftool keeps them so the server-side handler can
	// decide to drop them (protocompile already publishes the
	// same diagnostics for .proto docs) — better than baking
	// filter logic into buftool itself.
	fake := writeFakeBuf(t)
	t.Setenv(envVarName, fake)
	t.Setenv("BUFTOOL_TEST_MODE", "lint-compile")

	findings, err := Lint(context.Background(), []byte("anything"), "")
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, "COMPILE", findings[0].Type)
}

func TestLint_ReturnsErrBufNotFoundWhenMissing(t *testing.T) {
	t.Setenv(envVarName, "")
	t.Setenv("PATH", "")
	_, err := Lint(context.Background(), []byte("syntax = \"proto3\";"), "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBufNotFound))
}

func TestLint_PropagatesToolFailure(t *testing.T) {
	// Tool failure (non-0, non-100 exit + no JSON on stdout) is a
	// real error — distinct from "lint found things." Caller logs
	// + degrades; the trace panel shows the stderr.
	fake := writeFakeBuf(t)
	t.Setenv(envVarName, fake)
	t.Setenv("BUFTOOL_TEST_MODE", "lint-fail")

	_, err := Lint(context.Background(), []byte("anything"), "")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrBufNotFound))
	assert.Contains(t, err.Error(), "lint blew up")
}

func TestParseLintJSON_HandlesEmptyAndPartialInput(t *testing.T) {
	// Pure unit test on the parser — empty input, whitespace-only,
	// and a mid-stream garbage line.
	got, err := parseLintJSON(nil)
	require.NoError(t, err)
	assert.Empty(t, got)

	got, err = parseLintJSON([]byte("\n\n  \n"))
	require.NoError(t, err)
	assert.Empty(t, got)

	// Valid + garbage: partial result returned alongside the error
	// so the caller can decide whether the partial is useful.
	mixed := []byte(`{"path":"a","start_line":1,"start_column":1,"end_line":1,"end_column":1,"type":"T","message":"m"}` + "\nnot-json-at-all\n")
	got, err = parseLintJSON(mixed)
	require.Error(t, err)
	assert.Len(t, got, 1, "first line parses; the garbage short-circuits with the partial slice")
}

// writeRecordingBuf stages a fake buf that records the .proto files
// it can see under its input directory (the last argument) into
// recordFile, then exits clean. Lets tests assert WHERE Lint staged
// the buffer without racing the temp-dir cleanup.
func writeRecordingBuf(t *testing.T, recordFile string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake-buf shell script requires a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "recording-buf.sh")
	script := "#!/bin/sh\nfind \"$3\" -name '*.proto' > " + recordFile + "\nexit 0\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	return path
}

// The buffer stages under its include-path-relative location so
// directory- and filename-based lint rules (PACKAGE_DIRECTORY_MATCH,
// FILE_LOWER_SNAKE_CASE) judge the file where it actually lives.
func TestLint_StagesUnderRelPath(t *testing.T) {
	record := filepath.Join(t.TempDir(), "seen.txt")
	t.Setenv(envVarName, writeRecordingBuf(t, record))

	_, err := Lint(context.Background(), []byte("syntax = \"proto3\";"), "myapp/v1/greeting.proto")
	require.NoError(t, err)

	seen, err := os.ReadFile(record)
	require.NoError(t, err)
	assert.Contains(t, string(seen), filepath.Join("myapp", "v1", "greeting.proto"),
		"buffer must stage at its include-path-relative location")
}

// A relPath that escapes the staging root (or an empty one) falls
// back to the historical bare input.proto — never writes outside the
// temp module root.
func TestLint_NonLocalRelPathFallsBack(t *testing.T) {
	for _, rel := range []string{"", "../evil.proto", "/abs/evil.proto"} {
		record := filepath.Join(t.TempDir(), "seen.txt")
		t.Setenv(envVarName, writeRecordingBuf(t, record))

		_, err := Lint(context.Background(), []byte("syntax = \"proto3\";"), rel)
		require.NoError(t, err, "relPath %q", rel)

		seen, err := os.ReadFile(record)
		require.NoError(t, err)
		assert.Contains(t, string(seen), "input.proto", "relPath %q", rel)
		assert.NotContains(t, string(seen), "evil.proto", "relPath %q", rel)
	}
}

// Integration guard against the staging regression with a real buf
// (skips when buf isn't installed): a packaged file staged at its
// relative path must NOT trigger PACKAGE_DIRECTORY_MATCH.
func TestLint_RealBufNoDirectoryMismatch(t *testing.T) {
	t.Setenv(envVarName, "")
	if _, err := osexec.LookPath("buf"); err != nil {
		t.Skip("buf not on PATH")
	}
	src := []byte("syntax = \"proto3\";\npackage myapp.v1;\n\nmessage Greeting {\n  string name = 1;\n}\n")
	findings, err := Lint(context.Background(), src, "myapp/v1/greeting.proto")
	require.NoError(t, err)
	for _, f := range findings {
		assert.NotEqual(t, "PACKAGE_DIRECTORY_MATCH", f.Type,
			"staging under the relative path must satisfy directory-based rules: %+v", f)
	}
}
