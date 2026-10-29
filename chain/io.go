// Disk I/O helpers for chain.pxf editing. Mirrors the
// workspaceconfig io.go pattern (LoadDocument + atomic
// WriteDocument). Kept separate from chain.go so the read-only
// parsers there don't pull os/io into their import surface, and
// distinct from workspaceconfig/io.go because the file-name
// pattern and load semantics (no synthesized default on absent
// file — chain.pxf is user-authored) are different.

package chain

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// LoadDocument reads chain.pxf at path and returns the parsed AST
// for in-place editing. Unlike workspaceconfig.LoadDocument,
// missing-file is an error here — chain.pxf is user-authored, so
// "missing" is a configuration mistake, not a fresh-workspace
// state.
func LoadDocument(path string) (*pxf.Document, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path comes from the workspace's resolved chain spec location, not user input
	if err != nil {
		return nil, fmt.Errorf("reading chain spec %s: %w", path, err)
	}
	doc, err := pxf.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing chain spec %s: %w", path, err)
	}
	return doc, nil
}

// WriteDocument atomically writes doc to path, formatted via
// pxf.FormatDocument. Same temp-then-rename pattern as
// workspaceconfig.WriteDocument — see there for the rationale on
// atomicity vs. the watcher race. Inherits the existing file's
// mode when present; new files land at 0644 (committed-config
// convention).
func WriteDocument(path string, doc *pxf.Document) (err error) {
	formatted := pxf.FormatDocument(doc)

	mode := os.FileMode(0o644)
	if st, statErr := os.Stat(path); statErr == nil {
		mode = st.Mode().Perm()
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".chain.pxf.*.tmp")
	if err != nil {
		return fmt.Errorf("creating tempfile in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()

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
