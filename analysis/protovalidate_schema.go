// Phase 0 of the protovalidate → protolsp integration:
// schema-side validation of CEL expressions in
// `(buf.validate.*).cel` annotations. Mirror of the protocheck
// Phase 0 work (protocheck_schema_real.go) but for CEL via
// cel-go.
//
// Always tag-free — protovalidate-go + cel-go are Apache 2.0.
//
// MVP scope:
//   - Traversal form `(buf.validate.field).cel = {…}` and
//     `(buf.validate.field).cel = [{…}, …]` (singular and
//     repeated). Aggregate form
//     `(buf.validate.field) = { cel: [{…}] }` also recognized.
//   - Same for `(buf.validate.message).cel`. Message-level `this`
//     binds to cel.DynType — cel-go's dynamic access pattern
//     means `this.field_name` parses and type-checks
//     liberally; static field-name resolution against the
//     enclosing message is a future polish.
//   - Field-level `this` types from the protobuf scalar where
//     the AST exposes it (string → string, int32/64-family → int,
//     float/double → double, bool → bool, bytes → bytes). Enum
//     and message-typed fields fall back to DynType for the
//     same reason the protocheck Phase 0 falls back: the AST
//     can't disambiguate without semantic resolution, and
//     accurate enum-typing isn't worth the resolver pass.
//   - Diagnostic ranges narrow to the CEL token via
//     *common.Error.Location, mirroring protocheck's per-column
//     ranges. Backslash-escape fallback is the same as Phase 0
//     for protocheck.
//   - The environment is protovalidate's own: every rule
//     compiles against `pvcel.NewLibrary()` — the strings
//     extension, the `now` variable, `isEmail` / `isHostname` /
//     `isIp` / `isUri` / `unique` / … and the bytes overloads —
//     exactly what protovalidate's runtime compiles the same rule
//     against, so a rule the runtime accepts is never reported and
//     a rule it rejects (`math.greatest`, which the library does
//     not enable) is. protovalidate binds `this` the same way for
//     user-written rules; it binds no `rules` variable for them.

package analysis

