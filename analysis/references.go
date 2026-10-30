// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/workspaceconfig"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// defaultMaxReferencesWorkspaceFiles caps how many .pxf files
// collectPathHitsAcrossWorkspace will read off disk for a single
// references request. Past this point we return what we have plus
// a window/showMessage warning so the user knows the answer is
// partial. The cap exists to keep a "Find References" click on a
// 100k-file monorepo from pinning the LSP read goroutine for
// minutes.
//
// Picked at 10k: large enough that every realistic project sits
// well under it (the audit's test workspace has dozens of .pxf;
// the largest in-house monorepos top out in the low thousands);
// small enough that a serial fallback (~1 ms per file at worst)
// still completes in well under 10s.
//
// Tests override via Server.maxReferencesFiles — a non-zero value
// substitutes for the default, lets the truncation path be
// exercised without writing 10k files in a tmp dir.
const defaultMaxReferencesWorkspaceFiles = 10000

// referencesFilesCap returns the effective cap for the current
// server, falling back to defaultMaxReferencesWorkspaceFiles when
// the override is zero. Centralised so the cap can be made
// config-driven (e.g. .protolsp.pxf knob) without revisiting the
// walk site.
func (s *Workspace) referencesFilesCap() int {
	if s.maxReferencesFiles > 0 {
		return s.maxReferencesFiles
	}
	return defaultMaxReferencesWorkspaceFiles
}

