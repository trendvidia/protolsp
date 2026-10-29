// Phase 1 of proto authoring (pxfed docs/design/proto-authoring.md):
// surface protocompile parse + semantic errors for any .proto file
// the editor opens. No symbol resolution, no hover, no completion —
// just diagnostics. Future phases lean on the same Document tracking
// to layer richer features without changing the dispatch shape.

package analysis

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/trendvidia/protocompile/incremental"
	"github.com/trendvidia/protocompile/incremental/queries"
	"github.com/trendvidia/protocompile/ir"
	"github.com/trendvidia/protocompile/report"
	"github.com/trendvidia/protocompile/source"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/buftool"
	"github.com/trendvidia/protolsp/protoast"
	"github.com/trendvidia/protolsp/protoindex"
)

// IsProtoURI reports whether the URI is a .proto document we can
// analyze. Requires the file:// scheme in addition to the suffix:
// non-file URIs (registry:// schema previews, etc.) are virtual
// read-only views served by the schema resolver, NOT files we can
// run protocompile against. They never go through the analyze
// pipeline.
//
// Without the scheme check this routed registry:// .proto previews
// into analyzeProto, which then called URI.Filename() and panicked
// because go.lsp.dev/uri's Filename() only supports file://.
func IsProtoURI(u uri.URI) bool {
	s := string(u)
	return strings.HasPrefix(s, "file://") && strings.HasSuffix(s, ".proto")
}

// IsFileURI reports whether the URI has the file:// scheme. Used
// to gate the analyze pipeline — non-file URIs (registry://,
// untitled://, etc.) are virtual or read-only and don't go through
// any of the per-document analysis we'd run for files on disk.
func IsFileURI(u uri.URI) bool {
	return strings.HasPrefix(string(u), "file://")
}

