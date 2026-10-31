// Package protoopts holds AST helpers shared by the protocheck and
// protovalidate schema-side analyzers, so both can use one walker.
//
// Each analyzer has its own option-name matcher (the extension
// namespace differs), but they share the message-literal field
// lookup, the string-literal extraction, the field-type shape probes,
// and the AST-node to LSP-range conversion.
package protoopts

import (
	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"github.com/trendvidia/protocompile/source"
	"github.com/trendvidia/protocompile/token"
	"github.com/trendvidia/protocompile/token/keyword"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// FindMessageField returns the first non-extension field with
// the given simple name in a message literal (ast.ExprDict).
// Extension keys ([name]) and Any-URL keys are ignored because
// neither protocheck nor protovalidate nest extensions inside
// their option bodies. Returns the zero ExprField when no field
// matches; callers check IsZero.
func FindMessageField(dict ast.ExprDict, name string) ast.ExprField {
	for f := range seq.Values(dict.Elements()) {
		// Only bare single-ident keys participate: Path.AsIdent
		// is zero for extension components and multi-part paths.
		ident := f.Key().AsPath().AsIdent()
		if ident.IsZero() || ident.Text() != name {
			continue
		}
		return f
	}
	return ast.ExprField{}
}

// ExpressionStringFromValue extracts the decoded expression
// string from a value expression. The value must be a string
// literal; the lexer fuses adjacent literals (`"foo " "bar"`)
// into a single String token, so compound concatenation is
// handled by StringToken.Text(). Returns the decoded value, the
// literal token to anchor diagnostics on, and ok=true when the
// value is string-shaped. Other shapes return ok=false and the
// analyzer silently skips them.
func ExpressionStringFromValue(v ast.ExprAny) (string, token.Token, bool) {
	tok := v.AsLiteral().Token
	if tok.Kind() != token.String {
		return "", token.Zero, false
	}
	return tok.AsString().Text(), tok, true
}

// NodeRange converts an AST node's source span into an
// LSP-shaped (0-based line/col) range, columns counted in the
// negotiated position encoding. Falls back to a synthetic
// 1:1 position if the span is zero (shouldn't happen for nodes
// we got from a successful parse, but defensive against
// zero-value nodes).
func NodeRange(file *protoast.File, n source.Spanner, enc positions.Encoding) protocol.Range {
	span := source.GetSpan(n)
	if span.IsZero() {
		return ErrorRange(1, 1)
	}
	return file.RangeIn(span, enc)
}

// FieldTypeIsRepeated reports whether a field's type carries the
// `repeated` label. The old FieldNode.Label is a TypePrefixed
// wrapper around the element type in the new AST.
func FieldTypeIsRepeated(ty ast.TypeAny) bool {
	for p := range ty.Prefixes() {
		if p.Prefix() == keyword.Repeated {
			return true
		}
	}
	return false
}

// FieldTypeName returns the canonical dotted name of a field's
// type with all prefixes (repeated/optional/...) stripped —
// "int32" for scalars, "Location" / "foo.Location" for message
// references. Empty for non-path types (map<...>, parse errors).
func FieldTypeName(ty ast.TypeAny) string {
	return ty.RemovePrefixes().AsPath().Canonicalized()
}

// ErrorRange builds a one-character LSP range at the given
// 1-based line + column. protocompile emits 0 for the column on some
// errors; we clamp to (1,1) in that case so the editor always has a
// concrete position to draw the squiggle at.
func ErrorRange(line, col int) protocol.Range {
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
