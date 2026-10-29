//go:build protocheck

package analysis

import (
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

func runProtocheckAnalyzer(t *testing.T, src string) []protocol.Diagnostic {
	t.Helper()
	parsed := protoast.Parse("test.proto", []byte(src))
	require.NotNil(t, parsed, "parse should yield an AST for analyzer tests")
	return analyzeProtocheckExpressions(parsed, positions.UTF16)
}

func TestAnalyzeProtocheckExpressions_FieldLevel_Valid(t *testing.T) {
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  double balance = 1 [(protocheck.field) = {
    check: [{
      id: "non_negative",
      message: "must not be negative",
      expression: "this >= 0.0"
    }]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Empty(t, diags, "valid expression should produce no diagnostics")
}

func TestAnalyzeProtocheckExpressions_FieldLevel_SyntaxError(t *testing.T) {
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  double balance = 1 [(protocheck.field) = {
    check: [{
      id: "syntax_err",
      message: "broken",
      expression: "this >= "
    }]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Equal(t, "protocheck/expression", diags[0].Source)
	require.Equal(t, protocol.DiagnosticSeverityError, diags[0].Severity)
	// Range pins to the expression string literal (8th line, 1-based;
	// LSP line is 7).
	require.Equal(t, uint32(7), diags[0].Range.Start.Line)
}

func TestAnalyzeProtocheckExpressions_FieldLevel_TypeMismatch(t *testing.T) {
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  int32 age = 1 [(protocheck.field) = {
    check: [{
      id: "type_err",
      message: "broken",
      expression: "this == \"foo\""
    }]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Equal(t, "protocheck/expression", diags[0].Source)
	require.Contains(t, strings.ToLower(diags[0].Message), "type")
}

func TestAnalyzeProtocheckExpressions_MessageLevel_Valid(t *testing.T) {
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Transfer {
  string from_account = 1;
  string to_account = 2;
  double amount = 3;

  option (protocheck.message) = {
    check: [{
      id: "different_accounts",
      message: "cannot transfer to the same account",
      expression: "from_account != to_account"
    }]
  };
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Empty(t, diags, "valid message-level expression should pass")
}

func TestAnalyzeProtocheckExpressions_MessageLevel_UndefinedIdentifier(t *testing.T) {
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Transfer {
  string from_account = 1;
  string to_account = 2;

  option (protocheck.message) = {
    check: [{
      id: "typo",
      message: "broken",
      expression: "from_acount != to_account"
    }]
  };
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Equal(t, "protocheck/expression", diags[0].Source)
	// go/types diagnoses an undefined identifier — message may say
	// "undeclared" or "undefined" depending on Go version. Both
	// contain the typo'd name.
	require.Contains(t, diags[0].Message, "from_acount")
}

func TestAnalyzeProtocheckExpressions_MessageLevel_TypeMismatch(t *testing.T) {
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Transfer {
  string from_account = 1;
  double amount = 2;

  option (protocheck.message) = {
    check: [{
      id: "type_err",
      message: "broken",
      expression: "from_account == amount"
    }]
  };
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Equal(t, "protocheck/expression", diags[0].Source)
}

func TestAnalyzeProtocheckExpressions_MultipleChecks(t *testing.T) {
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  int32 age = 1 [(protocheck.field) = {
    check: [
      { id: "ok",  message: "fine",   expression: "this >= 0" },
      { id: "bad", message: "broken", expression: "this >= " }
    ]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1, "only the broken expression should diagnose")
	require.Equal(t, "protocheck/expression", diags[0].Source)
}

func TestAnalyzeProtocheckExpressions_TraversalFormIgnored(t *testing.T) {
	// (protocheck.field).int32 = {…} — traversal form. Carries no
	// `check` array; analyzer should silently skip it.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  int32 age = 1 [(protocheck.field).int32 = { gte: 0, lte: 150 }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Empty(t, diags)
}

func TestAnalyzeProtocheckExpressions_NoProtocheck(t *testing.T) {
	// File with no protocheck options at all — analyzer should
	// produce nothing, not error.
	src := `syntax = "proto3";
message Plain {
  string name = 1;
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Empty(t, diags)
}

func TestAnalyzeProtocheckExpressions_NilAST(t *testing.T) {
	require.Nil(t, analyzeProtocheckExpressions(nil, positions.UTF16))
}

func TestAnalyzeProtocheckExpressions_NonScalarFieldSkipped(t *testing.T) {
	// Message-typed field can't be type-checked at the AST level
	// because we don't resolve type names. The analyzer should
	// silently skip the (protocheck.field) annotation without
	// crashing.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Inner { string name = 1; }
message Outer {
  Inner inner = 1 [(protocheck.field) = {
    check: [{ id: "x", message: "x", expression: "this >= 0" }]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Empty(t, diags, "non-scalar field type → no env → silently skipped")
}

func TestAnalyzeProtocheckExpressions_RepeatedFieldSkipped(t *testing.T) {
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Cart {
  repeated string tags = 1 [(protocheck.field) = {
    check: [{ id: "x", message: "x", expression: "this >= 0" }]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Empty(t, diags, "repeated field → no scalar `this` → silently skipped")
}

// ---------------------------------------------------------------------------
// Phase 0.5: nested-message field access in the message-level env
// ---------------------------------------------------------------------------

func TestAnalyzeProtocheckExpressions_NestedMessageAccess_Valid(t *testing.T) {
	// origin.city — nested-message dot access in a message-level
	// expression. Requires the env to register `origin` and
	// `destination` as StructVariables with `city` as a scalar
	// child. Pre-Phase 0.5 this would fail with "undeclared
	// name: origin".
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Location {
  string city = 1;
  string country = 2;
}
message Shipment {
  Location origin = 1;
  Location destination = 2;
  double weight_kg = 3;

  option (protocheck.message) = {
    check: [{
      id: "different_cities",
      message: "origin and destination must differ",
      expression: "origin.city != destination.city"
    }]
  };
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Empty(t, diags, "nested-message dot access should type-check")
}

func TestAnalyzeProtocheckExpressions_NestedMessageAccess_TypoOnNestedField(t *testing.T) {
	// `origin.citty` — typo on the nested field. The StructDef
	// knows `city`/`country` and not `citty`, so go/types
	// catches the bad reference at compile time.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Location {
  string city = 1;
}
message Shipment {
  Location origin = 1;
  Location destination = 2;

  option (protocheck.message) = {
    check: [{
      id: "typo",
      message: "typo on nested",
      expression: "origin.citty != destination.city"
    }]
  };
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Contains(t, diags[0].Message, "citty")
}

func TestAnalyzeProtocheckExpressions_NestedMessageAccess_DeepChain(t *testing.T) {
	// `order.shipping.address.city` — 3 hops, at the
	// maxNestedStructDepth boundary. Should type-check.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Address  { string city = 1; }
message Shipping { Address address = 1; }
message Order    { Shipping shipping = 1; string buyer = 2; }
message Receipt {
  Order order = 1;
  string biller = 2;

  option (protocheck.message) = {
    check: [{
      id: "ship_city_matches_biller",
      message: "shipping city must equal biller",
      expression: "order.shipping.address.city == biller"
    }]
  };
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Empty(t, diags, "3-hop nested access should type-check at the depth boundary")
}

func TestAnalyzeProtocheckExpressions_NestedMessageAccess_Cycle(t *testing.T) {
	// Self-referential message (tree). The cycle guard in
	// buildStructDef should stop recursion before stack-overflow
	// AND allow one hop into the scalar `value` field — further
	// nesting cycles back and is pruned silently.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Tree {
  Tree left = 1;
  Tree right = 2;
  int32 value = 3;
}
message Root {
  Tree tree = 1;

  option (protocheck.message) = {
    check: [{
      id: "ok_value",
      message: "value check",
      expression: "tree.value >= 0"
    }]
  };
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Empty(t, diags)
}

func TestAnalyzeProtocheckExpressions_NestedMessageAccess_UnresolvedTypeSkipped(t *testing.T) {
	// `origin.city` where Location isn't declared in the file —
	// the type isn't in the index, so origin doesn't get a
	// StructVariable, and the expression fails to compile against
	// an empty env (only the local scalar `biller` is registered).
	// The user sees the diagnostic at the `origin` reference site.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Shipment {
  string biller = 1;
  Location origin = 2;

  option (protocheck.message) = {
    check: [{
      id: "x",
      message: "x",
      expression: "origin.city == biller"
    }]
  };
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Contains(t, diags[0].Message, "origin")
}

// ---------------------------------------------------------------------------
// PR B: per-column diagnostic ranges via *eval.CompileError.Offset
// ---------------------------------------------------------------------------

// rangeInside reports whether the diagnostic range is strictly
// *inside* the quoted literal — start is past the opening quote
// AND end is at or before the closing quote. Used to assert the
// precise-range path actually narrowed the diagnostic.
func rangeInside(t *testing.T, src string, d protocol.Diagnostic, line int, exprText string) {
	t.Helper()
	srcLine := strings.Split(src, "\n")[line]
	quoteOpen := strings.Index(srcLine, `"`)
	require.NotEqual(t, -1, quoteOpen, "test fixture must contain a quoted literal on the asserted line")
	quoteClose := quoteOpen + len(exprText) + 1
	require.Greater(t, int(d.Range.Start.Character), quoteOpen,
		"diagnostic start must be past the opening quote (precise range, not whole-literal)")
	require.LessOrEqual(t, int(d.Range.End.Character), quoteClose+1,
		"diagnostic end must not exceed the closing quote")
}

func TestAnalyzeProtocheckExpressions_PerColumn_TypeError(t *testing.T) {
	// `this.field` against a float64 `this` — Go's typechecker
	// reports a selector-on-non-struct error at the `.` (offset 4
	// inside the expression). Our code narrows the range past the
	// opening quote to that column.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  double balance = 1 [(protocheck.field) = {
    check: [{
      id: "bad_selector",
      message: "broken",
      expression: "this.field > 0.0"
    }]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Equal(t, uint32(7), diags[0].Range.Start.Line)
	rangeInside(t, src, diags[0], 7, `this.field > 0.0`)
}

func TestAnalyzeProtocheckExpressions_PerColumn_FirstCharError(t *testing.T) {
	// `from_acount != to_account` — typo'd identifier at offset 0
	// of the expression. Pre-govm-v1.2 this was indistinguishable
	// from EOF errors (both reported Offset=0 Length=0) and fell
	// back to whole-literal. v1.2's Length<0 sentinel
	// disambiguates them, so first-character errors now narrow
	// precisely to the start of the expression.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Transfer {
  string from_account = 1;
  string to_account = 2;

  option (protocheck.message) = {
    check: [{
      id: "typo",
      message: "broken",
      expression: "from_acount != to_account"
    }]
  };
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	srcLine := strings.Split(src, "\n")[10]
	openQuote := strings.Index(srcLine, `"from_acount`)
	require.Greater(t, int(diags[0].Range.Start.Character), openQuote,
		"first-character error should narrow past the opening quote now that "+
			"govm v1.2 distinguishes it from the EOF no-info case")
}

func TestAnalyzeProtocheckExpressions_PerColumn_SyntaxError_TrailingFallback(t *testing.T) {
	// `this >= ` — EOF-style trailing-operator error. govm v1.2
	// reports Length<0 here (no usable position info), which our
	// code maps to a whole-literal fallback. This test locks in
	// the fallback path so it doesn't silently flip when govm's
	// sentinel evolves.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  double balance = 1 [(protocheck.field) = {
    check: [{
      id: "syntax_err",
      message: "broken",
      expression: "this >= "
    }]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	srcLine := strings.Split(src, "\n")[7]
	openQuote := strings.Index(srcLine, `"this`)
	require.Equal(t, uint32(openQuote), diags[0].Range.Start.Character,
		"EOF-style error → whole-literal fallback anchors at opening quote")
}

func TestAnalyzeProtocheckExpressions_PerColumn_TypeError_Escapes(t *testing.T) {
	// `this == "foo"` against `int32 this` — type error inside a
	// literal that *contains backslash escapes* (\"foo\" in the
	// .proto source). The escape-aware fallback triggers, range
	// pins to the opening quote of the whole literal.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  int32 age = 1 [(protocheck.field) = {
    check: [{
      id: "type_err",
      message: "broken",
      expression: "this == \"foo\""
    }]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	srcLine := strings.Split(src, "\n")[7]
	openQuote := strings.Index(srcLine, `"this`)
	require.Equal(t, uint32(openQuote), diags[0].Range.Start.Character,
		"backslash escapes in raw literal → whole-literal fallback")
}

func TestAnalyzeProtocheckExpressions_CompoundLiteral_Valid(t *testing.T) {
	// `"this " ">= 0.0"` — two adjacent string literals
	// concatenated by the parser into "this >= 0.0". Phase 0
	// should accept the compound and type-check the concatenated
	// expression.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  double balance = 1 [(protocheck.field) = {
    check: [{
      id: "ok",
      message: "ok",
      expression: "this " ">= 0.0"
    }]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Empty(t, diags, "compound literal should join into a valid expression")
}

func TestAnalyzeProtocheckExpressions_CompoundLiteral_TypeError(t *testing.T) {
	// Concatenated `"this" " == " "\"foo\""` joins to
	// `this == "foo"` which type-errors against int32 this.
	// Phase 0 should fire the diagnostic; the range falls back to
	// the whole compound (per the documented limitation —
	// per-column inside a compound needs a bigger refactor).
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  int32 age = 1 [(protocheck.field) = {
    check: [{
      id: "type_err",
      message: "broken",
      expression: "this" " == \"foo\""
    }]
  }];
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Equal(t, "protocheck/expression", diags[0].Source)
}

func TestAnalyzeProtocheckExpressions_PerColumn_UndefinedIdent(t *testing.T) {
	// `to_accont` is at offset 16 inside the expression (`!=` at
	// offset 13, space at 15, the typo'd ident at 16). The
	// precise-range path narrows the diagnostic to the typo.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Transfer {
  string from_account = 1;
  string to_account = 2;

  option (protocheck.message) = {
    check: [{
      id: "typo",
      message: "broken",
      expression: "from_account != to_accont"
    }]
  };
}`
	diags := runProtocheckAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Equal(t, uint32(10), diags[0].Range.Start.Line)
	rangeInside(t, src, diags[0], 10, `from_account != to_accont`)
}

func TestAnalyzeProtocheckExpressions_NonASCIIPrefix_EncodingAware(t *testing.T) {
	// A block-comment emoji BEFORE the expression on the same
	// line makes the literal's byte column and UTF-16 column differ
	// (emoji = 4 bytes, 2 code units). The concatenated literal
	// forces the whole-token fallback, whose start is exactly the
	// opening quote — computable in both encodings from the source.
	src := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  double balance = 1 [(protocheck.field) = {
    check: [{
      id: "enc",
      message: "broken",
      /*😀*/ expression: "this " ">= "
    }]
  }];
}`
	line := strings.Split(src, "\n")[7]
	byteCol := strings.Index(line, `"this`)
	utf16Col := len(utf16.Encode([]rune(line[:byteCol])))
	require.NotEqual(t, byteCol, utf16Col,
		"fixture must place non-ASCII before the literal")

	parsed := protoast.Parse("test.proto", []byte(src))
	require.NotNil(t, parsed)

	utf8Diags := analyzeProtocheckExpressions(parsed, positions.UTF8)
	require.Len(t, utf8Diags, 1)
	require.Equal(t, uint32(byteCol), utf8Diags[0].Range.Start.Character, //nolint:gosec
		"utf-8: diagnostic column counts bytes")

	utf16Diags := analyzeProtocheckExpressions(parsed, positions.UTF16)
	require.Len(t, utf16Diags, 1)
	require.Equal(t, uint32(utf16Col), utf16Diags[0].Range.Start.Character, //nolint:gosec
		"utf-16: diagnostic column counts code units")
}

// Unsigned kinds bind as eval.Uint — mirroring protocheck-govm's
// protoKindToEvalType, the engine that evaluates these expressions
// at runtime. A uint-flavored comparison must type-check, and an
// operation govm rejects for uint (comparison against a negative
// int literal) must surface a diagnostic instead of silently
// passing Phase 0 and then failing (or misbehaving) in Phase 1.
func TestAnalyzeProtocheckExpressions_FieldLevel_UintBinding(t *testing.T) {
	valid := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  uint64 quota = 1 [(protocheck.field) = {
    check: [{
      id: "quota_max",
      message: "quota too large",
      expression: "this <= 1000"
    }]
  }];
}`
	require.Empty(t, runProtocheckAnalyzer(t, valid),
		"uint comparison against a uint literal type-checks")

	mismatch := `syntax = "proto3";
import "protocheck/protocheck.proto";
message Account {
  uint64 quota = 1 [(protocheck.field) = {
    check: [{
      id: "quota_min",
      message: "broken",
      expression: "this >= -1"
    }]
  }];
}`
	diags := runProtocheckAnalyzer(t, mismatch)
	require.Len(t, diags, 1,
		"uint compared against a negative int literal is a type error under the eval.Uint binding")
	require.Equal(t, "protocheck/expression", diags[0].Source)
}