// analyzeProto runs the .proto document through protocompile and
// stores the resulting diagnostics on the Document. Caller holds
// doc.mu write lock — same locking discipline as analyze() so the
// pipeline can call either branch under the same critical section.
//
// Scope (intentionally limited to Phase 1):
//
//   - Parse errors and semantic errors from protocompile's reporter
//     surface as LSP diagnostics with the same source string
//     ("protolsp/proto") so editors can color / filter them.
//   - No symbol resolution, no go-to-def, no hover, no completion.
//     Those land in later phases of the proto-authoring plan and
//     don't change the dispatch shape — they read doc.Source and
//     the AST that this function discards.
//   - Workspace imports resolve through the proto index:
//     protocompile's standard imports (google/protobuf/*) come from
//     its built-in well-knowns, everything else looks up the
//     buf.yaml-driven include paths via SnapshotProtoIndex. Imports
//     the index can't supply still error, verbatim.
//   - No buf integration. Lint / format come in Phase 5.
//
// Errors without a position get an artificial range at line 1 col 1
// so the diagnostic still lands somewhere visible — protocompile
// occasionally emits "compile error" wrappers without source info
// for whole-file failures.
func (s *Workspace) analyzeProto(doc *Document) {
	doc.ProtoDiagnostics = nil
	doc.protoAST = nil
	doc.rfc001Calls = nil
	doc.rfc001SourceMap = nil

	src := doc.Source
	filename := doc.URI.Filename()
	if filename == "" {
		filename = string(doc.URI)
	}

	// Compile against the in-memory source, resolving imports
	// through the proto index: the same buf.yaml-driven
	// include paths that back hover / definition / completion also
	// back the compile pass, so a valid workspace import never
	// squiggles. The index maps the import path to its on-disk file
	// (disk state — an unsaved open buffer of the import is not
	// consulted, matching how the nav features resolve; the bytes
	// are re-read because the pipeline wants sources keyed by the
	// requested import path, and the index parses under absolute
	// paths). The pipeline recurses through the opener for the
	// import's own imports; standard imports (google/protobuf/*)
	// resolve from the built-in well-knowns. Anything the index
	// can't supply — or every non-self import when the index isn't
	// built — still errors, and we surface the pipeline's message
	// verbatim so the user can fix the path (or their buf.yaml).
	//
	// This runs protocompile's report-native incremental pipeline
	// (queries.IR) rather than the root Compiler: the report
	// carries a precise start..end source span for every finding,
	// so squiggles cover the offending tokens exactly. (The root
	// Compiler's legacy Reporter routing — restored in protocompile
	// v0.16.0 — delivers point positions, which would degrade these
	// to one-character ranges.) The IR result itself is discarded;
	// this pass only wants the diagnostics.
	opener := &source.Openers{
		analyzeOpener{filename: filename, src: src, idx: s.SnapshotProtoIndex()},
		source.WKTs(),
	}
	results, rpt, runErr := incremental.Run(context.Background(), incremental.New(),
		queries.IR{Opener: opener, Session: &ir.Session{}, Path: filename})

	// Errors only (plus ICEs, which are compiler bugs the user
	// should still see). The pipeline's warnings are style-lint
	// shaped ("missing `package` declaration") — protolsp's warning
	// channel stays reserved for buf lint, matching the diagnostic
	// surface editors saw before the fork swap.
	// The IR, when the pipeline produced one (a partially-diagnosed
	// file still has one): the RFC-001 artifacts below distill it, and
	// the unresolved-call quick-fix needs its visible `function`
	// declarations.
	var irFile *ir.File
	if len(results) > 0 && results[0].Fatal == nil {
		irFile = results[0].Value
	}
	var visibleFuncs []rfc001DeclaredFunction
	visibleFuncsOnce := false
	visible := func() []rfc001DeclaredFunction {
		if !visibleFuncsOnce {
			visibleFuncsOnce = true
			if irFile != nil {
				visibleFuncs = rfc001VisibleFunctions(irFile)
			}
		}
		return visibleFuncs
	}

	var diags []protocol.Diagnostic
	for i := range rpt.Diagnostics {
		d := &rpt.Diagnostics[i]
		if d.Level() != report.Error && d.Level() != report.ICE {
			continue
		}
		span := d.Primary()
		rng := protoErrorRange(1, 1)
		if !span.IsZero() {
			// Findings anchored in an imported file keep their own
			// positions in that file — painting them at foreign
			// line numbers on THIS document would squiggle the
			// wrong lines. The import statement itself already
			// carries a positioned error when the import is what's
			// broken.
			if span.File.Path() != filename {
				continue
			}
			rng = protoast.SpanRangeIn(span, s.positionEncoding)
		}
		diag := protocol.Diagnostic{
			Range:    rng,
			Severity: protocol.DiagnosticSeverityError,
			Source:   "protolsp/proto",
			Message:  d.Message(),
		}
		rfc001AttachQuickFix(&diag, visible)
		diags = append(diags, diag)
	}
	if fatal := runError(results, runErr); fatal != nil && len(diags) == 0 {
		// The pipeline bailed without producing a positioned
		// diagnostic. Surface the wrapper error so the user still
		// sees something rather than a silent failure.
		diags = append(diags, protocol.Diagnostic{
			Range:    protoErrorRange(1, 1),
			Severity: protocol.DiagnosticSeverityError,
			Source:   "protolsp/proto",
			Message:  fatal.Error(),
		})
	}
	doc.ProtoDiagnostics = diags

	// RFC-001 IR artifacts — before this pass discards the IR,
	// distill the two things only the compile result knows: resolved
	// function-call sites inside opaque engine-expression fragments
	// (opaque-fragment navigation) and the canonical source map
	// (runtime-violation → source mapping). Plain values only; the
	// compile session's arenas stay droppable.
	if irFile != nil {
		doc.rfc001Calls = rfc001CallSites(irFile, filename)
		doc.rfc001SourceMap = rfc001SourceMapFor(irFile)
	}

	// Phase 5b — append buf lint findings alongside protocompile's
	// errors. Gated by bufLintEnabled so tests don't spin up
	// subprocesses by accident; production sets this true in New().
	// Missing buf binaries degrade silently; per-invocation failures
	// log via slog and skip lint without dropping protocompile's
	// diagnostics. COMPILE-type findings (build errors from
	// unresolved imports) are filtered because protocompile already
	// reports the same situation upstream.
	if s.bufLintEnabled {
		s.appendBufLintDiagnostics(doc)
	}

	// Phase 2 — retain the AST so hover + go-to-definition can
	// resolve cursor positions to declarations without re-parsing.
	// We parse separately rather than fishing the AST out of the
	// Compile result: the parse is cheap, always yields a (possibly
	// partial) AST, and is decoupled from the link / semantic
	// stages that may have bailed out above. Parse diagnostics are
	// swallowed — any parse errors here have already been reported
	// by the Compile call above, and we don't want duplicates.
	parsed := protoast.Parse(filename, src)
	doc.protoAST = parsed

	// Phase 0 of the protocheck integration — schema-side expression
	// validation. Compiles each (protocheck.field|message) check
	// expression with the protocheck expression engine and surfaces broken expressions as
	// "protocheck/expression" diagnostics. Distinct source string so
	// editors can color / filter independently from protocompile +
	// buf-lint findings. Runs only when we have an AST; a nil parse
	// result means the file is too broken to walk safely.
	if parsed != nil {
		doc.ProtoDiagnostics = append(doc.ProtoDiagnostics, analyzeProtocheckExpressions(parsed, s.positionEncoding)...)
		doc.ProtoDiagnostics = append(doc.ProtoDiagnostics, analyzeProtovalidateExpressions(parsed, s.positionEncoding)...)
	}
}

// analyzeOpener feeds the compile pipeline: the open document's
// in-memory bytes for its own path, index-resolved disk bytes for
// workspace imports, not-found for everything else (the pipeline's
// well-known fallback sits behind it in a source.Openers chain).
type analyzeOpener struct {
	filename string
	src      []byte
	idx      *protoindex.Index
}

