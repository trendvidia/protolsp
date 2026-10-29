// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// fieldOptionsWithRequired returns a FieldOptions with the
// (pxf.required) extension (field 1314, varint = 1) embedded as
// unknown bytes. pxf.IsRequired reads this fallback path on
// descriptors that weren't built against a registered extension —
// which is what protodesc-from-FileDescriptorProto gives us here.
//
// Mirrors the protoc / pure-descriptor codepath in
// protowire-go's annotations.getBoolOption.
func fieldOptionsWithRequired(t *testing.T) *descriptorpb.FieldOptions {
	t.Helper()
	var buf []byte
	buf = protowire.AppendTag(buf, 1314, protowire.VarintType)
	buf = protowire.AppendVarint(buf, 1)
	opts := &descriptorpb.FieldOptions{}
	require.NoError(t, proto.Unmarshal(buf, opts),
		"FieldOptions must accept a raw extension into its unknown bytes")
	return opts
}

// buildAppConfigMessageType constructs an AppConfig message with
// three fields, two of which carry (pxf.required) = true:
//
//	package myapp.v1;
//	message AppConfig {
//	  string api_url     = 1 [(pxf.required) = true];
//	  string db_password = 2 [(pxf.required) = true];
//	  bool   debug       = 3;
//	}
func buildAppConfigMessageType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("myapp/v1/app_config.proto"),
		Package: proto.String("myapp.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("AppConfig"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("api_url"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("apiUrl"),
						Options:  fieldOptionsWithRequired(t),
					},
					{
						Name:     proto.String("db_password"),
						Number:   proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("dbPassword"),
						Options:  fieldOptionsWithRequired(t),
					},
					{
						Name:     proto.String("debug"),
						Number:   proto.Int32(3),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("debug"),
					},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	return dynamicpb.NewMessageType(fd.Messages().Get(0))
}

const chainRequiredSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "sandbox" extends = "prod" }
  { name = "dev"     extends = "sandbox" }
]

layers = [
  { id = "base" path = "base.pxf"        for_each = ONCE }
  { id = "env"  path = "env/${env}.pxf"  for_each = PER_ENV }
]
`

// initializeWithRequiredChain stands up a workspace with the
// chainRequiredSpec, an AppConfig schema lookup, and a set of pre-
// written layer files. layers maps filename → contents.
func initializeWithRequiredChain(t *testing.T, layers map[string]string) (*Server, string) {
	t.Helper()
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(chainRequiredSpec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	for name, content := range layers {
		require.NoError(t, os.WriteFile(
			filepath.Join(workspaceDir, name), []byte(content), 0o600))
	}
	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	lookup := newFakeLookup()
	lookup.add(buildAppConfigMessageType(t))
	srv.Lookups().Swap(lookup, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain(), "chain must load for C3 tests")
	return srv, workspaceDir
}

// schemaDiagsFor extracts diagnostics with the schema source tag —
// the C3 chain-required check uses "protolsp/schema" to keep
// related diagnostics colocated for editor UI grouping.
func schemaDiagsFor(t *testing.T, srv *Server, docURI uri.URI) []protocol.Diagnostic {
	t.Helper()
	doc := srv.Documents().Get(docURI)
	require.NotNil(t, doc, "document must be open for diagnostic inspection")
	out := []protocol.Diagnostic{}
	for _, d := range srv.DiagnosticsFor(doc) {
		if d.Source == "protolsp/schema" {
			out = append(out, d)
		}
	}
	return out
}

func TestChainRequired_LeafErrorsWhenNoLayerSetsField(t *testing.T) {
	// Neither base nor any env layer sets db_password. Leaf env/dev.pxf
	// must error.
	srv, ws := initializeWithRequiredChain(t, map[string]string{
		"base.pxf": `@type myapp.v1.AppConfig
