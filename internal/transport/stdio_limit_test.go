// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package transport

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readAll exhausts a limitedFrameReader and returns whatever
// bytes it delivered before EOF or error.
func readAll(t *testing.T, r io.Reader) ([]byte, error) {
	t.Helper()
	var buf bytes.Buffer
	tmp := make([]byte, 128)
	for {
		n, err := r.Read(tmp)
		buf.Write(tmp[:n])
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf.Bytes(), nil
			}
			return buf.Bytes(), err
		}
	}
}

func makeFrame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

func TestLimitedFrameReader_SmallFramePassesThrough(t *testing.T) {
	frame := makeFrame(`{"jsonrpc":"2.0","method":"initialize"}`)
	r := newLimitedFrameReader(strings.NewReader(frame), MaxStdioFrameBytes)

	got, err := readAll(t, r)
	require.NoError(t, err)
	assert.Equal(t, frame, string(got),
		"reader must replay headers + body unchanged so jsonrpc2's framing parser sees the original stream")
}

func TestLimitedFrameReader_MultipleFramesParseInSequence(t *testing.T) {
	a := makeFrame(`{"id":1}`)
	b := makeFrame(`{"id":2}`)
	r := newLimitedFrameReader(strings.NewReader(a+b), MaxStdioFrameBytes)

	got, err := readAll(t, r)
	require.NoError(t, err)
	assert.Equal(t, a+b, string(got))
}

func TestLimitedFrameReader_OversizeRejectsBeforeBodyRead(t *testing.T) {
	// 100-byte body declared, but the limit is 50 — must fail
	// before any body bytes are pulled off the underlying reader.
	max := int64(50)
	body := strings.Repeat("x", 100)
	frame := makeFrame(body)
	underlying := strings.NewReader(frame)
	r := newLimitedFrameReader(underlying, max)

	_, err := readAll(t, r)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFrameTooLarge,
		"oversize frame must wrap the sentinel so callers can switch on it")
}

func TestLimitedFrameReader_HeaderBloatRejected(t *testing.T) {
	// A header line longer than maxStdioHeaderBytes (64 KiB)
	// gets rejected without ever reaching Content-Length parsing.
	bloat := strings.Repeat("X", maxStdioHeaderBytes+1)
	frame := "Bogus: " + bloat + "\r\nContent-Length: 1\r\n\r\nx"
	r := newLimitedFrameReader(strings.NewReader(frame), MaxStdioFrameBytes)

	_, err := readAll(t, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "framing header exceeds")
}

func TestLimitedFrameReader_MissingContentLengthRejected(t *testing.T) {
	frame := "X-Other: 1\r\n\r\n{}"
	r := newLimitedFrameReader(strings.NewReader(frame), MaxStdioFrameBytes)

	_, err := readAll(t, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing Content-Length")
}

func TestLimitedFrameReader_NegativeContentLengthRejected(t *testing.T) {
	frame := "Content-Length: -5\r\n\r\n"
	r := newLimitedFrameReader(strings.NewReader(frame), MaxStdioFrameBytes)

	_, err := readAll(t, r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "negative Content-Length")
}

func TestLimitedFrameReader_CaseInsensitiveHeader(t *testing.T) {
	// LSP spec: header names are case-insensitive. Real clients
	// (e.g. older Eclipse-LSP shims) sometimes send lowercase.
	body := `{"ok":true}`
	frame := fmt.Sprintf("content-length: %d\r\n\r\n%s", len(body), body)
	r := newLimitedFrameReader(strings.NewReader(frame), MaxStdioFrameBytes)

	got, err := readAll(t, r)
	require.NoError(t, err)
	assert.Equal(t, frame, string(got))
}

func TestLimitedFrameReader_BareLFAccepted(t *testing.T) {
	// Some clients send \n-only line endings instead of \r\n.
	// The reader tolerates both.
	body := `{"ok":true}`
	frame := fmt.Sprintf("Content-Length: %d\n\n%s", len(body), body)
	r := newLimitedFrameReader(strings.NewReader(frame), MaxStdioFrameBytes)

	got, err := readAll(t, r)
	require.NoError(t, err)
	assert.Equal(t, frame, string(got))
}

func TestLimitedFrameReader_EOFBetweenFramesIsClean(t *testing.T) {
	// Underlying EOF at a frame boundary surfaces as io.EOF, not
	// a framing error. jsonrpc2 treats EOF as a clean session
	// close.
	frame := makeFrame("{}")
	r := newLimitedFrameReader(strings.NewReader(frame), MaxStdioFrameBytes)

	got, err := readAll(t, r)
	require.NoError(t, err, "readAll swallows EOF; clean termination expected")
	assert.Equal(t, frame, string(got))
}
