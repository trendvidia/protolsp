// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// R4 of the dataset-regions design: per-cell type-mismatch
// diagnostics for @dataset rows. Pre-R4, dataset rows had NO
// schema-aware checking (PXF v1 @dataset documents have no
// @type, so the document-level validator never ran on them).
// R4 walks each row's cells against the resolved row-message
// descriptor and emits a Diagnostic per cell whose Value kind
// doesn't match its column's field kind.
//
// R4 v1 scope vs the design's "huge regions skip from the full
// pass, validate on edit" rule:
//   - Non-huge regions: validated end-to-end on every analyze.
//   - Huge resolvable regions: skipped, with a single info
//     diagnostic on the directive line pointing users at the
//     `pxf validate` CLI for whole-region checks. Per-edit row-
//     targeting (the design's lazier validation) is R4.5+; the
//     incremental-sync hooks are now in place to make it
//     straightforward to add.
//   - Huge unresolvable regions: a single Error on the directive
//     line ("unknown type X — row validation disabled").
//
// Per-cell validation checks the cell's literal kind against the
// field its column names, as the decoder does (#290): a dotted column
// names the leaf field of its path; a message type takes the literal
// it is written as (a wrapper its inner type's, pxf.Secret a string,
// the big-number types a number, Timestamp and Duration their own)
// or a block; a repeated field takes a list, each element as a single
// value of its type (a keyed one also takes its block of named
// entries), and a map field a block of "key: value" entries, each
// value likewise; an identifier is an enum value's name, or inf / nan
// in a float; null is allowed only on a singular field with presence:
// a message, or a scalar declared optional (protowire-go#182). A row
// whose cells all pass is then bound by the decoder (pxf.BindRow), for
// what it decides from the value rather than its kind: an int out of
// range, a name the enum lacks, a map key its key type cannot parse,
// and what a message block holds (#293). Its error lands on the cell
// it names.

package analysis

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// validateDatasetRegions returns per-cell diagnostics for every
// non-huge resolvable region in the document. Caller (validate)
// appends them alongside the existing schema findings.
//
// Reads doc.DatasetRegions (already populated by analyze) and
// doc.Parsed.Datasets (the AST source). Pure function modulo
// the resolver lookup — no mutation on doc.
func (s *Workspace) validateDatasetRegions(doc *Document) []protocol.Diagnostic {
	if s == nil || doc == nil || doc.Parsed == nil {
		return nil
	}
	// Dataset rows always bind to user-domain types — protolsp's
	// internal descriptors don't define any @dataset row types.
	// When no user lookup is configured, the row-type check has
	// nothing to validate against; surfacing "unknown row type"
	// in that mode would falsely imply the user should have a
	// registry set up.
	if !s.hasUserLookup() && len(doc.inline.types) == 0 {
		return nil
	}
	if len(doc.DatasetRegions) == 0 || len(doc.Parsed.Datasets) == 0 {
		return nil
	}
	parsedByLine := indexParsedDatasets(doc.Parsed.Datasets)

	var out []protocol.Diagnostic
	for i := range doc.DatasetRegions {
		region := &doc.DatasetRegions[i]
		parsed := parsedByLine[region.Range.Start.Line]
		// Unresolvable, huge or not: explicit "no validation"
		// diagnostic on the directive line so the user sees the
		// misconfiguration plainly. Not when no user lookup is
		// configured (above: the document's own types do not make
		// a type it does not define a misconfiguration), and not
		// for a typeless @dataset, whose failed binding protolsp/proto
		// reports (#287).
		if !region.Resolvable {
			if s.hasUserLookup() && (parsed == nil || parsed.Type != "") {
				out = append(out, datasetUnresolvableDiagnostic(region))
			}
			continue
		}
		// Huge resolvable: info diagnostic pointing at the CLI
		// for whole-region checks. R4.5+ will replace this with
		// per-edit row-targeting using incremental-sync ranges.
		if region.Huge {
			out = append(out, datasetHugeSkippedDiagnostic(region))
			continue
		}
		if parsed == nil {
			continue
		}
		mt, err := s.findTypeForDoc(doc, protoreflect.FullName(doc.datasetType(parsed)))
		if err != nil {
			continue
		}
		desc := mt.Descriptor()
		out = append(out, validateRows(parsed, desc)...)
	}
	return out
}

