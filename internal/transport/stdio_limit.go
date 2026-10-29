// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package transport

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MaxStdioFrameBytes caps the per-frame size of an inbound
// LSP-over-stdio message. Matches the websocket transport's
// default (4 MiB). A hostile client that sends a frame with
// Content-Length above this gets cut off BEFORE jsonrpc2's
// framing parser allocates a buffer of that size — closes a
// trivial-but-real DoS vector that the websocket transport
// already protects against via coder/websocket.SetReadLimit.
const MaxStdioFrameBytes int64 = 4 * 1024 * 1024

// maxStdioHeaderBytes caps the LSP framing-header section
// (everything before the empty CRLF line that introduces the
// JSON body). Real LSP headers are ~50-100 bytes; a client
// streaming arbitrarily-long header lines would otherwise force
// us to grow the header buffer indefinitely while we look for
// the terminator.
const maxStdioHeaderBytes = 64 * 1024

// ErrFrameTooLarge is returned by limitedFrameReader.Read when
// a frame's Content-Length exceeds MaxStdioFrameBytes. The
// error propagates up through jsonrpc2 and terminates the
// session — the client's framing is by definition wrong (or
// malicious), so there's no clean recovery.
var ErrFrameTooLarge = errors.New("stdio frame exceeds size limit")

// limitedFrameReader sits between os.Stdin and the jsonrpc2
// framing parser. It re-reads the Content-Length header out of
// each frame, validates it against `max`, then passes the
// header + body bytes through unmodified so jsonrpc2 sees the
// stream it expects.
//
// State machine:
//   - At frame start, accumulate header bytes (line-at-a-time)
//     into headerBuf, looking for the empty CRLF that ends the
//     header section.
//   - Parse Content-Length; reject frames over the limit.
//   - Flush the buffered headers back to the caller, then enter
//     body pass-through (with bodyRemaining acting as a hard
//     cap on how many bytes the caller can read before the
//     next frame's headers).
//   - When bodyRemaining drains to zero, loop back to step 1.
type limitedFrameReader struct {
	br            *bufio.Reader
	max           int64
	pending       []byte
	bodyRemaining int64
}

func newLimitedFrameReader(r io.Reader, max int64) *limitedFrameReader {
	return &limitedFrameReader{
		br:  bufio.NewReader(r),
		max: max,
	}
}

func (l *limitedFrameReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// 1) Flush any header bytes we've buffered after parsing.
	if len(l.pending) > 0 {
		n := copy(p, l.pending)
		l.pending = l.pending[n:]
		return n, nil
	}
	// 2) Body pass-through, capped at bodyRemaining.
	if l.bodyRemaining > 0 {
		toRead := int64(len(p))
		if toRead > l.bodyRemaining {
			toRead = l.bodyRemaining
		}
		n, err := l.br.Read(p[:toRead])
		l.bodyRemaining -= int64(n)
		return n, err
	}
	// 3) Frame boundary — read + validate the next frame's headers.
	headers, contentLen, err := readAndValidateFrameHeaders(l.br, l.max)
	if err != nil {
		return 0, err
	}
	l.pending = headers
	l.bodyRemaining = contentLen
	// Recurse to enter step 1.
	return l.Read(p)
}

// readAndValidateFrameHeaders reads one LSP header section
// terminated by a blank CRLF line, parses Content-Length, and
// returns the raw header bytes (for replay to jsonrpc2) along
// with the validated content length. Returns ErrFrameTooLarge
// when Content-Length exceeds max.
func readAndValidateFrameHeaders(br *bufio.Reader, max int64) ([]byte, int64, error) {
	var buf bytes.Buffer
	for {
		if buf.Len() > maxStdioHeaderBytes {
			return nil, 0, fmt.Errorf("stdio: framing header exceeds %d bytes", maxStdioHeaderBytes)
		}
		line, err := br.ReadString('\n')
		buf.WriteString(line)
		if err != nil {
			// io.EOF before any header bytes is a clean stream
			// close (the client disconnected at a frame boundary);
			// pass it up so jsonrpc2 reports a clean done. EOF
			// mid-header is a malformed frame; pass it up too.
			return nil, 0, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	contentLen, err := parseContentLength(buf.Bytes())
	if err != nil {
		return nil, 0, fmt.Errorf("stdio: %w", err)
	}
	if contentLen < 0 {
		return nil, 0, fmt.Errorf("stdio: negative Content-Length: %d", contentLen)
	}
	if contentLen > max {
		return nil, 0, fmt.Errorf("%w: Content-Length=%d, max=%d", ErrFrameTooLarge, contentLen, max)
	}
	return buf.Bytes(), contentLen, nil
}

// parseContentLength scans LSP framing headers for
// `Content-Length: NNN`. Header names are case-insensitive per
// the LSP spec; the value is a decimal integer (no leading +).
func parseContentLength(headers []byte) (int64, error) {
	// LSP framing uses \r\n line separators per the spec; some
	// clients send \n only. Split on \n and trim \r.
	for _, raw := range strings.Split(string(headers), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(line[:colon]), "content-length") {
			continue
		}
		v := strings.TrimSpace(line[colon+1:])
		return strconv.ParseInt(v, 10, 64)
	}
	return 0, errors.New("missing Content-Length header")
}