api_url = "https://example.com"
`,
		"env/prod.pxf":    `@type myapp.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type myapp.v1.AppConfig` + "\n",
		"env/dev.pxf":     `@type myapp.v1.AppConfig` + "\n",
	})
	docURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, `@type myapp.v1.AppConfig`+"\n")))

	diags := schemaDiagsFor(t, srv, docURI)
	require.Len(t, diags, 1,
		"exactly one missing-required error (db_password); api_url is set in base")
	d := diags[0]
	assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity)
	assert.Contains(t, d.Message, "db_password",
		"diagnostic names the missing required field")
	assert.Contains(t, d.Message, "dev",
		"diagnostic cites the env's chain context")
	assert.Contains(t, d.Message, "base.pxf",
		"diagnostic lists chain files for grounding")
}

func TestChainRequired_AncestorSetsFieldSuppressesError(t *testing.T) {
	// base.pxf sets db_password → dev leaf has no missing-required
	// diagnostic for it.
	srv, ws := initializeWithRequiredChain(t, map[string]string{
		"base.pxf": `@type myapp.v1.AppConfig
api_url = "https://example.com"
db_password = "from-base"
`,
		"env/prod.pxf":    `@type myapp.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type myapp.v1.AppConfig` + "\n",
		"env/dev.pxf":     `@type myapp.v1.AppConfig` + "\n",
	})
	docURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, `@type myapp.v1.AppConfig`+"\n")))

	diags := schemaDiagsFor(t, srv, docURI)
	assert.Empty(t, diags,
		"both required fields set by ancestors → no diagnostic on the leaf")
}

func TestChainRequired_LeafSetsFieldItselfSuppressesError(t *testing.T) {
	// Leaf sets db_password locally; ancestors don't. Still no diag.
	srv, ws := initializeWithRequiredChain(t, map[string]string{
		"base.pxf":        `@type myapp.v1.AppConfig` + "\napi_url = \"https://example.com\"\n",
		"env/prod.pxf":    `@type myapp.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type myapp.v1.AppConfig` + "\n",
		"env/dev.pxf": `@type myapp.v1.AppConfig
db_password = "from-leaf"
`,
	})
	docURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, `@type myapp.v1.AppConfig
