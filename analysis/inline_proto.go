// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// In-document schemas: the types a document's own @proto
// directives define. Draft -01 § The "@proto" Directive makes the
// embedded schema authoritative, so findTypeForDoc consults it before
// the workspace lookups, and every per-type feature (body validation,
// unknown fields, @dataset cell checks, Secret masking) works for a
// document that carries its own schema.
//
// The four body shapes compile the way protowire's `pxf` CLI compiles
// them (cmd/pxf/schema.go): a source form is a file of its own, a named
// form is wrapped in `message Name { ... }` under its package, an
// anonymous form is wrapped under a synthetic name and bound to the
// next typeless @dataset, and a descriptor form is a FileDescriptorSet.
// Imports resolve from the other @proto directives, the canonical
// schemas in inDocumentImports and the well-known types in
// documentWellKnownTypes; never from disk or a registry, and nothing
// else (draft -01 § The "@proto" Directive, "Imports": the list is
// exhaustive, decided on protowire#369 and protowire#388).

package analysis

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"

	"github.com/trendvidia/protocompile"
	"github.com/trendvidia/protocompile/reporter"
	"github.com/trendvidia/protowire"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// inlineSchema is what a document's @proto directives compile to.
// The zero value is a document with no in-document schema.
type inlineSchema struct {
	// key identifies what the directives compiled from: each one's
	// shape, name, body and anonymous binding. An edit that leaves
	// them unchanged reuses types and errs.
	key [sha256.Size]byte
	// types holds every message the directives define, nested ones
	// included, by full name: those of the files that compiled.
	types map[protoreflect.FullName]protoreflect.MessageType
	// errs is what did not compile, by directive, so it is placed
	// against the document as it is now.
	errs []inlineError
	// bound maps each typeless @dataset in Parsed.Datasets to the
	// synthetic name of the anonymous @proto bound to it.
	bound map[*pxf.DatasetDirective]protoreflect.FullName
	// diags reports what failed: a directive that did not compile, an
	// anonymous @proto with no @dataset to bind, a typeless @dataset
	// with no @proto before it.
	diags []protocol.Diagnostic
}

// inlineError is a compile error in directive proto of Parsed.Protos,
// at byte rel of its body, or at the directive when rel is -1.
type inlineError struct {
	proto, rel int
	msg        string
}

// findMessage returns the in-document type name, or nil.
func (is *inlineSchema) findMessage(name protoreflect.FullName) protoreflect.MessageType {
	if is == nil {
		return nil
	}
	return is.types[name]
}

// datasetType returns the row type name of d, which must point into
// doc.Parsed.Datasets: its own type, or the synthetic name of the
// anonymous @proto bound to it, or "".
func (doc *Document) datasetType(d *pxf.DatasetDirective) string {
	if d.Type != "" {
		return d.Type
	}
	return string(doc.inline.bound[d])
}

