// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package safeio holds bounded I/O helpers protolsp uses where a
// hostile or accidentally-huge file shouldn't be able to take down
// the LSP. Config-tier reads (.protolsp.pxf, chain.pxf, per-env
// chain layers) all flow through ReadFileLimit so a multi-GB file
// or /dev/zero symlink fails fast instead of OOM-ing the process.

package safeio

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// MaxConfigFileBytes is the per-file ceiling protolsp uses for
// config / chain / layer reads. 4 MiB is several orders of
// magnitude above any realistic `.protolsp.pxf` / `chain.pxf` /
// PXF chain layer (the largest real-world layers we've seen are
// in the tens of KiB) while staying well below process-OOM
// territory. A hostile workspace that points a chain layer at a
// huge file (or a symlink to /dev/zero) gets clipped here.
const MaxConfigFileBytes = 4 * 1024 * 1024

// ErrFileTooLarge is returned by ReadFileLimit when the file
// exceeds the requested ceiling. Callers should treat it the
// same as a read failure (log + degrade) — the file is still on
// disk, but protolsp refuses to load that much config into
// memory.
var ErrFileTooLarge = errors.New("file exceeds size limit")

// ReadFileLimit reads up to limit bytes from path. Returns the
// content (no extra copy) on success. On oversize, returns an
// error wrapping ErrFileTooLarge and the partial buffer is
// discarded — callers shouldn't be tempted to use a truncated
// config file.
//
// Pattern: read limit+1 bytes through io.LimitReader. If the
// returned slice is longer than limit, the file had more to
// give and we reject. This avoids needing an os.Stat round-
// trip (which races with size changes and doesn't catch
// growing pipes / device files anyway).
func ReadFileLimit(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 — caller composes path from controlled inputs
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only handle; close error is uninteresting
	buf, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrFileTooLarge, path, limit)
	}
	return buf, nil
}
