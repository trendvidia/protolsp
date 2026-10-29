// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package buftool wraps the external `buf` CLI for the proto-authoring
// phase 5 LSP features (format, lint). It's the first time protolsp
// shells out to a sibling binary, so this package establishes the
// resolution + invocation pattern other future shells can mirror.
//
// Resolution order:
//
//  1. PROTOLSP_BUF_PATH env var — explicit override, no further
//     lookup. Lets pxfed (or tests) pin a specific binary without
//     mutating PATH.
//  2. exec.LookPath("buf") — the conventional install path. Fails
//     cleanly when buf isn't installed.
//
// Missing-buf isn't an error in this package's vocabulary; the
// caller decides whether to surface a warning. Format / Lint return
// distinct sentinel errors (ErrBufNotFound) so handlers can degrade
// quietly when the binary isn't installed instead of poisoning every
// formatRequest with "binary missing" diagnostics.
package buftool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// killGraceWindow bounds how long Run waits for pipes to close
// after the context cancels. Without it, an orphaned subprocess
// (e.g. a shell whose child inherited stdout) can hold the pipe
// open long after the parent shell dies, leaving cmd.Run blocked
// well past the caller's deadline.
const killGraceWindow = 500 * time.Millisecond

// envVarName is the override env-var consumed by Resolve. Tests
// point this at a fake shell script; production deployments
// generally leave it unset.
const envVarName = "PROTOLSP_BUF_PATH"

// ErrBufNotFound is returned when neither PROTOLSP_BUF_PATH nor a
// PATH lookup turns up a usable buf binary. Callers check for this
// via errors.Is and typically degrade to "feature unavailable"
// rather than surfacing as a hard error.
var ErrBufNotFound = errors.New("buf binary not found")

// Resolve picks a buf binary path, preferring the env-var override.
// Returns ErrBufNotFound when nothing usable is found.
//
// The env-var path is honored even when it doesn't exist on disk —
// that's a test affordance (the fake-binary tests can stage a
// missing-binary scenario by pointing the var at a deleted file).
// Callers see the resulting exec error at invocation time, with the
// path embedded in the message for diagnostics.
func Resolve() (string, error) {
	if envPath := os.Getenv(envVarName); envPath != "" {
		return envPath, nil
	}
	path, err := exec.LookPath("buf")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBufNotFound, err)
	}
	return path, nil
}

// Format runs `buf format` against src and returns the formatted
// output. buf format reads from a file path (no stdin support as of
// 1.45), so we stage src in a temp file with the .proto suffix it
// expects. The temp file is removed before returning regardless of
// outcome.
//
// Returns ErrBufNotFound when the binary couldn't be resolved.
// Returns an error wrapping the exec.ExitError when buf itself
// errors (e.g. unparseable input); callers typically log + skip
// rather than treat that as fatal.
func Format(ctx context.Context, src []byte) ([]byte, error) {
	path, err := Resolve()
	if err != nil {
		return nil, err
	}
	tmpDir, err := os.MkdirTemp("", "protolsp-buf-format-")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir for buf format: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	tmpFile := filepath.Join(tmpDir, "input.proto")
	if err := os.WriteFile(tmpFile, src, 0o600); err != nil {
		return nil, fmt.Errorf("staging input for buf format: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, "format", tmpFile)
	cmd.WaitDelay = killGraceWindow
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("buf format failed: %w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// LintFinding is one buf-lint result line decoded from buf's
// `--error-format=json` output. Positions are 1-indexed (buf's
// convention); callers convert to LSP's 0-indexed Range when
// publishing as a Diagnostic.
//
// Type == "COMPILE" marks a build failure rather than a style
// finding — protocompile already publishes those for `.proto`
// docs, so the server-side handler filters them out before
// producing Diagnostics to avoid duplicates.
type LintFinding struct {
	Path        string `json:"path"`
	StartLine   int    `json:"start_line"`
	StartColumn int    `json:"start_column"`
	EndLine     int    `json:"end_line"`
	EndColumn   int    `json:"end_column"`
	Type        string `json:"type"`
	Message     string `json:"message"`
}

// Lint runs `buf lint --error-format=json` against src and returns
// the parsed findings. Same staging pattern as Format — src lands
// in a temp file because buf lint doesn't read from stdin. relPath
// is the document's include-path-relative location (forward-slash,
// e.g. "myapp/v1/greeting.proto"); the buffer is staged at that path
// under a temp module root and the root is linted, so directory- and
// filename-based rules (PACKAGE_DIRECTORY_MATCH, FILE_LOWER_SNAKE_CASE)
// judge the file where it actually lives instead of flagging every
// packaged file staged as a bare input.proto (#195). An empty or
// non-local relPath falls back to that bare staging.
//
// Exit-code semantics for buf lint:
//
//	0   → no findings (clean lint)
//	100 → findings present (still a successful invocation)
//	other → tool failure (bad args, IO error, etc.)
//
// We parse JSON regardless of 0 vs 100 — both are "buf ran
// successfully and reported its result." A non-0/100 exit code
// surfaces as an error only when parsing produced no findings,
// since some real failures (e.g. malformed args) emit stderr
// without structured stdout.
//
// Returns ErrBufNotFound when the binary couldn't be resolved.
func Lint(ctx context.Context, src []byte, relPath string) ([]LintFinding, error) {
	path, err := Resolve()
	if err != nil {
		return nil, err
	}
	tmpDir, err := os.MkdirTemp("", "protolsp-buf-lint-")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir for buf lint: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	rel := filepath.FromSlash(relPath)
	if rel == "" || !filepath.IsLocal(rel) {
		rel = "input.proto"
	}
	tmpFile := filepath.Join(tmpDir, rel)
	if err := os.MkdirAll(filepath.Dir(tmpFile), 0o700); err != nil {
		return nil, fmt.Errorf("staging input for buf lint: %w", err)
	}
	if err := os.WriteFile(tmpFile, src, 0o600); err != nil {
		return nil, fmt.Errorf("staging input for buf lint: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, "lint", "--error-format=json", tmpDir)
	cmd.WaitDelay = killGraceWindow
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	findings, parseErr := parseLintJSON(stdout.Bytes())
	// Successful invocation: either exit 0 with no findings, or any
	// exit with parseable JSON findings on stdout. The exit-100
	// "findings exist" case lives in the second arm.
	if parseErr == nil && (len(findings) > 0 || runErr == nil) {
		return findings, nil
	}
	if runErr != nil {
		// Non-zero exit with no parseable findings → real tool
		// failure (bad args, IO error, etc.). stderr carries the
		// most useful hint.
		return nil, fmt.Errorf("buf lint failed: %w: %s", runErr, stderr.String())
	}
	// Exit 0 but stdout didn't parse — partial result is what we
	// have; let the caller decide.
	return findings, parseErr
}

// parseLintJSON walks the stdout output one line at a time. Each
// non-empty line is expected to be a JSON-encoded LintFinding; any
// parse failure short-circuits with the partial slice + the error,
// letting the caller decide whether the partial result is useful
// based on the invocation's exit code.
func parseLintJSON(out []byte) ([]LintFinding, error) {
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var findings []LintFinding
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var f LintFinding
		if err := json.Unmarshal(line, &f); err != nil {
			return findings, fmt.Errorf("parsing buf lint output: %w", err)
		}
		findings = append(findings, f)
	}
	return findings, nil
}
