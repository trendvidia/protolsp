package chain

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// parseSpec is a test-side helper that runs pxf.Parse and fails
// the test on any parse error. Returns the AST so individual cases
// can stage their starting point inline.
func parseSpec(t *testing.T, src string) *pxf.Document {
	t.Helper()
	doc, err := pxf.Parse([]byte(src))
	require.NoError(t, err)
	return doc
}

// roundtripped formats the AST + reparses + finds the named env's
// key_name. Returns "" when the assignment is absent (the canonical
// "inherit" representation). Used to assert what the edit actually
// did to the on-wire bytes, not just to the in-memory AST.
func roundtripped(t *testing.T, doc *pxf.Document, env string) string {
	t.Helper()
	reparsed, err := pxf.Parse(pxf.FormatDocument(doc))
	require.NoError(t, err)
	envsList, err := findEnvironmentsList(reparsed)
	require.NoError(t, err)
	bv, err := findEnvironmentBlock(envsList, env)
	if errors.Is(err, ErrEnvironmentNotFound) {
		t.Fatalf("env %q disappeared after roundtrip", env)
	}
	require.NoError(t, err)
	for _, entry := range bv.Entries {
		assign, ok := entry.(*pxf.Assignment)
		if !ok || assign.Key != "key_name" {
			continue
		}
		s, ok := stringValue(assign.Value)
		if ok {
			return s
		}
	}
	return ""
}

const simpleSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "sandbox" extends = "prod" key_name = "sandbox-key" }
  { name = "dev"     extends = "sandbox" }
]

layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`

func TestSetEnvironmentKeyName_AddsFieldWhenAbsent(t *testing.T) {
	doc := parseSpec(t, simpleSpec)
	require.NoError(t, SetEnvironmentKeyName(doc, "prod", "prod-key"))
	assert.Equal(t, "prod-key", roundtripped(t, doc, "prod"),
		"new key_name appears on prod after the edit")
}

func TestSetEnvironmentKeyName_ReplacesFieldWhenPresent(t *testing.T) {
	doc := parseSpec(t, simpleSpec)
	require.NoError(t, SetEnvironmentKeyName(doc, "sandbox", "different-key"))
	assert.Equal(t, "different-key", roundtripped(t, doc, "sandbox"))
}

func TestSetEnvironmentKeyName_EmptyClearsExistingField(t *testing.T) {
	// Empty value means "inherit"; the canonical representation is
	// to drop the assignment entirely so the round-tripped output
	// matches what a hand-authored chain.pxf would look like for an
	// env that simply omits the field.
	doc := parseSpec(t, simpleSpec)
	require.NoError(t, SetEnvironmentKeyName(doc, "sandbox", ""))
	assert.Equal(t, "", roundtripped(t, doc, "sandbox"))
	// And the output bytes must not contain `key_name = ""` —
	// pinning that we DROP rather than emit-empty.
	out := string(pxf.FormatDocument(doc))
	assert.NotContains(t, out, `key_name = ""`,
		"clear writes no key_name assignment at all")
}

func TestSetEnvironmentKeyName_EmptyOnAbsentFieldIsNoOp(t *testing.T) {
	// prod has no key_name; clearing it must succeed without
	// mutating the document.
	doc := parseSpec(t, simpleSpec)
	before := string(pxf.FormatDocument(doc))
	require.NoError(t, SetEnvironmentKeyName(doc, "prod", ""))
	after := string(pxf.FormatDocument(doc))
	assert.Equal(t, before, after,
		"clear on an env that already lacks key_name is bytes-identical")
}

func TestSetEnvironmentKeyName_PreservesOtherEnvironments(t *testing.T) {
	// Editing one env must not perturb sibling envs' bytes. Pinning
	// because the round-trip formatter could in principle reorder
	// or rewrite map-shaped entries; this asserts the format stays
	// stable for the non-target rows.
	doc := parseSpec(t, simpleSpec)
	require.NoError(t, SetEnvironmentKeyName(doc, "prod", "prod-key"))
	// sandbox still carries its original key_name.
	assert.Equal(t, "sandbox-key", roundtripped(t, doc, "sandbox"))
	// dev still has none.
	assert.Equal(t, "", roundtripped(t, doc, "dev"))
}

func TestSetEnvironmentKeyName_EnvironmentNotFound(t *testing.T) {
	doc := parseSpec(t, simpleSpec)
	err := SetEnvironmentKeyName(doc, "staging", "any")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEnvironmentNotFound),
		"missing env returns the typed sentinel so the caller can map to InvalidArgument")
	assert.Contains(t, err.Error(), "staging",
		"error message names the offending env")
}

func TestSetEnvironmentKeyName_ErrorOnMalformedDocument(t *testing.T) {
	// No environments assignment at all.
	doc := parseSpec(t, `@type chameleon.v1.ChainSpec

version = "1.0.0"
`)
	err := SetEnvironmentKeyName(doc, "any", "v")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "environments")
}

func TestSetEnvironmentKeyName_ErrorOnNilDocument(t *testing.T) {
	require.Error(t, SetEnvironmentKeyName(nil, "any", "v"))
}

func TestSetEnvironmentKeyName_ErrorOnEmptyEnv(t *testing.T) {
	doc := parseSpec(t, simpleSpec)
	require.Error(t, SetEnvironmentKeyName(doc, "", "v"))
}

func TestSetEnvironmentKeyName_RoundtripPreservesComments(t *testing.T) {
	// pxf.FormatDocument carries comments through; this is the
	// load-bearing property of the AST-edit approach (vs. naive
	// regex substitution). A user with a chain.pxf annotated like
	//   # prod is the production environment, encrypts to root-key
	// must see that comment after pxfed edits the key_name. A
	// regression in pxf.FormatDocument's comment handling would
	// surface here.
	const withComments = `@type chameleon.v1.ChainSpec

version = "1.0.0"

# Production environment ancestry root.
environments = [
  # prod takes the company-wide encryption recipient.
  { name = "prod" key_name = "old-key" }
]
`
	doc := parseSpec(t, withComments)
	require.NoError(t, SetEnvironmentKeyName(doc, "prod", "new-key"))
	out := string(pxf.FormatDocument(doc))
	assert.Contains(t, out, "Production environment ancestry root",
		"leading-block comment survives")
	assert.Contains(t, out, "prod takes the company-wide",
		"inline list-element comment survives")
	assert.Contains(t, out, `"new-key"`,
		"new value lands on the wire")
	assert.False(t, strings.Contains(out, `"old-key"`),
		"old value is gone")
}