// loadInlineSchema compiles doc.Parsed's @proto directives into
// doc.inline, reusing the previous compile when the directives and
// their anonymous bindings have not changed. Bindings and diagnostics
// are worked out again on every pass: they hold positions. It runs
// with no @proto too: a typeless @dataset is then an orphan.
func loadInlineSchema(doc *Document) {
	p := doc.Parsed
	var diags []protocol.Diagnostic
	at := func(pos pxf.Position, format string, args ...any) {
		diags = append(diags, protocol.Diagnostic{
			Range:    pxfPosToRange(pos),
			Severity: protocol.DiagnosticSeverityError,
			Source:   optString("protolsp/proto"),
			Message:  protocol.String(fmt.Sprintf(format, args...)),
		})
	}

	// Bind each anonymous @proto to the next typeless @dataset, in
	// document order (draft § The "@proto" Directive: the binding is
	// one-shot). An orphan on either side is an error.
	anonName := map[int]protoreflect.FullName{}
	bound := map[*pxf.DatasetDirective]protoreflect.FullName{}
	type ent struct {
		off, idx int
		isProto  bool
	}
	var ents []ent
	for i, pd := range p.Protos {
		if pd.Shape == pxf.ProtoAnonymous {
			ents = append(ents, ent{pd.Pos.Offset, i, true})
		}
	}
	for i, d := range p.Datasets {
		if d.Type == "" {
			ents = append(ents, ent{d.Pos.Offset, i, false})
		}
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].off < ents[j].off })
	var pending []int
	for _, e := range ents {
		if e.isProto {
			pending = append(pending, e.idx)
			continue
		}
		if len(pending) == 0 {
			at(p.Datasets[e.idx].Pos, "@dataset has no type and no anonymous @proto before it to bind to")
			continue
		}
		name := protoreflect.FullName(fmt.Sprintf("%s%d", anonPrefix, len(anonName)))
		anonName[pending[0]] = name
		bound[&p.Datasets[e.idx]] = name
		pending = pending[1:]
	}
	for _, i := range pending {
		at(p.Protos[i].Pos, "anonymous @proto has no typeless @dataset after it to bind to")
	}

	key := inlineSchemaKey(p, anonName)
	if key != doc.inline.key {
		doc.inline = compileInlineSchema(p, anonName)
		doc.inline.key = key
	}
	for _, e := range doc.inline.errs {
		pd := p.Protos[e.proto]
		pos := pd.Pos
		if e.rel >= 0 {
			if off, ok := bodyOffset(doc.Source, pd, e.rel); ok {
				pos = positionAt(doc.Source, off)
			}
		}
		at(pos, "@proto does not compile: %s", e.msg)
	}
	doc.inline.bound = bound
	doc.inline.diags = diags
}

func inlineSchemaKey(p *pxf.Document, anonName map[int]protoreflect.FullName) [sha256.Size]byte {
	h := sha256.New()
	for i, pd := range p.Protos {
		fmt.Fprintf(h, "%d|%d|%s|%s|%d|", pd.Shape, len(pd.TypeName), pd.TypeName, anonName[i], len(pd.Body))
		h.Write(pd.Body)
	}
	var key [sha256.Size]byte
	h.Sum(key[:0])
	return key
}

// anonPrefix starts the synthetic name of an anonymous @proto. It is
// protolsp's own: a region bound to an anonymous @proto keeps an empty
// TypeFQN, and messages name it as "the anonymous @proto row type"
// (rowTypeName).
const anonPrefix = "__protolsp_anon_"

// inlineFile is one compiled file: the directive it came from and the
// bytes the file adds before the directive's body, so a compile error
// lands on the line that caused it.
type inlineFile struct {
	directive int // index in Parsed.Protos
	header    int
	hasBody   bool // false for a descriptor form's files
}

