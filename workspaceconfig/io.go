// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package workspaceconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// LoadDocument reads .protolsp.pxf at path and returns the parsed
// AST. Used by the configSet path: SetAttribute mutates the AST in
// place, so we need the structural tree (not the consumption-shape
// Config struct that Parse returns).
//
// A nonexistent file returns an empty Document seeded with the
// expected @type directive. That lets the very first Set on a
// fresh workspace create the file from scratch without callers
// special-casing the no-file case.
func LoadDocument(path string) (*pxf.Document, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is a workspace .protolsp.pxf chosen by the LSP, not user input
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &pxf.Document{TypeURL: ExpectedType}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	doc, err := pxf.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if doc.TypeURL == "" {
		// File exists but the @type directive is missing — Parse
		// already surfaces a warning. We default the TypeURL so
		// the round-tripped output is well-formed.
		doc.TypeURL = ExpectedType
	}
	return doc, nil
}

// WriteDocument atomically writes doc to path, formatted via
// pxf.FormatDocument so the output stays human-readable and any
// surviving leading/trailing comments on AST nodes are emitted.
//
// Atomic in the tempfile-then-rename sense: file watchers and
// concurrent readers always see either the prior contents or the
// new contents, never a half-written file. The tempfile sits in
// the same directory as the target so the rename is on one
// filesystem (cross-fs renames fall back to copy+delete and lose
// atomicity).
//
// Mode preservation: when path exists, the new file inherits its
// mode. New files land at 0644 — committed config convention.
func WriteDocument(path string, doc *pxf.Document) (err error) {
	formatted := pxf.FormatDocument(doc)

	mode := os.FileMode(0o644)
	if st, statErr := os.Stat(path); statErr == nil {
		mode = st.Mode().Perm()
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".protolsp.pxf.*.tmp")
	if err != nil {
		return fmt.Errorf("creating tempfile in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

	// On any failure after CreateTemp, remove the half-written
	// tempfile. Cleared once Rename succeeds.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(formatted); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing tempfile: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming tempfile onto %s: %w", path, err)
	}
	cleanup = false
	return nil
}
