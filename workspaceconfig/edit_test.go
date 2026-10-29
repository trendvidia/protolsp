// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package workspaceconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// ---------------------------------------------------------------------------
// GetAttribute
// ---------------------------------------------------------------------------

func TestGetAttribute_PresentReturnsValue(t *testing.T) {
	doc, err := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig

registry {
  address = "got-from-file:443"
}
`))
	require.NoError(t, err)

	val, present, err := GetAttribute(doc, "registry.address")
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, "got-from-file:443", val)
}

func TestGetAttribute_AbsentReturnsZero(t *testing.T) {
	doc, err := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig
`))
	require.NoError(t, err)

	val, present, err := GetAttribute(doc, "registry.address")
	require.NoError(t, err)
	assert.False(t, present, "absent attribute reports present=false")
	assert.Equal(t, "", val, "absent string attribute returns empty string zero")
}

func TestGetAttribute_StringListPresent(t *testing.T) {
	doc, err := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig

proto_files {
  paths = ["a/", "b/"]
}
`))
	require.NoError(t, err)

	val, present, err := GetAttribute(doc, "proto_files.paths")
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, []string{"a/", "b/"}, val)
}

func TestGetAttribute_BoolPresent(t *testing.T) {
	doc, err := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "c.pxf"
  enabled = false
}
`))
	require.NoError(t, err)

	val, present, err := GetAttribute(doc, "chameleon.enabled")
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, false, val)
}

func TestGetAttribute_UnknownAttributeErrors(t *testing.T) {
	doc, _ := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig`))
	_, _, err := GetAttribute(doc, "registry.does_not_exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown attribute")
}

func TestGetAttribute_WrongTypeInFileErrors(t *testing.T) {
	// chameleon.enabled is a bool but the file has a string.
	doc, err := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig

chameleon {
  enabled = "yes"
}
`))
	require.NoError(t, err)

	_, _, err = GetAttribute(doc, "chameleon.enabled")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected bool")
}

// ---------------------------------------------------------------------------
// SetAttribute
// ---------------------------------------------------------------------------

func TestSetAttribute_CreatesBlockAndAssignment(t *testing.T) {
	doc, err := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig
`))
	require.NoError(t, err)

	require.NoError(t, SetAttribute(doc, "registry.address", "new:443"))

	val, present, err := GetAttribute(doc, "registry.address")
	require.NoError(t, err)
	assert.True(t, present)
	assert.Equal(t, "new:443", val)
}

func TestSetAttribute_AddsAssignmentToExistingBlock(t *testing.T) {
	doc, err := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig

registry {
  address = "existing:443"
}
`))
	require.NoError(t, err)

	require.NoError(t, SetAttribute(doc, "registry.namespace", "new-ns"))

	addr, _, err := GetAttribute(doc, "registry.address")
	require.NoError(t, err)
	assert.Equal(t, "existing:443", addr, "Sibling assignment untouched")

	ns, _, err := GetAttribute(doc, "registry.namespace")
	require.NoError(t, err)
	assert.Equal(t, "new-ns", ns)
}

func TestSetAttribute_MutatesExistingAssignment(t *testing.T) {
	doc, err := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig

registry {
  address = "old:443"
}
`))
	require.NoError(t, err)

	require.NoError(t, SetAttribute(doc, "registry.address", "new:443"))

	val, _, err := GetAttribute(doc, "registry.address")
	require.NoError(t, err)
	assert.Equal(t, "new:443", val)
}

func TestSetAttribute_BoolAndList(t *testing.T) {
	doc, err := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig`))
	require.NoError(t, err)

	require.NoError(t, SetAttribute(doc, "chameleon.enabled", true))
	require.NoError(t, SetAttribute(doc, "proto_files.paths", []string{"x/", "y/"}))

	bv, _, err := GetAttribute(doc, "chameleon.enabled")
	require.NoError(t, err)
	assert.Equal(t, true, bv)

	pv, _, err := GetAttribute(doc, "proto_files.paths")
	require.NoError(t, err)
	assert.Equal(t, []string{"x/", "y/"}, pv)
}

func TestSetAttribute_UnknownAttributeErrors(t *testing.T) {
	doc, _ := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig`))
	err := SetAttribute(doc, "registry.bogus", "anything")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown attribute")
}

func TestSetAttribute_TypeMismatchErrors(t *testing.T) {
	doc, _ := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig`))

	err := SetAttribute(doc, "chameleon.enabled", "yes-please")
	require.Error(t, err, "string into bool attribute")
	assert.Contains(t, err.Error(), "expected bool")

	err = SetAttribute(doc, "proto_files.paths", "single-string")
	require.Error(t, err, "string into list attribute")
	assert.Contains(t, err.Error(), "expected []string")
}