// ReferencesFor resolves the path under the cursor and returns LSP
// Locations for every matching assignment / block in the open-doc
// set sharing the same WorkspaceDir. Returns nil when the cursor
// doesn't sit on a recognisable path target.
func (s *Workspace) ReferencesFor(ctx context.Context, docURI uri.URI, pos protocol.Position, includeDecl bool) []protocol.Location {
	doc := s.docs.Get(docURI)
	if doc == nil {
		return nil
	}
	doc.mu.RLock()
	if doc.Parsed == nil {
		doc.mu.RUnlock()
		return nil
	}
	cursorPath, declRange, _, ok := pathAtPosition(doc.Source, s.positionEncoding, doc.Parsed.Entries, pos)
	workspaceDir := doc.WorkspaceDir()
	doc.mu.RUnlock()
	if !ok || cursorPath == "" {
		return nil
	}

	hits := s.collectPathHitsAcrossWorkspace(ctx, workspaceDir, cursorPath)
	if len(hits) == 0 {
		return nil
	}

	// IncludeDeclaration controls whether the cursor's own
	// occurrence is reported. Most editors set it true; the few
	// that don't expect to see "other references only," so we
	// honor the flag rather than always echoing the declaration.
	out := make([]protocol.Location, 0, len(hits))
	for _, h := range hits {
		if !includeDecl && rangeEqual(h.rang, declRange) && h.uri == docURI {
			continue
		}
		out = append(out, protocol.Location{
			URI:   uri.URI(h.uri),
			Range: h.rang,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// referencesCandidate bundles a file's URI + on-disk path so the
// worker pool can resolve the open-doc shadow lookup AND fall
// through to the disk read without the walk callback having to
// re-derive either.
type referencesCandidate struct {
	uri  uri.URI
	path string
}

// collectPathHitsAcrossWorkspace scans every .pxf file under
// scopeDir on disk plus any open documents in the same workspace
// and collects path hits matching target. Open docs take
// precedence over their on-disk version so unsaved edits are
// reflected. Sub-workspaces (nested `.protolsp.pxf` ancestors)
// stop the descent, and hidden directories are skipped.
//
// scopeDir == "" falls back to the open-document-only walk so a
// doc with no workspace context (no `.protolsp.pxf` ancestor and
// no LSP rootURI) still gets a useful answer rather than an empty
// list.
//
// Best-effort I/O: a file that can't be read or doesn't parse is
// skipped without halting the search. Partial results are
// preferable to a hard error for a navigation feature; per-file
// failures log at Debug.
//
// Past maxReferencesWorkspaceFiles candidates, the walk truncates
// and a window/showMessage warning fires so the user knows the
// result is partial. Without the cap a Find-References on a
// 100k-file monorepo would pin the LSP read goroutine until
// completion.
func (s *Workspace) collectPathHitsAcrossWorkspace(ctx context.Context, scopeDir, target string) []pathHit {
	if scopeDir == "" {
		return s.collectPathHitsAcrossDocs("", target)
	}

	candidates, truncated := s.walkReferencesCandidates(scopeDir)
	hits := s.parallelMatchReferences(candidates, target)

	// Open-doc shadow set: files the user has open whose path
	// isn't on disk yet (e.g. a new tab not saved). Same
	// WorkspaceDir gate as collectPathHitsAcrossDocs.
	visited := make(map[uri.URI]struct{}, len(candidates))
	for _, c := range candidates {
		visited[c.uri] = struct{}{}
	}
	for _, u := range s.docs.uris() {
		if _, seen := visited[u]; seen {
			continue
		}
		doc := s.docs.Get(u)
		if doc == nil {
			continue
		}
		doc.mu.RLock()
		if doc.WorkspaceDir() != scopeDir || doc.Parsed == nil {
			doc.mu.RUnlock()
			continue
		}
		extra := findPathHits(doc.Source, s.positionEncoding, doc.URI, doc.Parsed.Entries, target)
		doc.mu.RUnlock()
		hits = append(hits, extra...)
	}

	if truncated {
		s.notifyReferencesTruncated(ctx, scopeDir)
	}

	return hits
}

// walkReferencesCandidates walks scopeDir collecting every .pxf
// file's (URI, path) pair, stopping once maxReferencesWorkspaceFiles
// candidates are gathered. Returns the candidates plus a flag
// indicating whether the walk was truncated.
//
// Walk errors are logged at Info — a file that vanishes mid-walk
// or has bad permissions doesn't fail the whole search, but the
// log surfaces the issue if a user reports missing hits.
func (s *Workspace) walkReferencesCandidates(scopeDir string) (candidates []referencesCandidate, truncated bool) {
	walkErr := filepath.WalkDir(scopeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			s.log.Info("references: walk error",
				slog.String("path", path),
				slog.String("err", err.Error()))
			return nil
		}
		if d.IsDir() {
			if path == scopeDir {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			// Sub-workspace boundary: anything below has its own
			// references scope. Stop the descent so cross-workspace
			// hits don't leak in.
			if _, statErr := os.Stat(filepath.Join(path, workspaceconfig.ConfigFileName)); statErr == nil {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".pxf" {
			return nil
		}
		if len(candidates) >= s.referencesFilesCap() {
			truncated = true
			return filepath.SkipAll
		}
		candidates = append(candidates, referencesCandidate{
			uri:  uri.File(path),
			path: path,
		})
		return nil
	})
	if walkErr != nil {
		s.log.Info("references: top-level walk error",
			slog.String("scope", scopeDir),
			slog.String("err", walkErr.Error()))
	}
	return candidates, truncated
}

// parallelMatchReferences fans candidates across a worker pool
// sized to min(runtime.NumCPU(), len(candidates)) and collects all
// matching pathHits. Each worker resolves the open-doc shadow
// lookup first (so unsaved edits win over disk) then falls
// through to disk read + parse. Per-file failures are silently
// skipped at Debug log level — partial results beat hard errors.
//
// Worker count is capped at NumCPU rather than 2×NumCPU even
// though the work is I/O-bound: the LSP serves other concurrent
// requests on the same process, and pinning all logical cores to
// one features's parses would starve everything else.
func (s *Workspace) parallelMatchReferences(candidates []referencesCandidate, target string) []pathHit {
	if len(candidates) == 0 {
		return nil
	}
	workers := runtime.NumCPU()
	if workers > len(candidates) {
		workers = len(candidates)
	}

	jobs := make(chan referencesCandidate, len(candidates))
	results := make(chan []pathHit, workers)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var local []pathHit
			for c := range jobs {
				local = append(local, s.matchReferencesInFile(c, target)...)
			}
			results <- local
		}()
	}
	for _, c := range candidates {
		jobs <- c
	}
	close(jobs)
	wg.Wait()
	close(results)

	var all []pathHit
	for batch := range results {
		all = append(all, batch...)
	}
	return all
}

// matchReferencesInFile resolves one candidate to its pathHits.
// Open-doc state takes precedence so unsaved edits are reflected;
// otherwise the file is read + parsed from disk.
//
// Read / parse failures log at Debug and return nil — a single
// bad file (transient I/O, broken pxf source mid-edit) shouldn't
// fail the whole references query.
func (s *Workspace) matchReferencesInFile(c referencesCandidate, target string) []pathHit {
	if doc := s.docs.Get(c.uri); doc != nil {
		doc.mu.RLock()
		if doc.Parsed != nil {
			hits := findPathHits(doc.Source, s.positionEncoding, doc.URI, doc.Parsed.Entries, target)
			doc.mu.RUnlock()
			return hits
		}
		doc.mu.RUnlock()
	}
	content, err := os.ReadFile(c.path) // #nosec G304 -- candidate paths are workspace-rooted
	if err != nil {
		s.log.Debug("references: read failed",
			slog.String("path", c.path),
			slog.String("err", err.Error()))
		return nil
	}
	parsed, err := pxf.Parse(content)
	if err != nil {
		s.log.Debug("references: parse failed",
			slog.String("path", c.path),
			slog.String("err", err.Error()))
		return nil
	}
	return findPathHits(content, s.positionEncoding, c.uri, parsed.Entries, target)
}

// notifyReferencesTruncated tells the editor the user's request
// hit the workspace-size cap so the result list is necessarily
// partial. Best-effort — the user still got a partial result,
// the message is a courtesy.
func (s *Workspace) notifyReferencesTruncated(ctx context.Context, scopeDir string) {
	s.pub.ShowMessage(ctx, protocol.MessageTypeWarning, fmt.Sprintf(
		"protolsp: references results truncated — workspace at %q exceeded %d .pxf files. "+
			"Split into sub-workspaces with their own .protolsp.pxf to narrow scope.",
		scopeDir, s.referencesFilesCap()))
}

// collectPathHitsAcrossDocs walks every open document whose
// WorkspaceDir matches scopeDir and collects path hits matching
// target. Each document's lock is held for the duration of its own
// walk only.
func (s *Workspace) collectPathHitsAcrossDocs(scopeDir, target string) []pathHit {
	all := s.docs.uris()
	var out []pathHit
	for _, u := range all {
		d := s.docs.Get(u)
		if d == nil {
			continue
		}
		d.mu.RLock()
		// Same-workspace gate. WorkspaceDir is set at didOpen via
		// FindWorkspaceDirFor; docs that never resolved a
		// workspace dir (no `.protolsp.pxf` ancestor) keep empty
		// strings and match the "no workspace" cohort, which is
		// fine — the root LSP workspace lives in that bucket too.
		if d.WorkspaceDir() != scopeDir {
			d.mu.RUnlock()
			continue
		}
		if d.Parsed == nil {
			d.mu.RUnlock()
			continue
		}
		hits := findPathHits(d.Source, s.positionEncoding, d.URI, d.Parsed.Entries, target)
		d.mu.RUnlock()
		out = append(out, hits...)
	}
	return out
}

func rangeEqual(a, b protocol.Range) bool {
	return a.Start.Line == b.Start.Line &&
		a.Start.Character == b.Start.Character &&
		a.End.Line == b.End.Line &&
		a.End.Character == b.End.Character
}
