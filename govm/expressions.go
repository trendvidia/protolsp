// Phase 0 of the protocheck → protolsp integration: schema-side
// expression validation. Walks the .proto AST for
// `(protocheck.field)` / `(protocheck.message)` aggregate options
// carrying `check: [{ expression: "..." }]` entries and type-checks
// each expression via GoVM's eval.Env.Compile. Syntax errors, type
// mismatches, undefined identifiers, and non-bool return types all
// surface as LSP diagnostics with source "protocheck/expression".
//
// This is distinct from (and prerequisite for) Phase 1, which
// validates .pxf instance data against the same constraints at
// runtime via protocheck/v2 + the protocheck-govm expression
// engine. This file deliberately stays on direct govm eval — the
// v2 seam exposes evaluate-only entry points, and Phase 0 needs
// compile-only checking with *eval.CompileError offsets — but its
// variable-binding rules must mirror protocheck-govm's (see
// scalarFromTypeName). Catching broken expressions at schema-edit
// time prevents them from silently invalidating every downstream
// diagnostic Phase 1 would produce.
//
// MVP scope:
//   - Aggregate-form options only: `(protocheck.field) = {…}`. The
//     traversal forms `(protocheck.field).string = {…}` carry no
//     `check` entries by construction, so they're irrelevant.
//   - Field-level `this` env: scalar proto types only. Enum + message
//     fields produce no env entry; expressions referencing `this`
//     will fail compile naturally and surface that way.
//   - Message-level env: each enclosing scalar field becomes a typed
//     variable. Repeated / message / enum fields are skipped (no
//     scalar `this` semantics). Nested-message dot access
//     (`origin.city`) is a follow-up.
//   - Top-level messages only. Nested-message recursion is mechanical
//     but deferred until a real use case appears.
//   - Diagnostic range pins to the offending token via
//     `*eval.CompileError.Offset` + `Length` from govm v1.1+.
//     Whole-literal fallback for the small set of cases that
//     map awkwardly (no usable position info, backslash escapes
//     in the source literal, concatenated literals). See
//     `expressionErrorRange` for the table.
package govm