db_password = "from-leaf"
`)))

	diags := schemaDiagsFor(t, srv, docURI)
	assert.Empty(t, diags,
		"leaf-set required field → no diagnostic (trivial case)")
}

func TestChainRequired_OnceLayerSuppressesDiagnostics(t *testing.T) {
	// base.pxf is ONCE and omits both required fields. In chain mode,
	// no required-field diagnostic should fire on base.pxf — the
	// check lives at the per-env leaves.
	srv, ws := initializeWithRequiredChain(t, map[string]string{
		"base.pxf":        `@type myapp.v1.AppConfig` + "\n",
		"env/prod.pxf":    `@type myapp.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type myapp.v1.AppConfig` + "\n",
		"env/dev.pxf":     `@type myapp.v1.AppConfig` + "\n",
	})
	docURI := uri.File(filepath.Join(ws, "base.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, `@type myapp.v1.AppConfig`+"\n")))

	diags := schemaDiagsFor(t, srv, docURI)
	assert.Empty(t, diags,
		"ONCE chain layer suppresses required-field diags entirely")
}

func TestChainRequired_StandaloneFileGetsNoRequiredDiagnostic(t *testing.T) {
	// File outside the chain falls through chainRequiredDiagnostics
	// (returns nil). Today's protolsp doesn't emit per-file
	// required-field diagnostics for standalone files either —
	// pxf.UnmarshalDescriptor skips its post-decode pass — so this
	// asserts that standalone behavior is UNCHANGED by C3. (Adding
	// per-file required checks for standalone is a separate
	// decision outside the chameleon-integration design.)
	srv, ws := initializeWithRequiredChain(t, map[string]string{
		"base.pxf":        `@type myapp.v1.AppConfig` + "\n",
		"env/prod.pxf":    `@type myapp.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type myapp.v1.AppConfig` + "\n",
		"env/dev.pxf":     `@type myapp.v1.AppConfig` + "\n",
	})
	docURI := uri.File(filepath.Join(ws, "scratch.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, `@type myapp.v1.AppConfig`+"\n")))

	diags := schemaDiagsFor(t, srv, docURI)
	assert.Empty(t, diags,
		"standalone files: no chain-aware diagnostics, today's per-file required behavior is also none")
}

func TestChainRequired_EncryptedAncestorSkippedOnRead(t *testing.T) {
	// An encrypted ancestor with all required fields set is OPAQUE
	// to the LSP — we don't read its contents. The leaf should
	// still error if no readable layer sets the required field.
	const encryptedSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = [
  { id = "secrets" path = "secrets.pxf"   for_each = ONCE encrypted = true }
  { id = "env"     path = "env/${env}.pxf" for_each = PER_ENV }
]
`
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"), []byte(encryptedSpec), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	// Pretend secrets.pxf does set both required fields — but it's
	// encrypted, so the LSP can't see that.
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "secrets.pxf"), []byte(`@type myapp.v1.AppConfig
api_url = "from-secrets"
db_password = "from-secrets"
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "env", "prod.pxf"),
		[]byte(`@type myapp.v1.AppConfig`+"\n"), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "env", "dev.pxf"),
		[]byte(`@type myapp.v1.AppConfig`+"\n"), 0o600))

	srv, _ := initializeWithWorkspace(t, workspaceDir, nil)
	lookup := newFakeLookup()
	lookup.add(buildAppConfigMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	docURI := uri.File(filepath.Join(workspaceDir, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, `@type myapp.v1.AppConfig`+"\n")))

	diags := schemaDiagsFor(t, srv, docURI)
	// The encrypted layer is opaque; LSP can't tell what it sets.
	// So both required fields still surface as missing. Acceptable
	// behavior per the design (false-positive on encrypted-only
	// fields; users can opt out by setting the field in a readable
	// layer or by suppressing via inline_secrets policy later).
	require.Len(t, diags, 2,
		"both required fields error because the encrypted ancestor is opaque")
}

func TestChainRequired_UnsavedEditOnAncestorIsRespected(t *testing.T) {
	// An ancestor layer file is open in the editor with a NEW
	// required-field assignment that hasn't been saved to disk.
	// The leaf's diagnostic should reflect the editor state, not
	// the disk state.
	srv, ws := initializeWithRequiredChain(t, map[string]string{
		"base.pxf":        `@type myapp.v1.AppConfig` + "\napi_url = \"x\"\n",
		"env/prod.pxf":    `@type myapp.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type myapp.v1.AppConfig` + "\n",
		"env/dev.pxf":     `@type myapp.v1.AppConfig` + "\n",
	})
	// Open env/prod.pxf with a body that sets db_password — but
	// the disk version still doesn't.
	prodURI := uri.File(filepath.Join(ws, "env", "prod.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(prodURI), 1, `@type myapp.v1.AppConfig
db_password = "from-unsaved-edit"
`)))

	// Now analyze env/dev.pxf — it should NOT error on db_password.
	devURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type myapp.v1.AppConfig`+"\n")))

	diags := schemaDiagsFor(t, srv, devURI)
	assert.Empty(t, diags,
		"editor state (open doc with unsaved edit) feeds the chain check")
}

func TestChainRequired_NoTypeDirectiveYieldsNoDiagnostic(t *testing.T) {
	// Leaf has no @type — no schema to read required fields from,
	// so chain-aware mode also has nothing to check. Falls through
	// cleanly (no panic, no spurious diagnostic).
	srv, ws := initializeWithRequiredChain(t, map[string]string{
		"base.pxf":        `@type myapp.v1.AppConfig` + "\n",
		"env/prod.pxf":    `@type myapp.v1.AppConfig` + "\n",
		"env/sandbox.pxf": `@type myapp.v1.AppConfig` + "\n",
		"env/dev.pxf":     `name = "no type"` + "\n",
	})
	docURI := uri.File(filepath.Join(ws, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(docURI), 1, `name = "no type"`+"\n")))

	diags := schemaDiagsFor(t, srv, docURI)
	assert.Empty(t, diags,
		"no @type → no schema to consult, no chain-required diagnostics")
}

// ---------------------------------------------------------------------------
// Unit-level tests of the helpers — small, fast, no I/O.
// ---------------------------------------------------------------------------