import (
	"sync"

	pvcel "buf.build/go/protovalidate/cel"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"github.com/trendvidia/protocompile/token"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// analyzeProtovalidateExpressions returns diagnostics for any
// broken CEL expressions in `(buf.validate.*).cel` annotations,
// with ranges in the negotiated position encoding.
// Nil-safe on the input — caller (analyzeProto) passes the
// parsed file which may be nil if the parser bailed completely.
func analyzeProtovalidateExpressions(file *protoast.File, enc positions.Encoding) []protocol.Diagnostic {
	if file == nil || file.AST == nil {
		return nil
	}
	var diags []protocol.Diagnostic
	for decl := range seq.Values(file.AST.Decls()) {
		def := decl.AsDef()
		if def.IsZero() || def.Classify() != ast.DefKindMessage {
			continue
		}
		diags = append(diags, analyzeMessageProtovalidate(file, def.AsMessage(), enc)...)
	}
	return diags
}

func analyzeMessageProtovalidate(file *protoast.File, msg ast.DefMessage, enc positions.Encoding) []protocol.Diagnostic {
	var diags []protocol.Diagnostic
	for decl := range seq.Values(msg.Body.Decls()) {
		def := decl.AsDef()
		if def.IsZero() {
			continue
		}
		switch def.Classify() {
		case ast.DefKindOption:
			// Message-level CEL.
			if cels := collectCelEntries(def.AsOption().Option, "buf.validate.message"); cels != nil {
				env := newCelMessageEnv()
				for _, lit := range cels {
					diags = append(diags, validateSingleCelExpression(file, lit, env, enc)...)
				}
			}
		case ast.DefKindField:
			f := def.AsField()
			if f.Options.IsZero() {
				continue
			}
			for opt := range seq.Values(f.Options.Entries()) {
				cels := collectCelEntries(opt, "buf.validate.field")
				if cels == nil {
					continue
				}
				env := newCelFieldEnv(f)
				for _, lit := range cels {
					diags = append(diags, validateSingleCelExpression(file, lit, env, enc)...)
				}
			}
		}
	}
	return diags
}

// collectCelEntries returns each CEL-rule dict declared on a
// `(<ext>) = { cel: [...] }` aggregate option or a
// `(<ext>).cel = [...] / {...}` traversal option. Returns nil
// when the option targets a different extension or doesn't carry
// CEL at all.
func collectCelEntries(opt ast.Option, ext string) []ast.ExprDict {
	if opt.Path.IsZero() || opt.Value.IsZero() {
		return nil
	}
	var parts []ast.PathComponent
	for pc := range opt.Path.Components() {
		parts = append(parts, pc)
	}
	if len(parts) == 0 {
		return nil
	}
	// First part must be the extension reference: a
	// parenthesized (…) component whose inner path is the
	// dotted extension name.
	extPath := parts[0].AsExtension()
	if extPath.IsZero() || extPath.Canonicalized() != ext {
		return nil
	}

	switch len(parts) {
	case 1:
		// Aggregate form: `(ext) = { cel: [...] }` — find the
		// `cel` field in the body and pull its entries.
		body := opt.Value.AsDict()
		if body.IsZero() {
			return nil
		}
		celField := findMessageField(body, "cel")
		if celField.IsZero() {
			return nil
		}
		return celEntriesFromValue(celField.Value())
	case 2:
		// Traversal form: `(ext).cel = [...] / {...}`. Second
		// part must be the bare `cel` ident (no extension
		// parens).
		second := parts[1].AsIdent()
		if second.IsZero() || second.Text() != "cel" {
			return nil
		}
		return celEntriesFromValue(opt.Value)
	}
	return nil
}

// celEntriesFromValue normalizes `[{…}, {…}]` and `{…}` (singular
// repeated shorthand) into a flat slice of CEL-rule dicts.
func celEntriesFromValue(v ast.ExprAny) []ast.ExprDict {
	switch v.Kind() {
	case ast.ExprKindArray:
		elems := v.AsArray().Elements()
		out := make([]ast.ExprDict, 0, elems.Len())
		for e := range seq.Values(elems) {
			if lit := e.AsDict(); !lit.IsZero() {
				out = append(out, lit)
			}
		}
		return out
	case ast.ExprKindDict:
		return []ast.ExprDict{v.AsDict()}
	}
	return nil
}

func validateSingleCelExpression(file *protoast.File, lit ast.ExprDict, env *cel.Env, enc positions.Encoding) []protocol.Diagnostic {
	exprField := findMessageField(lit, "expression")
	if exprField.IsZero() {
		return nil
	}
	expr, anchor, ok := expressionStringFromValue(exprField.Value())
	if !ok || env == nil {
		return nil
	}
	_, issues := env.Compile(expr)
	if issues == nil || issues.Err() == nil {
		return nil
	}
	var diags []protocol.Diagnostic
	for _, e := range issues.Errors() {
		diags = append(diags, protocol.Diagnostic{
			Range:    celErrorRange(file, anchor, e, enc),
			Severity: protocol.DiagnosticSeverityError,
			Source:   "buf/validate/cel",
			Message:  e.Message,
		})
	}
	return diags
}

// protovalidateBaseEnv is the environment protovalidate's runtime
// compiles every rule against — cel-go's standard library plus
// protovalidate's own library — built once per process. Per-rule
// environments extend it with `this`. A nil env (the library
// failed to load, which cel-go only does on conflicting
// declarations) disables the analysis rather than degrading to a
// bare environment: a bare environment reports every protovalidate
// function as undeclared, a false positive.
var (
	protovalidateEnvOnce sync.Once
	protovalidateEnv     *cel.Env
)

func protovalidateBaseEnv() *cel.Env {
	protovalidateEnvOnce.Do(func() {
		protovalidateEnv, _ = cel.NewEnv(cel.Lib(pvcel.NewLibrary()))
	})
	return protovalidateEnv
}

// newCelFieldEnv constructs the CEL env for a field-level rule:
// protovalidate's environment with `this` typed from the protobuf
// scalar. Non-scalar field types (enum, message, repeated) fall
// back to DynType. nil when the base environment is unavailable.
func newCelFieldEnv(f ast.DefField) *cel.Env {
	t := cel.DynType
	if !fieldTypeIsRepeated(f.Type) {
		if scalar, ok := celTypeFromProtoScalar(fieldTypeName(f.Type)); ok {
			t = scalar
		}
	}
	return newCelEnvWithThis(t)
}

// newCelMessageEnv constructs the CEL env for a message-level
// rule. `this` is the enclosing message, which we can't easily
// model statically without semantic resolution, so it gets
// DynType. cel-go's dynamic access (`this.field_name`) parses
// and type-checks liberally — sufficient for the syntax + basic
// type errors a Phase 0 pass should catch. nil when the base
// environment is unavailable.
func newCelMessageEnv() *cel.Env {
	return newCelEnvWithThis(cel.DynType)
}

func newCelEnvWithThis(t *cel.Type) *cel.Env {
	base := protovalidateBaseEnv()
	if base == nil {
		return nil
	}
	env, err := base.Extend(cel.Variable("this", t))
	if err != nil {
		return nil
	}
	return env
}

func celTypeFromProtoScalar(name string) (*cel.Type, bool) {
	switch name {
	case "int32", "sint32", "sfixed32",
		"int64", "sint64", "sfixed64":
		return cel.IntType, true
	case "uint32", "fixed32",
		"uint64", "fixed64":
		return cel.UintType, true
	case "float", "double":
		return cel.DoubleType, true
	case "bool":
		return cel.BoolType, true
	case "string":
		return cel.StringType, true
	case "bytes":
		return cel.BytesType, true
	}
	return nil, false
}

// celErrorRange computes the precise LSP range for a cel-go
// compile error inside a string literal. Mirror of
// expressionErrorRange (which targets protocheck's expression
// compile errors); the structure is the same but cel.Error positions are
// already in expression-local coordinates so the math is simpler.
//
// Fallbacks to the whole-literal range when:
//   - The token isn't a string literal, or is a concatenation of
//     adjacent literals (`"a" "b"`) — the raw source has multiple
//     separate quoted parts with gaps, so a decoded-string offset
//     doesn't map to a single contiguous range.
//   - The literal contains backslash escapes — decoded-string
//     positions don't line up 1:1 with raw-source bytes through
//     escapes.
//   - The token span is zero (synthetic / corrupt node).
func celErrorRange(file *protoast.File, tok token.Token, e *common.Error, enc positions.Encoding) protocol.Range {
	whole := nodeRange(file, tok, enc)
	s := tok.AsString()
	if s.IsZero() || s.IsConcatenated() {
		return whole
	}
	if e == nil || e.Location == nil {
		return whole
	}
	tokSpan := tok.Span()
	if tokSpan.IsZero() {
		return whole
	}
	if s.HasEscapes() {
		return whole
	}

	// cel-go's Location: Line() is 1-based; Column() is 0-based
	// within the expression source. For single-line expressions
	// (the common case) line is always 1. For multi-line — not
	// expected in protovalidate annotations but defensive — we
	// fall back to whole-literal.
	if e.Location.Line() != 1 {
		return whole
	}
	// The token span starts at the opening quote; +1 skips it,
	// and for escape-free single literals decoded[i] == raw[i+1]
	// byte-for-byte, so Column() indexes straight into the raw
	// source. 1-char range at the bad token.
	start := tokSpan.Start + 1 + e.Location.Column()
	return file.RangeIn(file.Src.Span(start, start+1), enc)
}
