// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package safeio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadFileLimit_SmallFileReturnsContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.pxf")
	body := []byte("@type c4test.v1.Anything\nfield = \"x\"\n")
	require.NoError(t, os.WriteFile(path, body, 0o600))

	got, err := ReadFileLimit(path, 1024)
	require.NoError(t, err)
	assert.Equal(t, body, got)
}

func TestReadFileLimit_OversizeReturnsErrFileTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.pxf")
	// Write 1 KiB but allow only 512 bytes. The limit+1 read
	// pattern detects oversize without an os.Stat call.
	body := make([]byte, 1024)
	require.NoError(t, os.WriteFile(path, body, 0o600))

	_, err := ReadFileLimit(path, 512)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFileTooLarge,
		"oversize must wrap the sentinel so callers can switch on it")
}

func TestReadFileLimit_MissingFileSurfacesIOError(t *testing.T) {
	// Non-existent files surface the os.Open error verbatim so
	// callers can distinguish "not present" (soft fail) from
	// "too large" (refusal).
	_, err := ReadFileLimit(filepath.Join(t.TempDir(), "absent"), 1024)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrFileTooLarge),
		"missing file is not an oversize error")
}

func TestReadFileLimit_ExactLimitFits(t *testing.T) {
	// A file at exactly `limit` bytes must pass — the +1 read
	// pattern only rejects strictly larger files.
	dir := t.TempDir()
	path := filepath.Join(dir, "edge.pxf")
	body := make([]byte, 256)
	require.NoError(t, os.WriteFile(path, body, 0o600))

	got, err := ReadFileLimit(path, 256)
	require.NoError(t, err)
	assert.Len(t, got, 256)
}
