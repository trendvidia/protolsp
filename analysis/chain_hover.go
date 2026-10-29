// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// C6 of the chameleon-integration design: merged-value hover.
// When the cursor lands on a field name in a PER_ENV chain layer
// document, append a "Chain resolution" section to the standard
// hover markdown listing the field's value at every layer in the
// env's resolve order. The last layer that sets the field is
// marked "← effective" so users see at a glance which value
// reaches the runtime.
//
// Behavior matches the design doc's worked example: rows show file
// path + value; absent layers are omitted (no noise); encrypted
// layers show `<encrypted>` so the user knows the layer exists but
// the LSP can't read it; pxf.Secret-typed fields render
// `<redacted>` across every layer so plaintext doesn't leak from
// ancestor files through hover. Standalone docs and ONCE chain
// layers get no chain section — the resolve-order semantics only
// apply to per-env leaves.

package analysis

import (
	"fmt"
	"path/filepath"
	"strings"

	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protolsp/redact"
	"github.com/trendvidia/protolsp/safeio"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// renderChainResolution builds the "Chain resolution" markdown
// section for a hover target, or returns "" when the document
// doesn't qualify for chain-aware merged-value display.
//
// Gates (any failing → "" so the caller falls through to the
// standard hover unchanged):
//   - doc is a PER_ENV chain layer with a bound environment
//   - workspace root is a file:// scheme (we need local disk)
//   - chain is loaded
//   - at least one layer in the env's resolve order sets the path
//
// Secret-typed fields: when the leaf descriptor is pxf.Secret,
// every row's value column is `<redacted>` regardless of source.
// This prevents plaintext from ancestor files reaching the hover
// payload — defense in depth alongside the existing per-doc
// redact.String pass on the rendered markdown.
func (s *Workspace) renderChainResolution(doc *Document, path string, fd protoreflect.FieldDescriptor) string {
	if doc == nil || doc.LayerInfo == nil || !doc.LayerInfo.IsLayer {
		return ""
	}
	if doc.LayerInfo.Strategy != "PER_ENV" || doc.LayerInfo.Environment == "" {
		return ""
	}
	docChain := s.ChainForDoc(doc)
	if docChain == nil {
		return ""
	}
	rootPath := s.workspaceDirForDoc(doc)
	if rootPath == "" {
		return ""
	}
	segments := strings.Split(path, ".")
	if len(segments) == 0 {
		return ""
	}
	order := docChain.ResolveOrder(doc.LayerInfo.Environment)
	if len(order) == 0 {
		return ""
	}
	secret := fd != nil && isSecretField(fd)

	type row struct {
		File  string
		Value string
		Set   bool
	}
	rows := make([]row, 0, len(order))
	for _, layer := range order {
		r := row{File: layer.ResolvedPath}
		// C7 part 2: env-sourced layers don't have a file on disk —
		// they synthesize values from process env vars at runtime.
		// Render an explicit placeholder so users see the layer
		// exists and what kind of source backs it.
		if docChain.LayerIsEnvSourced(layer) {
			r.File = fmt.Sprintf("(env source: %s)", layer.Source)
			r.Value = "<env>"
			rows = append(rows, r)
			continue
		}
		if layer.Encrypted {
			r.Value = "<encrypted>"
			rows = append(rows, r)
			continue
		}
		absPath := layer.ResolvedPath
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(rootPath, filepath.FromSlash(layer.ResolvedPath))
		}
		layerURI := uri.File(absPath)
		var content []byte
		if openDoc := s.docs.Get(layerURI); openDoc != nil {
			content = openDoc.Source
		} else if b, err := safeio.ReadFileLimit(absPath, safeio.MaxConfigFileBytes); err == nil {
			content = b
		}
		if content == nil {
			rows = append(rows, r)
			continue
		}
		v, found := extractValueAtPath(content, segments)
		if !found {
			rows = append(rows, r)
			continue
		}
		r.Set = true
		if secret {
			r.Value = redact.Placeholder
		} else {
			r.Value = v
		}
		rows = append(rows, r)
	}
	// Effective layer = last row whose Set is true. Encrypted rows
	// are NOT effective even when their on-paper position would
	// suggest otherwise — we can't observe their contents, so we
	// can't claim they're winning the merge.
	effectiveIdx := -1
	for i, r := range rows {
		if r.Set {
			effectiveIdx = i
		}
	}
	if effectiveIdx < 0 {
		// No readable layer sets the field; nothing useful to show.
		// The existing hover (type + descriptor location) is enough.
		return ""
	}
	// Render. Skip rows where the field isn't set and the layer
	// isn't encrypted — they'd be empty and add noise. Encrypted
	// rows always render because they carry useful "you can't see
	// in here" signal even when their content is unknown.
	var b strings.Builder
	b.WriteString("\nChain resolution:\n\n```\n")
	maxFile := 0
	for _, r := range rows {
		if !r.Set && r.Value == "" {
			continue
		}
		if len(r.File) > maxFile {
			maxFile = len(r.File)
		}
	}
	for i, r := range rows {
		if !r.Set && r.Value == "" {
			continue
		}
		marker := ""
		if i == effectiveIdx {
			marker = "  ← effective"
		}
		pad := strings.Repeat(" ", maxFile-len(r.File))
		fmt.Fprintf(&b, "  %s:%s %s%s\n", r.File, pad, r.Value, marker)
	}
	b.WriteString("```\n")
	return b.String()
}