func (o analyzeOpener) Open(path string) (*source.File, error) {
	if path == o.filename {
		return source.NewFile(path, string(o.src)), nil
	}
	if node := o.idx.Lookup(path); node != nil {
		if data, err := os.ReadFile(node.Name()); err == nil {
			return source.NewFile(path, string(data)), nil
		}
	}
	return nil, fs.ErrNotExist
}

// runError reduces an incremental.Run outcome to the first fatal
// error: the executor-level error if any, else the first per-query
// fatal. nil when the run produced (possibly diagnostic-laden)
// results for every query.
func runError[T any](results []incremental.Result[T], runErr error) error {
	if runErr != nil {
		return runErr
	}
	for _, r := range results {
		if r.Fatal != nil {
			return r.Fatal
		}
	}
	return nil
}

// appendBufLintDiagnostics invokes buf lint against doc.Source and
// appends each finding to doc.ProtoDiagnostics as a Warning with
// source "buf/lint". Caller holds doc.mu (same locking discipline
// as analyzeProto, since the call sites overlap).
//
// COMPILE-type findings are filtered: protocompile's pass above
// already reports unresolved imports and build errors with
// source-anchored Error severity, so re-publishing buf's COMPILE
// noise here would clutter the gutter with duplicates.
//
// Buf runs synchronously inside analyzeProto. The debounce upstream
// already throttles per-change re-analysis; a per-call 50–100ms
// subprocess fits inside that window. Move to an async republish
// pass if a real perf complaint surfaces.
func (s *Workspace) appendBufLintDiagnostics(doc *Document) {
	findings, err := buftool.Lint(context.Background(), doc.Source, s.lintRelPath(doc))
	if err != nil {
		if errors.Is(err, buftool.ErrBufNotFound) {
			// Steady state on machines without buf — no log churn.
			return
		}
		s.log.Warn("buf lint failed", "uri", doc.URI, "err", err)
		return
	}
	for _, f := range findings {
		if f.Type == "COMPILE" {
			continue
		}
		doc.ProtoDiagnostics = append(doc.ProtoDiagnostics, protocol.Diagnostic{
			Range:    bufLintRange(f),
			Severity: protocol.DiagnosticSeverityWarning,
			Source:   "buf/lint",
			Message:  f.Message + " (" + f.Type + ")",
		})
	}
}

// lintRelPath maps the document's disk path to its include-path-
// relative form (forward slashes) so buf's directory- and filename-
// based lint rules judge the file where it actually lives.
// Empty when no index is built or the document lies outside every
// include path — buftool then falls back to bare staging.
func (s *Workspace) lintRelPath(doc *Document) string {
	idx := s.SnapshotProtoIndex()
	if idx == nil {
		return ""
	}
	filename := doc.URI.Filename()
	for _, inc := range idx.IncludePaths() {
		rel, err := filepath.Rel(inc, filename)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			continue
		}
		return filepath.ToSlash(rel)
	}
	return ""
}

// bufLintRange converts buf's 1-indexed finding position to an LSP
// 0-indexed Range. Buf occasionally reports 0 for a column when the
// finding isn't anchored to a specific token (whole-file rules like
// PACKAGE_DEFINED); we clamp to (0,0) so the diagnostic still has a
// concrete render position.
func bufLintRange(f buftool.LintFinding) protocol.Range {
	startLine := f.StartLine
	startCol := f.StartColumn
	endLine := f.EndLine
	endCol := f.EndColumn
	if startLine < 1 {
		startLine = 1
	}
	if startCol < 1 {
		startCol = 1
	}
	if endLine < startLine {
		endLine = startLine
	}
	if endCol < startCol {
		// Some findings emit a one-char span (start == end on the
		// same line). Promote to start+1 so the LSP range is
		// non-empty.
		endCol = startCol + 1
	}
	return protocol.Range{
		Start: protocol.Position{
			Line:      uint32(startLine - 1), // #nosec G115 -- clamped above
			Character: uint32(startCol - 1),  // #nosec G115 -- clamped above
		},
		End: protocol.Position{
			Line:      uint32(endLine - 1), // #nosec G115 -- clamped above
			Character: uint32(endCol - 1),  // #nosec G115 -- clamped above
		},
	}
}

// protoErrorRange builds a one-character LSP range at the given
// 1-based line + column. protocompile emits 0 for the column on some
// errors; we clamp to (1,1) in that case so the editor always has a
// concrete position to draw the squiggle at.
func protoErrorRange(line, col int) protocol.Range {
	if line < 1 {
		line = 1
	}
	if col < 1 {
		col = 1
	}
	lspLine := uint32(line - 1) // #nosec G115 -- guarded above
	lspCol := uint32(col - 1)   // #nosec G115 -- guarded above
	return protocol.Range{
		Start: protocol.Position{Line: lspLine, Character: lspCol},
		End:   protocol.Position{Line: lspLine, Character: lspCol + 1},
	}
}