func compileInlineSchema(p *pxf.Document, anonName map[int]protoreflect.FullName) inlineSchema {
	var out inlineSchema
	fail := func(directive, rel int, format string, args ...any) {
		out.errs = append(out.errs, inlineError{directive, rel, fmt.Sprintf(format, args...)})
	}

	// One virtual file per directive. Two named directives with the
	// same name and body define one type, which the draft allows, so
	// the second is dropped rather than reported as a duplicate.
	virtual := map[string][]byte{}
	descs := map[string]*descriptorpb.FileDescriptorProto{}
	placed := map[string]inlineFile{}
	var roots []string
	named := map[string]bool{}
	for i, pd := range p.Protos {
		name := fmt.Sprintf("__protolsp_inline_%d.proto", i)
		switch pd.Shape {
		case pxf.ProtoSource:
			virtual[name] = pd.Body
		case pxf.ProtoNamed:
			k := pd.TypeName + "\x00" + string(pd.Body)
			if named[k] {
				continue
			}
			named[k] = true
			virtual[name] = wrapMessageBody(pd.TypeName, pd.Body)
		case pxf.ProtoAnonymous:
			n, ok := anonName[i]
			if !ok {
				continue // orphan, reported by loadInlineSchema
			}
			virtual[name] = wrapMessageBody(string(n), pd.Body)
		case pxf.ProtoDescriptor:
			set := &descriptorpb.FileDescriptorSet{}
			if err := proto.Unmarshal(pd.Body, set); err != nil {
				fail(i, -1, "descriptor is not a FileDescriptorSet: %v", err)
				continue
			}
			for _, f := range set.GetFile() {
				descs[f.GetName()] = f
				placed[f.GetName()] = inlineFile{directive: i}
				roots = append(roots, f.GetName())
			}
			continue
		default:
			fail(i, -1, "unknown @proto shape")
			continue
		}
		f := inlineFile{directive: i, hasBody: true}
		if pd.Shape != pxf.ProtoSource {
			f.header = len(virtual[name]) - len(pd.Body) - len("\n}\n")
		}
		placed[name] = f
		roots = append(roots, name)
	}
	if len(roots) == 0 {
		return out
	}

	// Not WithStandardImports, which would serve every file protoc
	// bundles under google/protobuf/. An allowed well-known type is
	// answered with an empty result, and the compiler's own copy serves
	// it; anything else is refused, as the reference refuses it
	// (protowire internal/schemaresolve compileDocument).
	resolver := protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
		if b, ok := virtual[path]; ok {
			return protocompile.SearchResult{Source: bytes.NewReader(b)}, nil
		}
		if f, ok := descs[path]; ok {
			return protocompile.SearchResult{Proto: f}, nil
		}
		if slices.Contains(inDocumentImports, path) {
			b, err := fs.ReadFile(protowire.BundledProto, "proto/"+path)
			if err != nil {
				return protocompile.SearchResult{}, err
			}
			return protocompile.SearchResult{Source: bytes.NewReader(b)}, nil
		}
		if slices.Contains(documentWellKnownTypes, path) {
			return protocompile.SearchResult{}, nil
		}
		return protocompile.SearchResult{}, errNotImportable
	})
	var compileErrs []reporter.ErrorWithPos
	rep := reporter.NewReporter(func(e reporter.ErrorWithPos) error {
		compileErrs = append(compileErrs, e)
		return nil
	}, nil)
	c := &protocompile.Compiler{Resolver: resolver, Reporter: rep}
	files, err := c.Compile(context.Background(), roots...)
	if len(compileErrs) > 0 || err != nil {
		reported := false
		for _, e := range compileErrs {
			sp := e.GetPosition()
			f, ok := placed[sp.Filename]
			if !ok {
				continue
			}
			reported = true
			rel := -1
			if f.hasBody {
				if r := sp.Offset - f.header; r >= 0 && r <= len(p.Protos[f.directive].Body) {
					rel = r
				}
			}
			fail(f.directive, rel, "%s", e.Unwrap().Error())
		}
		if !reported {
			msg := "unknown error"
			if err != nil && !errors.Is(err, reporter.ErrInvalidSource) {
				msg = err.Error()
			} else if len(compileErrs) > 0 {
				msg = compileErrs[0].Error()
			}
			fail(0, -1, "%s", msg)
		}
	}
	// What did compile is kept, as the workspace's own .proto files
	// are (schema.OpenProtoFiles): one @proto mid-edit does not take
	// the others' types, and their Secret masking, with it. protocompile
	// leaves a nil in the slot of a file that failed, or that imports
	// one.
	out.types = map[protoreflect.FullName]protoreflect.MessageType{}
	for _, f := range files {
		if f == nil {
			continue
		}
		addMessages(out.types, f.Messages())
	}
	return out
}

// inDocumentImports are the canonical protowire schemas a source-form
// @proto may import: the schemas whose types a document can hold
// (draft -01 § The "@proto" Directive, decided on protowire#369). A
// copy of protowire's internal schemaresolve.InDocumentImports.
var inDocumentImports = []string{
	"pxf/annotations.proto",
	"pxf/bignum.proto",
	"pxf/secret.proto",
	"sbe/annotations.proto",
	"envelope/v1/envelope.proto",
}

// documentWellKnownTypes are the google/protobuf/ files a source-form
// @proto may import (decided on protowire#388): not compiler/plugin.proto
// or the language features files. A copy of protowire's internal
// schemaresolve.DocumentWellKnownTypes.
var documentWellKnownTypes = []string{
	"google/protobuf/any.proto",
	"google/protobuf/api.proto",
	"google/protobuf/descriptor.proto",
	"google/protobuf/duration.proto",
	"google/protobuf/empty.proto",
	"google/protobuf/field_mask.proto",
	"google/protobuf/source_context.proto",
	"google/protobuf/struct.proto",
	"google/protobuf/timestamp.proto",
	"google/protobuf/type.proto",
	"google/protobuf/wrappers.proto",
}