// extractValueAtPath parses the given PXF content and returns a
// short string repr of the value at the dotted path, plus a bool
// for "the path resolved to a present assignment." Unparseable
// content yields ("", false) — a mid-edit layer file shouldn't
// poison sibling-layer rendering.
//
// Path semantics match findAssignmentByPath in
// field_refs_definition.go: top-level → walk Entries; nested →
// descend into Blocks whose Name matches the next segment.
func extractValueAtPath(content []byte, segments []string) (string, bool) {
	parsed, err := pxf.Parse(content)
	if err != nil || parsed == nil {
		return "", false
	}
	return walkValueByPath(parsed.Entries, segments)
}

// walkValueByPath is extractValueAtPath's recursive worker.
// Exposed separately for tests that operate on an already-parsed
// document.
func walkValueByPath(entries []pxf.Entry, segments []string) (string, bool) {
	if len(segments) == 0 {
		return "", false
	}
	target := segments[0]
	for _, e := range entries {
		switch n := e.(type) {
		case *pxf.Assignment:
			if n.Key == target && len(segments) == 1 {
				return formatValueRepr(n.Value), true
			}
		case *pxf.Block:
			if n.Name != target {
				continue
			}
			if len(segments) == 1 {
				return "{...}", true // block matched at root of path
			}
			if v, ok := walkValueByPath(n.Entries, segments[1:]); ok {
				return v, true
			}
		}
	}
	return "", false
}

// formatValueRepr renders a single-line string for a pxf.Value,
// suitable for the chain-resolution table column. Goal: stable,
// unambiguous, short. Non-scalar types collapse to a placeholder
// so the table stays one-line-per-layer.
//
// String values are double-quoted; the user sees the literal as
// authored. Integer / float / duration / timestamp keep their raw
// representation. Block / list values are flattened to `{...}` /
// `[...]` — rendering the inner content would blow up the hover
// payload and probably wasn't what the user asked to inspect.
func formatValueRepr(v pxf.Value) string {
	switch n := v.(type) {
	case *pxf.StringVal:
		return fmt.Sprintf("%q", n.Value)
	case *pxf.IntVal:
		return withPrec(n.Raw, n.Prec)
	case *pxf.FloatVal:
		return withPrec(n.Raw, n.Prec)
	case *pxf.BoolVal:
		if n.Value {
			return "true"
		}
		return "false"
	case *pxf.BytesVal:
		return "<bytes>"
	case *pxf.NullVal:
		return "null"
	case *pxf.IdentVal:
		return n.Name
	case *pxf.TimestampVal:
		return n.Raw
	case *pxf.DurationVal:
		return n.Raw
	case *pxf.ListVal:
		return "[...]"
	case *pxf.BlockVal:
		return "{...}"
	}
	return "<value>"
}

// withPrec writes a number literal as authored: Raw, and the precision
// suffix a pxf.BigFloat literal may carry (0.1p53).
func withPrec(raw, prec string) string {
	if prec == "" {
		return raw
	}
	return raw + "p" + prec
}