// indexParsedDatasets maps each parsed dataset by its start line
// (converted from pxf 1-indexed to LSP 0-indexed). Used to match
// our DatasetRegion entries to the parser's directives.
func indexParsedDatasets(datasets []pxf.DatasetDirective) map[uint32]*pxf.DatasetDirective {
	out := make(map[uint32]*pxf.DatasetDirective, len(datasets))
	for i := range datasets {
		d := &datasets[i]
		var line uint32
		if d.Pos.Line > 0 {
			line = uint32(d.Pos.Line - 1) // #nosec G115 -- line numbers within doc bounds
		}
		out[line] = d
	}
	return out
}

// validateRows walks a parsed dataset's Rows + Columns against
// the row-message descriptor and emits a Diagnostic per cell whose
// Value type doesn't match its column's field kind. Skips cells
// that are nil (absent — already valid per the row-shape rules),
// and skips columns the row type doesn't declare (these are
// emitted as a single per-column diagnostic on the directive
// line so we don't repeat it on every row). A row whose cells all
// pass goes to the decoder (rowProblem); with a bad column it would
// only repeat the column's error, so then no row does.
func validateRows(d *pxf.DatasetDirective, desc protoreflect.MessageDescriptor) []protocol.Diagnostic {
	var out []protocol.Diagnostic
	// First: per-column field resolution against the descriptor.
	// Unknown columns emit one diagnostic at the directive
	// position (not per row — that would N-flood for an unknown
	// column with many rows).
	cols := make([]protoreflect.FieldDescriptor, len(d.Columns))
	columnsOK := true
	for i, col := range d.Columns {
		fd, problem := columnField(desc, col)
		cols[i] = fd
		if problem != "" {
			columnsOK = false
			out = append(out, protocol.Diagnostic{
				Range:    pxfPosToRange(d.Pos),
				Severity: protocol.DiagnosticSeverityError,
				Source:   "protolsp/dataset",
				Message:  problem,
			})
		}
	}
	// Then: per-cell type checking.
	for _, row := range d.Rows {
		rowOK := columnsOK
		for i, cell := range row.Cells {
			if i >= len(cols) || cols[i] == nil || cell == nil {
				continue
			}
			pos, problem := cellProblem(d.Columns[i], cell, cols[i])
			if problem == "" {
				continue
			}
			rowOK = false
			out = append(out, protocol.Diagnostic{
				Range:    cellRange(pos),
				Severity: protocol.DiagnosticSeverityError,
				Source:   "protolsp/dataset",
				Message:  problem,
			})
		}
		if !rowOK {
			continue
		}
		if pos, problem := rowProblem(d, desc, row); problem != "" {
			out = append(out, protocol.Diagnostic{
				Range:    cellRange(pos),
				Severity: protocol.DiagnosticSeverityError,
				Source:   "protolsp/dataset",
				Message:  problem,
			})
		}
	}
	return out
}

// rowProblem binds row as the decoder does and returns the decoder's
// error, or "" when it binds. The decoder stops at its first error and
// names the column it was in ("column X: ..."); the error goes on that
// column's cell, and on the row when it names none.
func rowProblem(d *pxf.DatasetDirective, desc protoreflect.MessageDescriptor, row pxf.DatasetRow) (pxf.Position, string) {
	err := pxf.BindRow(dynamicpb.NewMessage(desc), d.Columns, row)
	if err == nil {
		return pxf.Position{}, ""
	}
	pos, msg := row.Pos, err.Error()
	var pe *pxf.Error
	if errors.As(err, &pe) {
		msg = pe.Msg
	}
	for i, col := range d.Columns {
		rest, ok := strings.CutPrefix(msg, "column "+col+": ")
		if ok && i < len(row.Cells) && row.Cells[i] != nil {
			pos, msg = valuePos(row.Cells[i]), fmt.Sprintf("column %q: %s", col, rest)
			break
		}
	}
	return pos, hideAnonNames(msg)
}

// anonName matches the synthetic name of an anonymous @proto in the
// decoder's words, with the dot that follows it when a nested type is
// named.
var anonName = regexp.MustCompile(regexp.QuoteMeta(anonPrefix) + `[0-9]+(\.?)`)

// hideAnonNames rewrites a decoder message the way shownName names a
// type: a type nested in an anonymous @proto by its name inside it, and
// the anonymous type itself as the anonymous @proto row type.
func hideAnonNames(msg string) string {
	return anonName.ReplaceAllStringFunc(msg, func(m string) string {
		if strings.HasSuffix(m, ".") {
			return ""
		}
		return "the anonymous @proto row type"
	})
}

