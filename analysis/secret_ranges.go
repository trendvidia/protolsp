// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// S1 of the secret-handling design: walk the parsed AST against the
// bound message descriptor, record every assignment whose
// FieldDescriptor's message type is pxf.Secret, and stash the
// plaintext value + LSP range on the Document for downstream emit-
// boundary redaction.
//
// Two surface forms recognized:
//   - Scalar shorthand: `pw = "x"` — Assignment whose key resolves
//     to a pxf.Secret-typed field; Value is *pxf.StringVal.
//   - Block form: `pw { value = "x"; hint = "..." }` — Block whose
//     name resolves to a pxf.Secret-typed field; recurse one level
//     to find the inner `value` Assignment.
//
// Either form may also appear as a value: `pw = { value = "x" }`, an
// element of a list (`keys = ["a", { value = "b" }]`), a map value,
// or anywhere inside a message written as `db = { ... }`. And each
// may sit in a @dataset cell, under a column naming the Secret field
// or a dotted path to its `value` (collectDatasetSecretRanges).
//
// Non-string Secret values are skipped (the schema would reject
// them; we don't want to mask other parts of the diagnostic).

package analysis

import (
	"fmt"
	"strings"

	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// pxfSecretFQN is the well-known FQN every pxf.Secret-typed field
// references. Matches protowire-go's wellknown.go isSecret check.
const pxfSecretFQN = "pxf.Secret"

// collectSecretRanges walks entries against md, populating
// doc.SecretRanges and doc.secretValues for every pxf.Secret-typed
// assignment. pathPrefix is the dotted ancestry leading into this
// scope ("" at the top level; "credentials." inside a credentials
// block, etc.) and is used to assemble each finding's Path.
//
// Recurses into Blocks whose field type is a non-Secret message
// — same pattern as walkFieldAssignments / findHoverTarget. A
// Block whose field IS pxf.Secret stops the recursion at this
// level and goes through handleSecretBlock.
func collectSecretRanges(entries []pxf.Entry, md protoreflect.MessageDescriptor, pathPrefix string, doc *Document) {
	if md == nil {
		return
	}
	for _, entry := range entries {
		switch n := entry.(type) {
		case *pxf.Assignment:
			fd := md.Fields().ByName(protoreflect.Name(n.Key))
			if fd == nil {
				continue
			}
			collectSecretValue(n.Value, fd, pathPrefix+n.Key, doc)
		case *pxf.Block:
			fd := md.Fields().ByName(protoreflect.Name(n.Name))
			if fd == nil {
				continue
			}
			if isSecretField(fd) {
				handleSecretBlock(n.Entries, pathPrefix+n.Name, doc)
				continue
			}
			if pxf.KeyField(fd) != nil {
				// Keyed repeated field: descend into each entry's body
				// (keyed by name) against the element message so secrets
				// inside keyed entries are still detected. The entry key
				// joins the dotted path (children.loginButton.token).
				elem := fd.Message()
				for _, ce := range n.Entries {
					if ke, ok := asKeyedEntry(ce); ok {
						collectSecretRanges(ke.body, elem, pathPrefix+n.Name+"."+ke.key+".", doc)
					}
				}
				continue
			}
			if fd.Kind() == protoreflect.MessageKind && fd.Message() != nil {
				collectSecretRanges(n.Entries, fd.Message(), pathPrefix+n.Name+".", doc)
			}
		}
	}
}

// isSecretField reports whether the field's resolved type is
// pxf.Secret. Matches upstream's name-based detection — keeps the
// LSP independent of any specific Secret descriptor blob.
func isSecretField(fd protoreflect.FieldDescriptor) bool {
	if fd == nil {
		return false
	}
	msg := fd.Message()
	if msg == nil {
		return false
	}
	return string(msg.FullName()) == pxfSecretFQN
}

// collectSecretValue records the pxf.Secret values in v, the value
// bound to fd. A Secret is either form: a string literal is the
// scalar shorthand, a block the block form. A list binds each element
// to fd, a map block each value to fd's map value, and any other
// message block is walked for the Secrets inside it. Elements and map
// values extend the path by their index or key ("keys.0",
// "tenants.acme").
func collectSecretValue(v pxf.Value, fd protoreflect.FieldDescriptor, path string, doc *Document) {
	switch val := v.(type) {
	case *pxf.ListVal:
		if !fd.IsList() {
			return
		}
		for i, el := range val.Elements {
			collectSecretElement(el, fd, fmt.Sprintf("%s.%d", path, i), doc)
		}
	case *pxf.BlockVal:
		if fd.IsMap() {
			for _, e := range val.Entries {
				if me, ok := e.(*pxf.MapEntry); ok {
					collectSecretElement(me.Value, fd.MapValue(), path+"."+me.Key, doc)
				}
			}
			return
		}
		collectSecretElement(val, fd, path, doc)
	default:
		collectSecretElement(v, fd, path, doc)
	}
}

// collectSecretElement is collectSecretValue for one element: a
// value of fd's message type, never a whole list or map.
func collectSecretElement(v pxf.Value, fd protoreflect.FieldDescriptor, path string, doc *Document) {
	if isSecretField(fd) {
		switch val := v.(type) {
		case *pxf.StringVal:
			recordSecret(doc, path, val.Value, val.Pos, SecretFormScalar)
		case *pxf.BlockVal:
			handleSecretBlock(val.Entries, path, doc)
		}
		return
	}
	if bv, ok := v.(*pxf.BlockVal); ok && fd.Message() != nil {
		collectSecretRanges(bv.Entries, fd.Message(), path+".", doc)
	}
}

// collectDatasetSecretRanges records the pxf.Secret values in every
// @dataset row whose type resolves. A cell binds as the entry
// "<column> = <cell>" would in a body of the row type (draft -01
// § The "@dataset" Directive), so a column naming a Secret field, or
// a message holding one, is walked as that entry's value. A dotted
// column into a Secret (`pw.value`) names the Secret's value directly
// and records the cell; `pw.hint` and `pw.fingerprint` are not
// sensitive. A dataset bound to an anonymous @proto resolves to that
// directive's type (#287).
//
// Paths are "row<N>.<field path>", N counting a dataset's rows from
// 1, so each row's secret gets its own env-var name in the
// literal-secret quick fix (ROW2_PW).
func (s *Workspace) collectDatasetSecretRanges(doc *Document) {
	for i := range doc.Parsed.Datasets {
		d := &doc.Parsed.Datasets[i]
		name := doc.datasetType(d)
		if name == "" {
			continue
		}
		mt, err := s.findTypeForDoc(doc, protoreflect.FullName(name))
		if err != nil || mt == nil {
			continue
		}
		desc := mt.Descriptor()
		for ci, col := range d.Columns {
			fd, secretPath, ok := resolveSecretColumn(desc, col)
			if !ok {
				continue
			}
			for ri, row := range d.Rows {
				if ci >= len(row.Cells) || row.Cells[ci] == nil {
					continue
				}
				cell := row.Cells[ci]
				if secretPath != "" {
					if sv, ok := cell.(*pxf.StringVal); ok {
						recordSecret(doc, fmt.Sprintf("row%d.%s", ri+1, secretPath), sv.Value, sv.Pos, SecretFormBlock)
					}
					continue
				}
				collectSecretValue(cell, fd, fmt.Sprintf("row%d.%s", ri+1, col), doc)
			}
		}
	}
}

// resolveSecretColumn resolves a @dataset column against the row
// type, as the cell check does (columnField). It returns the field the
// column's cells bind to, or, for a dotted path into a pxf.Secret that
// ends in "value", the path of the Secret field itself ("db.pw" for
// "db.pw.value"). ok is false when the column cannot hold a secret: it
// is one the binder rejects, or goes into a Secret's hint or
// fingerprint.
func resolveSecretColumn(desc protoreflect.MessageDescriptor, col string) (fd protoreflect.FieldDescriptor, secretPath string, ok bool) {
	fd, problem := columnField(desc, col)
	if problem != "" {
		return nil, "", false
	}
	parent, inside := strings.CutSuffix(col, "."+string(fd.Name()))
	if !inside || fd.ContainingMessage().FullName() != pxfSecretFQN {
		return fd, "", true
	}
	if fd.Name() != "value" {
		return nil, "", false
	}
	return fd, parent, true
}

// handleSecretBlock finds the inner `value` Assignment within a
// block-form pxf.Secret declaration and records its range. The
// block's `hint` and `fingerprint` sub-fields are deliberately
// non-sensitive and left alone.
func handleSecretBlock(entries []pxf.Entry, path string, doc *Document) {
	for _, entry := range entries {
		assign, ok := entry.(*pxf.Assignment)
		if !ok {
			continue
		}
		if assign.Key != "value" {
			continue
		}
		sv, ok := assign.Value.(*pxf.StringVal)
		if !ok {
			continue
		}
		recordSecret(doc, path, sv.Value, sv.Pos, SecretFormBlock)
		return
	}
}

// recordSecret appends one secret entry to Document state. The Range
// covers the full string literal — opening quote through closing
// quote inclusive — so editors masking the range hide the whole
// "..." form, not just the inner content.
//
// Assumes the common scalar form `"value"` (open + content +
// close = len(value) + 2 source characters). Triple-quoted
// (`"""..."""`) and byte-string (`b"..."`) forms have different
// lengths and aren't computed precisely here; S3's literal-secret
// quick-fix targets only the common form, which is what protolsp
// users hit in practice.
//
// pxf.StringVal.Pos points at the opening quote per the upstream
// lexer (see protowire-go's lexer.go lexString — caller passes Pos
// before consuming the leading `"`). The +2 below assumes that.
func recordSecret(doc *Document, path, value string, pos pxf.Position, form SecretForm) {
	start := PxfPosToLSP(pos)
	r := protocol.Range{
		Start: start,
		End: protocol.Position{
			Line:      start.Line,
			Character: start.Character + uint32(len(value)) + 2, // #nosec G115 -- value length fits in uint32 for any realistic config secret.
		},
	}
	doc.SecretRanges = append(doc.SecretRanges, SecretRange{
		Range: r,
		Path:  path,
		Form:  form,
	})
	doc.secretValues = append(doc.secretValues, value)
}