func TestSetAttribute_SemanticValidatorRejects(t *testing.T) {
	doc, _ := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig`))
	err := SetAttribute(doc, "security.inline_secrets", "maybe")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "allow|warn|refuse")
}

func TestSetAttribute_NilDocErrors(t *testing.T) {
	err := SetAttribute(nil, "registry.address", "x:1")
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Round-trip: comments + structure survive Set + FormatDocument
// ---------------------------------------------------------------------------

func TestSetAttribute_PreservesLeadingComments(t *testing.T) {
	// The file has a leading comment above the registry block and a
	// trailing comment on the address line. A Set that mutates only
	// the address value must leave both comments intact.
	src := `@type protolsp.v1.WorkspaceConfig

# Authoritative registry for this workspace.
registry {
  address = "old:443"
  namespace = "ns"
}
`
	doc, err := pxf.Parse([]byte(src))
	require.NoError(t, err)

	require.NoError(t, SetAttribute(doc, "registry.address", "new:443"))

	out := string(pxf.FormatDocument(doc))
	assert.Contains(t, out, "# Authoritative registry for this workspace.",
		"leading comment on the registry block survives Set")
	assert.Contains(t, out, `"new:443"`, "the new value lands in the file")
	assert.NotContains(t, out, `"old:443"`, "the old value is gone")
	assert.Contains(t, out, `namespace = "ns"`,
		"sibling assignment kept unchanged")
}

func TestSetAttribute_PreservesUnrelatedBlocks(t *testing.T) {
	// chameleon and security blocks must survive a registry.address Set.
	src := `@type protolsp.v1.WorkspaceConfig

registry {
  address = "old:443"
}

chameleon {
  chain_spec = "c.pxf"
}

security {
  inline_secrets = "warn"
}
`
	doc, err := pxf.Parse([]byte(src))
	require.NoError(t, err)

	require.NoError(t, SetAttribute(doc, "registry.address", "new:443"))

	out := string(pxf.FormatDocument(doc))
	assert.Contains(t, out, `chain_spec = "c.pxf"`)
	assert.Contains(t, out, `inline_secrets = "warn"`)
	assert.Contains(t, out, `"new:443"`)
}

func TestSetAttribute_RoundTripStableAfterReparse(t *testing.T) {
	// Set → format → parse → Get should return what we Set. Catches
	// any encoder/parser asymmetry.
	doc, err := pxf.Parse([]byte(`@type protolsp.v1.WorkspaceConfig`))
	require.NoError(t, err)

	require.NoError(t, SetAttribute(doc, "registry.address", "rt:443"))
	require.NoError(t, SetAttribute(doc, "registry.namespace", "rt-ns"))
	require.NoError(t, SetAttribute(doc, "proto_files.paths", []string{"a/", "b/"}))
	require.NoError(t, SetAttribute(doc, "chameleon.enabled", false))

	formatted := pxf.FormatDocument(doc)
	reparsed, warnings, err := Parse(formatted)
	require.NoError(t, err)
	assert.Empty(t, warnings)

	assert.Equal(t, "rt:443", reparsed.Registry.Address)
	assert.Equal(t, "rt-ns", reparsed.Registry.Namespace)
	assert.Equal(t, []string{"a/", "b/"}, reparsed.ProtoFiles.Paths)
	// chameleon.enabled = false in the file → defaults logic stays off.
	assert.False(t, reparsed.Chameleon.Enabled)
}

// ---------------------------------------------------------------------------
// LoadDocument + WriteDocument
// ---------------------------------------------------------------------------

func TestLoadDocument_NonexistentReturnsSeededDoc(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.pxf")
	doc, err := LoadDocument(path)
	require.NoError(t, err)
	require.NotNil(t, doc)
	assert.Equal(t, ExpectedType, doc.TypeURL,
		"absent file returns an AST seeded with the expected @type")
	assert.Empty(t, doc.Entries)
}

func TestWriteDocument_AtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".protolsp.pxf")
	require.NoError(t, os.WriteFile(path, []byte(`@type protolsp.v1.WorkspaceConfig

# preserved comment
registry {
  address = "before:443"
}
`), 0o600))

	doc, err := LoadDocument(path)
	require.NoError(t, err)
	require.NoError(t, SetAttribute(doc, "registry.address", "after:443"))
	require.NoError(t, WriteDocument(path, doc))

	got, err := os.ReadFile(path) // #nosec G304 -- test path under t.TempDir
	require.NoError(t, err)
	gotStr := string(got)
	assert.Contains(t, gotStr, "# preserved comment")
	assert.Contains(t, gotStr, `"after:443"`)
	assert.NotContains(t, gotStr, `"before:443"`)

	// Tempfile is cleaned up.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var leaked []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			leaked = append(leaked, e.Name())
		}
	}
	assert.Empty(t, leaked, "no tempfile should remain after successful write")
}

func TestWriteDocument_PreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".protolsp.pxf")
	require.NoError(t, os.WriteFile(path, []byte(`@type protolsp.v1.WorkspaceConfig
`), 0o600))

	doc, err := LoadDocument(path)
	require.NoError(t, err)
	require.NoError(t, SetAttribute(doc, "registry.address", "x:1"))
	require.NoError(t, WriteDocument(path, doc))

	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm(),
		"existing file's mode must survive the atomic rewrite")
}