// columnField resolves a column to the field its cells bind to: a
// field of desc, or for a dotted path the leaf field it names. When it
// cannot, it returns the reason, as the binder words it (draft -01
// § The "@dataset" Directive, Column paths): a segment names no field,
// or a path goes through a repeated, map or scalar field, or into a
// type written as a literal. pxf.Secret is not one of those. A nil
// field always comes with a reason.
func columnField(desc protoreflect.MessageDescriptor, col string) (protoreflect.FieldDescriptor, string) {
	segs := strings.Split(col, ".")
	md := desc
	for i, seg := range segs {
		fd := md.Fields().ByName(protoreflect.Name(seg))
		switch {
		case fd == nil && i == 0:
			return nil, fmt.Sprintf("unknown column %q on %s", col, rowTypeName(desc))
		case fd == nil:
			return nil, fmt.Sprintf("column %q: %s has no field %q", col, shownName(md), seg)
		case i == len(segs)-1:
			return fd, ""
		}
		at := strings.Join(segs[:i+1], ".")
		switch {
		case fd.IsMap():
			return nil, fmt.Sprintf("column %q: %q is a map field, and a column path cannot go through one", col, at)
		case fd.IsList():
			return nil, fmt.Sprintf("column %q: %q is a repeated field, and a column path cannot go through one", col, at)
		case fd.Message() == nil:
			return nil, fmt.Sprintf("column %q: %q is not a message field", col, at)
		}
		if lit := literalMessage(fd.Message()); lit != literalNone && lit != literalSecret {
			return nil, fmt.Sprintf("column %q: %q is a %s, which is written as a literal; a column path cannot go into it", col, at, fd.Message().FullName())
		}
		md = fd.Message()
	}
	panic("unreachable: strings.Split returns at least one segment")
}

// literalKind names the literal a message type is written as, besides
// a block.
type literalKind int

const (
	literalNone      literalKind = iota // a block only
	literalWrapper                      // its inner value field's literal
	literalSecret                       // a string
	literalBigInt                       // an integer
	literalBigNumber                    // an integer or a float (Decimal, BigFloat)
	literalTimestamp
	literalDuration
)

func literalMessage(md protoreflect.MessageDescriptor) literalKind {
	switch md.FullName() {
	case "google.protobuf.DoubleValue", "google.protobuf.FloatValue",
		"google.protobuf.Int64Value", "google.protobuf.UInt64Value",
		"google.protobuf.Int32Value", "google.protobuf.UInt32Value",
		"google.protobuf.BoolValue", "google.protobuf.StringValue",
		"google.protobuf.BytesValue":
		return literalWrapper
	case pxfSecretFQN:
		return literalSecret
	case "pxf.BigInt":
		return literalBigInt
	case "pxf.Decimal", "pxf.BigFloat":
		return literalBigNumber
	case "google.protobuf.Timestamp":
		return literalTimestamp
	case "google.protobuf.Duration":
		return literalDuration
	}
	return literalNone
}

// cellProblem checks a cell's literal kinds against the field its
// column names. It returns where the field stops accepting the cell
// and what is wrong there, or "" when the field accepts it: the cell
// itself, a list element, or a map entry or value. Where the decoder's
// verdict depends on more than the kind (an int out of range, a name
// the enum lacks, what a block holds), the cell is accepted, so a
// doubt never becomes a false error.
func cellProblem(col string, cell pxf.Value, fd protoreflect.FieldDescriptor) (pxf.Position, string) {
	switch {
	case fd.IsList():
		if _, ok := cell.(*pxf.BlockVal); ok && pxf.IsKeyed(fd) {
			return pxf.Position{}, "" // a keyed field's block of named entries
		}
		lv, ok := cell.(*pxf.ListVal)
		if !ok {
			break
		}
		for _, el := range lv.Elements {
			if !elementMatches(el, fd) {
				return valuePos(el), fmt.Sprintf("column %q: a list element expects %s; got %s", col, singularForm(fd), cellKindName(el))
			}
		}
		return pxf.Position{}, ""
	case fd.IsMap():
		bv, ok := cell.(*pxf.BlockVal)
		if !ok {
			break
		}
		for _, e := range bv.Entries {
			me, ok := e.(*pxf.MapEntry)
			if !ok {
				pos := entryPos(e)
				if pos == (pxf.Position{}) {
					pos = bv.Pos // an @type entry
				}
				return pos, fmt.Sprintf("column %q: a map entry is written \"key: value\"", col)
			}
			if !elementMatches(me.Value, fd.MapValue()) {
				return valuePos(me.Value), fmt.Sprintf("column %q: a map value expects %s; got %s", col, singularForm(fd.MapValue()), cellKindName(me.Value))
			}
		}
		return pxf.Position{}, ""
	default:
		if singularMatches(cell, fd) || nullable(cell, fd) {
			return pxf.Position{}, ""
		}
	}
	return valuePos(cell), fmt.Sprintf("column %q expects %s; got %s", col, expectedForm(fd), cellKindName(cell))
}