// errNotImportable is the answer for an import outside the draft's
// list. Its text is the reference's.
var errNotImportable = fmt.Errorf("a @proto may import only another @proto in the document, %s, or the well-known types %s",
	strings.Join(inDocumentImports, ", "), strings.Join(documentWellKnownTypes, ", "))

// bodyOffset is the document offset of byte rel of pd's body. A body
// written verbatim is found as is. A triple-quoted source form is not:
// the parser drops its leading newline and the closing line's indent
// from every line, so each line is mapped back where it was cut.
func bodyOffset(src []byte, pd pxf.ProtoDirective, rel int) (int, bool) {
	from := pd.Pos.Offset + len("@proto")
	if from < 0 || from > len(src) || len(pd.Body) == 0 {
		return 0, false
	}
	if j := bytes.Index(src[from:], pd.Body); j >= 0 {
		return from + j + rel, true
	}
	if pd.Shape != pxf.ProtoSource {
		return 0, false
	}
	q := bytes.Index(src[from:], []byte(`"""`))
	if q < 0 {
		return 0, false
	}
	start := from + q + len(`"""`)
	n := bytes.Index(src[start:], []byte(`"""`))
	if n < 0 {
		n = len(src) - start
	}
	raw := src[start : start+n]
	if len(raw) > 0 && raw[0] == '\n' {
		raw, start = raw[1:], start+1
	}
	// As the parser's dedent: the indent is the closing line, when
	// that line is only whitespace.
	lines := bytes.Split(raw, []byte("\n"))
	var indent []byte
	if last := lines[len(lines)-1]; len(bytes.TrimSpace(last)) == 0 {
		indent, lines = last, lines[:len(lines)-1]
	}
	var body []byte
	lineAt := start // document offset of the current raw line
	for k, line := range lines {
		if k > 0 {
			body = append(body, '\n')
		}
		cut := 0
		if bytes.HasPrefix(line, indent) {
			cut = len(indent)
		}
		if rel >= len(body) && rel <= len(body)+len(line)-cut {
			if !bytes.Equal(dedented(lines, indent), pd.Body) {
				return 0, false
			}
			return lineAt + cut + rel - len(body), true
		}
		body = append(body, line[cut:]...)
		lineAt += len(line) + 1
	}
	return 0, false
}

// dedented joins lines with indent cut from each, as the parser does.
func dedented(lines [][]byte, indent []byte) []byte {
	out := make([][]byte, len(lines))
	for i, l := range lines {
		out[i] = bytes.TrimPrefix(l, indent)
	}
	return bytes.Join(out, []byte("\n"))
}

// positionAt is the 1-based line and byte column of offset in src.
func positionAt(src []byte, offset int) pxf.Position {
	line, col := 1, 1
	for _, c := range src[:offset] {
		if c == '\n' {
			line, col = line+1, 1
			continue
		}
		col++
	}
	return pxf.Position{Line: line, Column: col, Offset: offset}
}

func addMessages(into map[protoreflect.FullName]protoreflect.MessageType, msgs protoreflect.MessageDescriptors) {
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		if md.IsMapEntry() {
			continue
		}
		into[md.FullName()] = dynamicpb.NewMessageType(md)
		addMessages(into, md.Messages())
	}
}

// wrapMessageBody turns the body of a named or anonymous @proto into a
// file: `@proto trades.v1.Trade { body }` is package trades.v1 holding
// `message Trade { body }`. A name with no dot has no package.
func wrapMessageBody(typeName string, body []byte) []byte {
	pkg, msg := "", typeName
	if i := strings.LastIndex(typeName, "."); i >= 0 {
		pkg, msg = typeName[:i], typeName[i+1:]
	}
	var b strings.Builder
	b.WriteString("syntax = \"proto3\";\n")
	if pkg != "" {
		fmt.Fprintf(&b, "package %s;\n", pkg)
	}
	fmt.Fprintf(&b, "message %s {\n", msg)
	b.Write(body)
	b.WriteString("\n}\n")
	return []byte(b.String())
}
