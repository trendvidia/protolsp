package analysis

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// parsePxf is a thin helper around pxf.Parse for the resolver tests.
// Failing parses abort the test — every fixture in this file is
// hand-authored and expected to be well-formed.
func parsePxf(t *testing.T, src string) *pxf.Document {
	t.Helper()
	doc, err := pxf.Parse([]byte(src))
	require.NoError(t, err)
	return doc
}

func TestSplitFieldPath(t *testing.T) {
	cases := map[string][]pathSegment{
		"":                     nil,
		"email":                {{Name: "email"}},
		"address.city":         {{Name: "address"}, {Name: "city"}},
		"items[3]":             {{Name: "items", HasIndex: true, Index: 3}},
		"items[3].sku":         {{Name: "items", HasIndex: true, Index: 3}, {Name: "sku"}},
		`labels["env"]`:        {{Name: "labels", HasKey: true, Key: "env"}},
		`labels["env"].value`:  {{Name: "labels", HasKey: true, Key: "env"}, {Name: "value"}},
		"a.b.c.d":              {{Name: "a"}, {Name: "b"}, {Name: "c"}, {Name: "d"}},
		"items[0].tags[1]":     {{Name: "items", HasIndex: true, Index: 0}, {Name: "tags", HasIndex: true, Index: 1}},
		"items[0].address.zip": {{Name: "items", HasIndex: true, Index: 0}, {Name: "address"}, {Name: "zip"}},
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			got := splitFieldPath(input)
			require.Equal(t, want, got)
		})
	}
}

func TestResolveProtocheckPath_TopLevelScalar(t *testing.T) {
	doc := parsePxf(t, `email = "bad"`)
	pos, ok := resolveProtocheckPath(doc.Entries, "email")
	require.True(t, ok)
	require.Equal(t, 1, pos.Line)
}

func TestResolveProtocheckPath_NestedBlock(t *testing.T) {
	src := `name = "alice"
address {
  city = "Mountain View"
  zip = "94043"
}`
	doc := parsePxf(t, src)
	pos, ok := resolveProtocheckPath(doc.Entries, "address.zip")
	require.True(t, ok)
	// `zip` lives on line 4 (1-based).
	require.Equal(t, 4, pos.Line)
}

func TestResolveProtocheckPath_NestedAssignmentBlock(t *testing.T) {
	// `address = { … }` — inline block-value form on an
	// Assignment, vs. the `address { … }` nested-message Block
	// form covered by the previous test.
	src := `address = {
  city = "Mountain View"
  zip = "94043"
}`
	doc := parsePxf(t, src)
	pos, ok := resolveProtocheckPath(doc.Entries, "address.zip")
	require.True(t, ok)
	require.Equal(t, 3, pos.Line)
}

func TestResolveProtocheckPath_ListIndex(t *testing.T) {
	src := `items = [
  { sku = "A" }
  { sku = "B" }
  { sku = "C" }
]`
	doc := parsePxf(t, src)
	pos, ok := resolveProtocheckPath(doc.Entries, "items[1].sku")
	require.True(t, ok)
	// `sku = "B"` is on line 3.
	require.Equal(t, 3, pos.Line)
}

func TestResolveProtocheckPath_ListIndexOutOfRange(t *testing.T) {
	src := `items = [{ sku = "A" }]`
	pos, ok := resolveProtocheckPath(parsePxf(t, src).Entries, "items[5].sku")
	// The resolver returns the parent position (the `items` line)
	// when the index is out of range — visible to the user, not
	// a misleading origin pin.
	require.True(t, ok)
	require.Equal(t, 1, pos.Line)
}

func TestResolveProtocheckPath_UnknownField(t *testing.T) {
	doc := parsePxf(t, `email = "x"`)
	pos, ok := resolveProtocheckPath(doc.Entries, "nonexistent")
	require.False(t, ok)
	require.Equal(t, pxf.Position{Line: 1, Column: 1}, pos)
}

func TestResolveProtocheckPath_EmptyPath(t *testing.T) {
	doc := parsePxf(t, `email = "x"`)
	pos, ok := resolveProtocheckPath(doc.Entries, "")
	require.False(t, ok)
	require.Equal(t, pxf.Position{Line: 1, Column: 1}, pos)
}

func TestResolveProtocheckPath_MapKey_Value(t *testing.T) {
	// `labels["env"].value` — scalar map entry. The resolver lands
	// at the value side of the MapEntry; the value `"prod"` lives
	// on the same line as the key.
	src := `labels = {
  env: "prod"
  team: "platform"
}`
	doc := parsePxf(t, src)
	pos, ok := resolveProtocheckPath(doc.Entries, `labels["env"].value`)
	require.True(t, ok)
	require.Equal(t, 2, pos.Line)
}

func TestResolveProtocheckPath_MapKey_KeyAnchor(t *testing.T) {
	// `labels["team"].key` — anchors at the MapEntry itself,
	// which starts at the key token.
	src := `labels = {
  env: "prod"
  team: "platform"
}`
	doc := parsePxf(t, src)
	pos, ok := resolveProtocheckPath(doc.Entries, `labels["team"].key`)
	require.True(t, ok)
	require.Equal(t, 3, pos.Line)
}