// elementMatches reports whether a list element or a map value is one
// the field's element type accepts: what a singular field of that type
// accepts, except null.
func elementMatches(v pxf.Value, fd protoreflect.FieldDescriptor) bool {
	if _, null := v.(*pxf.NullVal); null {
		return false
	}
	return singularMatches(v, fd)
}

// nullable reports whether v is a null a singular field fd takes, as
// the decoder decides it (protowire-go nullable): fd has presence and
// is not required. A message field takes null in singularMatches.
func nullable(v pxf.Value, fd protoreflect.FieldDescriptor) bool {
	_, null := v.(*pxf.NullVal)
	return null && fd.HasPresence() && fd.Cardinality() != protoreflect.Required
}

// singularMatches reports whether v is a literal a singular field of
// fd's type accepts; fd's cardinality is ignored.
func singularMatches(v pxf.Value, fd protoreflect.FieldDescriptor) bool {
	if fd.Message() != nil {
		return cellMatchesMessage(v, fd.Message())
	}
	return cellMatchesScalar(v, fd.Kind())
}

func cellMatchesMessage(cell pxf.Value, md protoreflect.MessageDescriptor) bool {
	switch cell.(type) {
	case *pxf.BlockVal, *pxf.NullVal:
		return true
	case *pxf.ListVal:
		return false
	}
	switch literalMessage(md) {
	case literalWrapper:
		return cellMatchesScalar(cell, md.Fields().ByName("value").Kind())
	case literalSecret:
		_, ok := cell.(*pxf.StringVal)
		return ok
	case literalBigInt:
		_, ok := cell.(*pxf.IntVal)
		return ok
	case literalBigNumber:
		switch cell.(type) {
		case *pxf.IntVal, *pxf.FloatVal:
			return true
		}
		return false
	case literalTimestamp:
		_, ok := cell.(*pxf.TimestampVal)
		return ok
	case literalDuration:
		_, ok := cell.(*pxf.DurationVal)
		return ok
	}
	return false // a block only
}

func cellMatchesScalar(cell pxf.Value, kind protoreflect.Kind) bool {
	switch v := cell.(type) {
	case *pxf.StringVal:
		return kind == protoreflect.StringKind
	case *pxf.BytesVal:
		return kind == protoreflect.BytesKind
	case *pxf.BoolVal:
		return kind == protoreflect.BoolKind
	case *pxf.FloatVal:
		return kind == protoreflect.FloatKind || kind == protoreflect.DoubleKind
	case *pxf.IntVal:
		switch kind {
		case protoreflect.Int32Kind, protoreflect.Int64Kind,
			protoreflect.Uint32Kind, protoreflect.Uint64Kind,
			protoreflect.Sint32Kind, protoreflect.Sint64Kind,
			protoreflect.Fixed32Kind, protoreflect.Fixed64Kind,
			protoreflect.Sfixed32Kind, protoreflect.Sfixed64Kind,
			protoreflect.EnumKind, protoreflect.FloatKind, protoreflect.DoubleKind:
			return true
		}
		return false
	case *pxf.IdentVal:
		// An enum value's name (whether the enum has it is the
		// decoder's call), or inf / nan in a float.
		switch kind {
		case protoreflect.EnumKind:
			return true
		case protoreflect.FloatKind, protoreflect.DoubleKind:
			return v.Name == "inf" || v.Name == "nan"
		}
		return false
	}
	return false // null, a block, a list, a timestamp, a duration
}

// expectedForm says what a column's field takes, for the mismatch
// message.
func expectedForm(fd protoreflect.FieldDescriptor) string {
	switch {
	case fd.IsList() && pxf.IsKeyed(fd):
		return "a list or a block"
	case fd.IsList():
		return "a list"
	case fd.IsMap():
		return "a map block"
	}
	return singularForm(fd)
}