import (
	"errors"

	"github.com/trendvidia/protocompile/ast"
	"github.com/trendvidia/protocompile/seq"
	"github.com/trendvidia/protocompile/token"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/govm/v2/eval"
	"github.com/trendvidia/protolsp/internal/protoopts"
	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// analyzeProtocheckExpressions returns diagnostics for any broken
// protocheck expressions in the file, with ranges in the negotiated
// position encoding. Nil-safe on the input — the caller
// (analyzeProto) passes the parsed file which may be nil if the
// parser bailed completely.
func analyzeProtocheckExpressions(file *protoast.File, enc positions.Encoding) []protocol.Diagnostic {
	if file == nil || file.AST == nil {
		return nil
	}
	var diags []protocol.Diagnostic
	for decl := range seq.Values(file.AST.Decls()) {
		def := decl.AsDef()
		if def.IsZero() || def.Classify() != ast.DefKindMessage {
			continue
		}
		diags = append(diags, analyzeMessageProtocheck(file, def.AsMessage(), enc)...)
	}
	return diags
}

func analyzeMessageProtocheck(file *protoast.File, msg ast.DefMessage, enc positions.Encoding) []protocol.Diagnostic {
	var diags []protocol.Diagnostic
	// File-level message-type index for nested-message resolution
	// in message-level expressions (`origin.city != destination.city`).
	// Built per outer call so it stays linear in file size and avoids
	// threading state across the package; the cost is one extra walk
	// of the file's decls per top-level message, which is negligible
	// vs. the GoVM compile that follows.
	msgTypes := buildMessageTypeIndex(file.AST)

	for decl := range seq.Values(msg.Body.Decls()) {
		def := decl.AsDef()
		if def.IsZero() {
			continue
		}
		switch def.Classify() {
		case ast.DefKindOption:
			// Message-level: `option (protocheck.message) = {…};`
			opt := def.AsOption().Option
			if isAggregateOption(opt, "protocheck.message") {
				env := newMessageEnv(msg, msgTypes)
				diags = append(diags, validateCheckExpressions(file, opt, env, enc)...)
			}
		case ast.DefKindField:
			// Field-level: `string foo = 1 [(protocheck.field) = {…}];`
			f := def.AsField()
			if f.Options.IsZero() {
				continue
			}
			for opt := range seq.Values(f.Options.Entries()) {
				if !isAggregateOption(opt, "protocheck.field") {
					continue
				}
				env, ok := newFieldEnv(f)
				if !ok {
					// Field type doesn't map to a scalar — no
					// meaningful `this` to type-check against.
					// Skip without diagnostic; the user authored
					// the constraint intentionally and may have
					// custom rules that don't need `this`.
					continue
				}
				diags = append(diags, validateCheckExpressions(file, opt, env, enc)...)
			}
		}
	}
	return diags
}

// buildMessageTypeIndex maps each message name in the file to its
// AST projection so message-level expressions can descend into
// message-typed fields (e.g. `origin.city`) without needing the
// semantic linker. Nested messages declared inside another message
// are also included so a sibling field can reference them.
func buildMessageTypeIndex(file *ast.File) map[string]ast.DefMessage {
	out := make(map[string]ast.DefMessage)
	var walk func(decls seq.Indexer[ast.DeclAny])
	walk = func(decls seq.Indexer[ast.DeclAny]) {
		for decl := range seq.Values(decls) {
			def := decl.AsDef()
			if def.IsZero() || def.Classify() != ast.DefKindMessage {
				continue
			}
			m := def.AsMessage()
			if !m.Name.IsZero() {
				out[m.Name.Text()] = m
			}
			walk(m.Body.Decls())
		}
	}
	walk(file.Decls())
	return out
}

// maxNestedStructDepth caps how deep buildStructDef will recurse
// when expanding nested message types into eval.StructDef values.
// A small bound keeps over-deep schemas from blowing the stack;
// the per-call-path visited map already handles cycles. The cap
// is set to 4 so the canonical protocheck README example
// `order.shipping.address.city` (3 dot hops to a scalar leaf,
// 4 levels of message resolution including the leaf's owning
// type) type-checks cleanly. Beyond that, the user gets a
// silently-pruned StructDef and the expression fails compile
// with the natural "undefined field" diagnostic, which is the
// signal to either shorten the chain or file a follow-up to
// bump this knob.
const maxNestedStructDepth = 4

// scalarFromTypeName maps a protobuf scalar type name to the
// corresponding GoVM eval.Type. Returns (_, false) for any
// non-scalar (message-typed field, enum reference) since the AST
// alone can't distinguish those from user-defined message names.
//
// The mapping MUST mirror protocheck-govm's protoKindToEvalType —
// that engine is what evaluates these expressions at runtime (via
// the protocheck/v2 seam), and a kind the type-checker and the
// engine bind differently would make Phase 0 pass expressions that
// Phase 1 rejects (or vice versa). Unsigned kinds bind as
// eval.Uint there, hence here.
func scalarFromTypeName(name string) (eval.Type, bool) {
	switch name {
	case "int32", "sint32", "sfixed32",
		"int64", "sint64", "sfixed64":
		return eval.Int, true
	case "uint32", "fixed32",
		"uint64", "fixed64":
		return eval.Uint, true
	case "float", "double":
		return eval.Float64, true
	case "bool":
		return eval.Bool, true
	case "string":
		return eval.String, true
	}
	return 0, false
}

// newMessageEnv builds the GoVM env for a message-level expression
// on `msg`. Each scalar field becomes an eval.Variable; each
// message-typed field becomes an eval.StructVariable backed by a
// StructDef built from the referenced type. Cycle / depth control
// lives in buildStructDef.
//
// msgTypes provides the file-level type index so dotted access
// (`origin.city`) can type-check without dragging in semantic
// resolution. Unresolved types (imports, well-known types we
// don't index) are silently skipped — the expression that
// references them will fail compile naturally and surface as a
// regular diagnostic.
func newMessageEnv(msg ast.DefMessage, msgTypes map[string]ast.DefMessage) *eval.Env {
	visited := make(map[string]bool)
	opts := buildEnvOptions(msg, msgTypes, visited, 0)
	return eval.NewEnv(opts...)
}

func buildEnvOptions(msg ast.DefMessage, msgTypes map[string]ast.DefMessage, visited map[string]bool, depth int) []eval.EnvOption {
	var opts []eval.EnvOption
	for decl := range seq.Values(msg.Body.Decls()) {
		def := decl.AsDef()
		if def.IsZero() || def.Classify() != ast.DefKindField {
			continue
		}
		f := def.AsField()
		if protoopts.FieldTypeIsRepeated(f.Type) {
			continue
		}
		fldName := f.Name.Text()
		typeName := protoopts.FieldTypeName(f.Type)
		if t, ok := scalarFromTypeName(typeName); ok {
			opts = append(opts, eval.Variable(fldName, t))
			continue
		}
		// Message-typed field? Look it up in the file's type
		// index; if it resolves, build a StructDef.
		nested, ok := msgTypes[typeName]
		if !ok {
			continue
		}
		sd := buildStructDef(nested, msgTypes, visited, depth+1)
		if sd == nil {
			continue
		}
		opts = append(opts, eval.StructVariable(fldName, sd))
	}
	return opts
}

// buildStructDef recursively constructs an eval.StructDef for a
// message-typed field's nested fields. Returns nil when the
// current type would cycle back into a visited message OR when
// depth would exceed maxNestedStructDepth. Both branches let the
// caller silently skip the variable; the user's expression then
// fails compile with a normal "undeclared name" diagnostic
// pointing at the inaccessible path component.
func buildStructDef(msg ast.DefMessage, msgTypes map[string]ast.DefMessage, visited map[string]bool, depth int) *eval.StructDef {
	name := msg.Name.Text()
	if visited[name] || depth >= maxNestedStructDepth {
		return nil
	}
	visited[name] = true
	defer delete(visited, name)

	sd := &eval.StructDef{Name: name}
	for decl := range seq.Values(msg.Body.Decls()) {
		def := decl.AsDef()
		if def.IsZero() || def.Classify() != ast.DefKindField {
			continue
		}
		f := def.AsField()
		if protoopts.FieldTypeIsRepeated(f.Type) {
			continue
		}
		fldName := f.Name.Text()
		typeName := protoopts.FieldTypeName(f.Type)
		if t, ok := scalarFromTypeName(typeName); ok {
			sd.Fields = append(sd.Fields, eval.StructField{
				Name:       fldName,
				ScalarType: t,
			})
			continue
		}
		nested, ok := msgTypes[typeName]
		if !ok {
			continue
		}
		sub := buildStructDef(nested, msgTypes, visited, depth+1)
		if sub == nil {
			continue
		}
		sd.Fields = append(sd.Fields, eval.StructField{
			Name:       fldName,
			StructType: sub,
		})
	}
	return sd
}

func newFieldEnv(f ast.DefField) (*eval.Env, bool) {
	if protoopts.FieldTypeIsRepeated(f.Type) {
		return nil, false
	}
	t, ok := scalarFromTypeName(protoopts.FieldTypeName(f.Type))
	if !ok {
		return nil, false
	}
	return eval.NewEnv(eval.Variable("this", t)), true
}

// isAggregateOption matches `(<dotted-name>) = { … }` exactly: a
// single-component option name whose parenthesized extension path
// equals the expected dotted name AND whose value is a message
// literal (dict). The traversal forms
// (`(protocheck.field).string = {…}`) and scalar assignments are
// filtered out because they don't carry `check` arrays.
func isAggregateOption(opt ast.Option, dottedName string) bool {
	if opt.Path.IsZero() || opt.Value.IsZero() {
		return false
	}
	var parts []ast.PathComponent
	for pc := range opt.Path.Components() {
		parts = append(parts, pc)
	}
	if len(parts) != 1 {
		return false
	}
	ext := parts[0].AsExtension()
	if ext.IsZero() || ext.Canonicalized() != dottedName {
		return false
	}
	return !opt.Value.AsDict().IsZero()
}

// validateCheckExpressions descends into the option body looking for
// `check: […]` and validates each entry's `expression` string.
// Tolerates the single-entry `check: {…}` shorthand (protobuf text
// format permits singular form for repeated fields).
func validateCheckExpressions(file *protoast.File, opt ast.Option, env *eval.Env, enc positions.Encoding) []protocol.Diagnostic {
	body := opt.Value.AsDict()
	checkField := protoopts.FindMessageField(body, "check")
	if checkField.IsZero() {
		return nil
	}
	v := checkField.Value()
	switch v.Kind() {
	case ast.ExprKindArray:
		var diags []protocol.Diagnostic
		for elem := range seq.Values(v.AsArray().Elements()) {
			lit := elem.AsDict()
			if lit.IsZero() {
				continue
			}
			diags = append(diags, validateSingleExpression(file, lit, env, enc)...)
		}
		return diags
	case ast.ExprKindDict:
		return validateSingleExpression(file, v.AsDict(), env, enc)
	}
	return nil
}

func validateSingleExpression(file *protoast.File, lit ast.ExprDict, env *eval.Env, enc positions.Encoding) []protocol.Diagnostic {
	exprField := protoopts.FindMessageField(lit, "expression")
	if exprField.IsZero() {
		return nil
	}
	expr, anchor, ok := protoopts.ExpressionStringFromValue(exprField.Value())
	if !ok {
		return nil
	}
	if _, err := env.Compile(expr); err != nil {
		return []protocol.Diagnostic{{
			Range:    expressionErrorRange(file, anchor, err, enc),
			Severity: protocol.DiagnosticSeverityError,
			Source:   protocol.NewOptional("protocheck/expression"),
			Message:  protocol.String(err.Error()),
		}}
	}
	return nil
}

// expressionErrorRange computes the precise LSP range for a
// GoVM compile error inside a string literal. It pulls the
// offset+length out of the *eval.CompileError chain (govm v1.1+)
// and maps them through the raw source bytes back to a range.
// Falls back to the whole-token range when:
//
//   - The error doesn't carry a *eval.CompileError (defensive
//     against future error wrapping changes).
//   - The CompileError has Length < 0 — govm's documented
//     sentinel for "no usable position info."
//   - The literal is a concatenation (`"foo" "bar"`). The raw
//     source has multiple separate quoted parts with gaps, so a
//     decoded-string offset doesn't map to a single contiguous
//     range. Per-column inside a concatenation is a bigger
//     refactor; not worth it for the rare case.
//   - The literal contains backslash escapes. Decoded-string
//     positions don't line up 1:1 with raw-source bytes once
//     escapes are present; mapping requires a per-byte walk that
//     isn't worth the LOC for the rare case (real-world GoVM
//     expressions are almost never escape-laden).
//
// All fallbacks land the diagnostic on the entire literal (quotes
// included), which is exactly the pre-v1.1 behavior — no
// regression for cases this can't handle precisely.
func expressionErrorRange(file *protoast.File, tok token.Token, err error, enc positions.Encoding) protocol.Range {
	whole := protoopts.NodeRange(file, tok, enc)

	// Concatenated literals always fall back — see function comment.
	s := tok.AsString()
	if s.IsZero() || s.IsConcatenated() {
		return whole
	}

	var ce *eval.CompileError
	if !errors.As(err, &ce) {
		return whole
	}
	if ce.Length < 0 {
		// govm v1.2's sentinel: underlying error had no position
		// info or pointed outside the expression range (commonly:
		// EOF-style errors at the end of an incomplete expression).
		// Fall back to whole-literal so we don't mislead the user
		// with a precise-but-wrong pointer. Pre-v1.2 govm used
		// `Offset==0 && Length==0` for the same case, which made
		// genuine first-character errors indistinguishable; the
		// negative-Length sentinel disambiguates them.
		return whole
	}
	tokSpan := tok.Span()
	if tokSpan.IsZero() {
		return whole
	}
	if s.HasEscapes() {
		return whole
	}

	// Raw layout: opening-quote + expression-bytes + closing-quote.
	// For non-escaped literals, decoded[i] == raw[i+1] by byte, so
	// the bad token starts at byte (tokSpan.Start + 1 + Offset) and
	// runs for Length bytes (defaulting to 1 if Length is 0).
	length := ce.Length
	if length <= 0 {
		length = 1
	}
	// Clamp so a runaway Offset+Length doesn't escape the literal.
	exprLen := len(s.Text())
	startOff := ce.Offset
	if startOff < 0 {
		startOff = 0
	}
	if startOff > exprLen {
		startOff = exprLen
	}
	endOff := startOff + length
	if endOff > exprLen {
		endOff = exprLen
	}
	start := tokSpan.Start + 1 + startOff
	end := tokSpan.Start + 1 + endOff
	return file.RangeIn(file.Src.Span(start, end), enc)
}