func TestResolveProtocheckPath_MapKey_NestedMessageValue(t *testing.T) {
	// `nested_map["alice"].value.name` — message-typed map value.
	// The MapEntry's Value is itself a BlockVal we descend into.
	src := `nested_map = {
  alice: { name: "Alice" }
  bob:   { name: "Bob" }
}`
	doc := parsePxf(t, src)
	pos, ok := resolveProtocheckPath(doc.Entries, `nested_map["bob"].value.name`)
	require.True(t, ok)
	require.Equal(t, 3, pos.Line)
}

func TestResolveProtocheckPath_MapKey_UnknownKey(t *testing.T) {
	// `labels["nope"]` — key isn't present. The resolver returns
	// the parent assignment's position so the diagnostic still
	// lands somewhere relevant, not at the document origin.
	src := `labels = {
  env: "prod"
}`
	doc := parsePxf(t, src)
	pos, ok := resolveProtocheckPath(doc.Entries, `labels["nope"].value`)
	require.True(t, ok)
	require.Equal(t, 1, pos.Line)
}

func TestProtocheckDataDiagnostics_EmptyFindings(t *testing.T) {
	doc := &Document{}
	require.Nil(t, protocheckDataDiagnostics(doc))
}

func TestProtocheckDataDiagnostics_RendersFindings(t *testing.T) {
	doc := &Document{
		protocheckFindings: []protocheckFinding{
			{
				FieldPath:    "age",
				ConstraintID: "int32.lte",
				Message:      "value must be at most 150",
				Pos:          pxf.Position{Line: 5, Column: 3},
			},
			{
				FieldPath:    "email",
				ConstraintID: "string.email",
				Message:      "value must be a valid email",
				Pos:          pxf.Position{Line: 7, Column: 1},
			},
		},
	}
	diags := protocheckDataDiagnostics(doc)
	require.Len(t, diags, 2)
	for _, d := range diags {
		require.Equal(t, "protocheck", d.Source)
		require.Equal(t, protocol.DiagnosticSeverityError, d.Severity)
	}
	require.Equal(t, "int32.lte", diags[0].Code)
	require.Equal(t, "string.email", diags[1].Code)
}

func TestProtocheckDataDiagnostics_ForKeySuffix(t *testing.T) {
	doc := &Document{
		protocheckFindings: []protocheckFinding{{
			FieldPath:    "labels[env]",
			ConstraintID: "string.min_len",
			Message:      "value length must be at least 3",
			ForKey:       true,
			Pos:          pxf.Position{Line: 2, Column: 1},
		}},
	}
	diags := protocheckDataDiagnostics(doc)
	require.Len(t, diags, 1)
	require.Equal(t, "value length must be at least 3 (key)", diags[0].Message,
		"map-key violations render the same (key) suffix as Report.Format and the runtime overlay")
	require.Equal(t, protocol.DiagnosticSeverityError, diags[0].Severity)
}

func TestProtocheckDataDiagnostics_UnimplementedFunctionIsInformation(t *testing.T) {
	doc := &Document{
		protocheckFindings: []protocheckFinding{{
			FieldPath:    "name",
			ConstraintID: protocheckCodeFunctionUnimplemented,
			Message:      "protolsp.test.fnlib.is_special: not implemented",
			Pos:          pxf.Position{Line: 2, Column: 1},
		}},
	}
	diags := protocheckDataDiagnostics(doc)
	require.Len(t, diags, 1)
	require.Equal(t, protocol.DiagnosticSeverityInformation, diags[0].Severity,
		"a declared-but-unimplemented function isn't a data error — the LSP just can't evaluate the rule")
}

func TestProtocheckDataDiagnostics_RuleRelatedInformation(t *testing.T) {
	loc := protocol.Location{
		URI: "file:///work/proto/fnlib.proto",
		Range: protocol.Range{
			Start: protocol.Position{Line: 10, Character: 4},
			End:   protocol.Position{Line: 10, Character: 4},
		},
	}
	doc := &Document{
		protocheckFindings: []protocheckFinding{
			{
				FieldPath:    "name",
				ConstraintID: "gadget.special",
				Message:      "rule failed",
				Pos:          pxf.Position{Line: 2, Column: 1},
				Rule:         &loc,
			},
			{
				FieldPath:    "age",
				ConstraintID: "int32.lte",
				Message:      "value must be at most 150",
				Pos:          pxf.Position{Line: 3, Column: 1},
				// No Rule — protocheckpb Check rules carry no source info.
			},
		},
	}
	diags := protocheckDataDiagnostics(doc)
	require.Len(t, diags, 2)
	require.Len(t, diags[0].RelatedInformation, 1,
		"a finding with a resolved rule location links back to the schema")
	require.Equal(t, loc, diags[0].RelatedInformation[0].Location)
	require.Equal(t, "constraint declared here", diags[0].RelatedInformation[0].Message)
	require.Empty(t, diags[1].RelatedInformation,
		"findings without rule enrichment render without a link")
}