// singularForm says what a singular field of fd's type takes; fd's
// cardinality is ignored.
func singularForm(fd protoreflect.FieldDescriptor) string {
	if fd.Message() == nil {
		return fd.Kind().String()
	}
	switch literalMessage(fd.Message()) {
	case literalWrapper:
		return fd.Message().Fields().ByName("value").Kind().String() + " or a block"
	case literalSecret:
		return "string or a block"
	case literalBigInt:
		return "an integer or a block"
	case literalBigNumber:
		return "a number or a block"
	case literalTimestamp:
		return "a timestamp or a block"
	case literalDuration:
		return "a duration or a block"
	}
	return "a block"
}

// cellRange returns the LSP Range for a position in a dataset row.
// Range is single-position — the position of the value's first byte,
// with End == Start. Editors can highlight from that point or expand
// to the whole token; LSP doesn't require non-zero-width ranges for
// diagnostics.
func cellRange(pos pxf.Position) protocol.Range {
	lspPos := PxfPosToLSP(pos)
	return protocol.Range{Start: lspPos, End: lspPos}
}

// cellKindName returns a short human-readable name for the cell's
// pxf.Value kind, used in diagnostic messages.
func cellKindName(cell pxf.Value) string {
	switch cell.(type) {
	case *pxf.StringVal:
		return "string"
	case *pxf.IntVal:
		return "int"
	case *pxf.BoolVal:
		return "bool"
	case *pxf.FloatVal:
		return "float"
	case *pxf.BytesVal:
		return "bytes"
	case *pxf.NullVal:
		return "null"
	case *pxf.TimestampVal:
		return "timestamp"
	case *pxf.DurationVal:
		return "duration"
	case *pxf.ListVal:
		return "list"
	case *pxf.BlockVal:
		return "block"
	case *pxf.IdentVal:
		return "identifier"
	}
	return "value"
}

// datasetUnresolvableDiagnostic builds the single diagnostic
// shown on a region whose row type isn't in the bound resolver.
// Error severity — the user almost certainly didn't intend this.
func datasetUnresolvableDiagnostic(region *DatasetRegion) protocol.Diagnostic {
	directiveRange := protocol.Range{
		Start: region.Range.Start,
		End:   protocol.Position{Line: region.Range.Start.Line, Character: region.Range.Start.Character + uint32(len("@dataset"))},
	}
	return protocol.Diagnostic{
		Range:    directiveRange,
		Severity: protocol.DiagnosticSeverityError,
		Source:   "protolsp/dataset",
		Message:  fmt.Sprintf("unknown row type %q — row validation disabled", region.TypeFQN),
	}
}

// datasetHugeSkippedDiagnostic builds the single info-severity
// note shown on a huge region whose rows aren't validated end-to-
// end. Points users at the CLI for whole-region checks.
func datasetHugeSkippedDiagnostic(region *DatasetRegion) protocol.Diagnostic {
	directiveRange := protocol.Range{
		Start: region.Range.Start,
		End:   protocol.Position{Line: region.Range.Start.Line, Character: region.Range.Start.Character + uint32(len("@dataset"))},
	}
	return protocol.Diagnostic{
		Range:    directiveRange,
		Severity: protocol.DiagnosticSeverityInformation,
		Source:   "protolsp/dataset",
		Message:  fmt.Sprintf("huge @dataset region (%d rows, %d bytes) — row validation skipped; run `pxf validate` for whole-region checks", region.RowCount, region.ByteSize),
	}
}

// rowTypeName names a row type in a message. The synthetic name of an
// anonymous @proto is protolsp's own, so it is not shown.
func rowTypeName(desc protoreflect.MessageDescriptor) string {
	if strings.HasPrefix(string(desc.FullName()), anonPrefix) {
		return "the anonymous @proto row type"
	}
	return fmt.Sprintf("row type %q", desc.FullName())
}

// shownName names a message in a diagnostic. A type nested in an
// anonymous @proto shows by its name inside it, without the synthetic
// root.
func shownName(md protoreflect.MessageDescriptor) string {
	n := string(md.FullName())
	if !strings.HasPrefix(n, anonPrefix) {
		return n
	}
	if _, rest, ok := strings.Cut(n, "."); ok {
		return rest
	}
	return "the anonymous @proto row type"
}
