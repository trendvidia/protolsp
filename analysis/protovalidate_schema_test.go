package analysis

import (
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// runProtovalidateAnalyzer parses src and returns the diagnostics
// the schema-side CEL analyzer emits. Mirror of the protocheck
// test helper.
func runProtovalidateAnalyzer(t *testing.T, src string) []protocol.Diagnostic {
	t.Helper()
	parsed := protoast.Parse("test.proto", []byte(src))
	require.NotNil(t, parsed, "parse should yield an AST for analyzer tests")
	return analyzeProtovalidateExpressions(parsed, positions.UTF16)
}

func TestAnalyzeProtovalidateExpressions_FieldLevel_Valid(t *testing.T) {
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Account {
  double balance = 1 [(buf.validate.field).cel = {
    id: "non_negative",
    message: "must not be negative",
    expression: "this >= 0.0"
  }];
}`
	diags := runProtovalidateAnalyzer(t, src)
	require.Empty(t, diags, "valid CEL expression should produce no diagnostics")
}

func TestAnalyzeProtovalidateExpressions_FieldLevel_SyntaxError(t *testing.T) {
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Account {
  double balance = 1 [(buf.validate.field).cel = {
    id: "syntax_err",
    message: "broken",
    expression: "this >= "
  }];
}`
	diags := runProtovalidateAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Equal(t, "buf/validate/cel", optText(diags[0].Source))
	require.Equal(t, protocol.DiagnosticSeverityError, diags[0].Severity)
}

func TestAnalyzeProtovalidateExpressions_FieldLevel_TypeMismatch(t *testing.T) {
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Account {
  int32 age = 1 [(buf.validate.field).cel = {
    id: "type_err",
    message: "broken",
    expression: "this == \"foo\""
  }];
}`
	diags := runProtovalidateAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Equal(t, "buf/validate/cel", optText(diags[0].Source))
}

func TestAnalyzeProtovalidateExpressions_MessageLevel_Valid(t *testing.T) {
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Transfer {
  string from_account = 1;
  string to_account = 2;
  double amount = 3;

  option (buf.validate.message).cel = {
    id: "different_accounts",
    message: "cannot transfer to the same account",
    expression: "this.from_account != this.to_account"
  };
}`
	diags := runProtovalidateAnalyzer(t, src)
	require.Empty(t, diags, "valid message-level CEL should pass")
}

func TestAnalyzeProtovalidateExpressions_AggregateForm_MultipleCel(t *testing.T) {
	// Aggregate form with multiple CEL rules in the array. First
	// is valid; second has a syntax error. Exactly one
	// diagnostic.
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Account {
  int32 age = 1 [(buf.validate.field) = {
    cel: [
      { id: "ok",  message: "ok",     expression: "this >= 0" },
      { id: "bad", message: "broken", expression: "this >= " }
    ]
  }];
}`
	diags := runProtovalidateAnalyzer(t, src)
	require.Len(t, diags, 1)
	require.Equal(t, "buf/validate/cel", optText(diags[0].Source))
}

func TestAnalyzeProtovalidateExpressions_AggregateForm(t *testing.T) {
	// `(buf.validate.field) = { cel: [{…}] }` — aggregate form.
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Account {
  int32 age = 1 [(buf.validate.field) = {
    cel: [{ id: "ok", message: "ok", expression: "this >= 0" }]
  }];
}`
	diags := runProtovalidateAnalyzer(t, src)
	require.Empty(t, diags, "aggregate form with valid CEL should pass")
}

func TestAnalyzeProtovalidateExpressions_NonCelOptionIgnored(t *testing.T) {
	// `(buf.validate.field).int32 = { lte: 150 }` — declarative
	// rule, no CEL. Analyzer should skip silently.
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Account {
  int32 age = 1 [(buf.validate.field).int32 = { gte: 0, lte: 150 }];
}`
	diags := runProtovalidateAnalyzer(t, src)
	require.Empty(t, diags)
}

func TestAnalyzeProtovalidateExpressions_NoProtovalidate(t *testing.T) {
	src := `syntax = "proto3";
message Plain {
  string name = 1;
}`
	diags := runProtovalidateAnalyzer(t, src)
	require.Empty(t, diags)
}

func TestAnalyzeProtovalidateExpressions_NilAST(t *testing.T) {
	require.Nil(t, analyzeProtovalidateExpressions(nil, positions.UTF16))
}

func TestAnalyzeProtovalidateExpressions_PerColumn_PrecisePosition(t *testing.T) {
	// `this.field` against a float64 `this` — cel-go reports the
	// selector failure at a column inside the expression. Range
	// should narrow past the opening quote.
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Account {
  double balance = 1 [(buf.validate.field).cel = {
    id: "bad_selector",
    message: "broken",
    expression: "this.field > 0.0"
  }];
}`
	diags := runProtovalidateAnalyzer(t, src)
	require.Len(t, diags, 1)
	srcLine := strings.Split(src, "\n")[6]
	openQuote := strings.Index(srcLine, `"this`)
	require.Greater(t, int(diags[0].Range.Start.Character), openQuote,
		"diagnostic start should be past the opening quote")
}

func TestAnalyzeProtovalidateExpressions_NonASCIIPrefix_EncodingAware(t *testing.T) {
	// A block-comment emoji BEFORE the expression on the same
	// line makes the literal's byte column and UTF-16 column differ
	// (emoji = 4 bytes, 2 code units). The concatenated literal
	// forces the whole-token fallback, whose start is exactly the
	// opening quote — computable in both encodings from the source.
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Account {
  double balance = 1 [(buf.validate.field).cel = {
    id: "enc",
    message: "broken",
    /*😀*/ expression: "this " ">= "
  }];
}`
	line := strings.Split(src, "\n")[6]
	byteCol := strings.Index(line, `"this`)
	utf16Col := len(utf16.Encode([]rune(line[:byteCol])))
	require.NotEqual(t, byteCol, utf16Col,
		"fixture must place non-ASCII before the literal")

	parsed := protoast.Parse("test.proto", []byte(src))
	require.NotNil(t, parsed)

	utf8Diags := analyzeProtovalidateExpressions(parsed, positions.UTF8)
	require.Len(t, utf8Diags, 1)
	require.Equal(t, uint32(byteCol), utf8Diags[0].Range.Start.Character, //nolint:gosec
		"utf-8: diagnostic column counts bytes")

	utf16Diags := analyzeProtovalidateExpressions(parsed, positions.UTF16)
	require.Len(t, utf16Diags, 1)
	require.Equal(t, uint32(utf16Col), utf16Diags[0].Range.Start.Character, //nolint:gosec
		"utf-16: diagnostic column counts code units")
}

func TestAnalyzeProtovalidateExpressions_PerColumn_NonASCIIPrefix(t *testing.T) {
	// Precise-range path: the narrowed per-column range (past
	// the opening quote, at the failing selector) must also convert
	// through the negotiated encoding. The two encodings' start
	// columns differ by exactly the emoji's width difference.
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Account {
  double balance = 1 [(buf.validate.field).cel = {
    id: "bad_selector",
    message: "broken",
    /*😀*/ expression: "this.field > 0.0"
  }];
}`
	line := strings.Split(src, "\n")[6]
	byteCol := strings.Index(line, `"this`)
	utf16Col := len(utf16.Encode([]rune(line[:byteCol])))

	parsed := protoast.Parse("test.proto", []byte(src))
	require.NotNil(t, parsed)

	utf8Diags := analyzeProtovalidateExpressions(parsed, positions.UTF8)
	require.Len(t, utf8Diags, 1)
	utf16Diags := analyzeProtovalidateExpressions(parsed, positions.UTF16)
	require.Len(t, utf16Diags, 1)

	require.Greater(t, int(utf8Diags[0].Range.Start.Character), byteCol,
		"utf-8 range narrows past the opening quote")
	require.Greater(t, int(utf16Diags[0].Range.Start.Character), utf16Col,
		"utf-16 range narrows past the opening quote")
	require.Equal(t, byteCol-utf16Col,
		int(utf8Diags[0].Range.Start.Character)-int(utf16Diags[0].Range.Start.Character),
		"the encodings differ by exactly the emoji's width difference")
}

// Rules compile against protovalidate's own library, so what
// the runtime accepts is silent here and what it rejects is
// reported — pinned in both directions. Expressions are wrapped in
// single-quoted proto literals so CEL's double quotes need no
// escaping.
func TestAnalyzeProtovalidateExpressions_ProtovalidateLibrary(t *testing.T) {
	field := func(typ, expr string) string {
		return `syntax = "proto3";
import "buf/validate/validate.proto";
import "google/protobuf/timestamp.proto";
message A {
  ` + typ + ` v = 1 [(buf.validate.field).cel = { id: "r", message: "m", expression: '` + expr + `' }];
}`
	}
	t.Run("protovalidate functions are silent", func(t *testing.T) {
		for _, tc := range []struct{ typ, expr string }{
			{"string", `this.isEmail()`},
			{"string", `this.isHostname() && this.isUri() && this.isIp()`},
			{"string", `this.lowerAscii() == "a"`},
			{"string", `this.format(["a"]) != ""`},
			{"bytes", `this.contains(b"x")`},
			{"google.protobuf.Timestamp", `this < now`},
			{"double", `!this.isNan() && !this.isInf()`},
		} {
			assert.Empty(t, runProtovalidateAnalyzer(t, field(tc.typ, tc.expr)),
				"%s: %s is accepted by protovalidate's runtime", tc.typ, tc.expr)
		}
	})
	t.Run("what protovalidate rejects is reported", func(t *testing.T) {
		for _, tc := range []struct{ expr, msg string }{
			{`this.isEmial()`, "undeclared reference to 'isEmial'"},
			{`math.greatest(1, 2) > 0`, "undeclared reference to 'math'"},
			{`this.?foo.orValue(1) == 1`, "unsupported syntax '.?'"},
		} {
			// cel-go reports a qualified callee once per segment
			// (`math`, then `greatest`); every error is reported
			// verbatim, so assert on the first.
			diags := runProtovalidateAnalyzer(t, field("string", tc.expr))
			require.NotEmpty(t, diags, tc.expr)
			assert.Contains(t, MessageText(diags[0].Message), tc.msg)
			assert.Equal(t, "buf/validate/cel", optText(diags[0].Source))
		}
	})
}

func TestAnalyzeProtovalidateExpressions_CompoundLiteral(t *testing.T) {
	// `"this " ">= 0.0"` — compound literal joined by the parser.
	// The analyzer should still type-check the concatenated
	// expression.
	src := `syntax = "proto3";
import "buf/validate/validate.proto";
message Account {
  double balance = 1 [(buf.validate.field).cel = {
    id: "ok",
    message: "ok",
    expression: "this " ">= 0.0"
  }];
}`
	diags := runProtovalidateAnalyzer(t, src)
	require.Empty(t, diags, "compound literal should join into a valid CEL expression")
}
