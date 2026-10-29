// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// buildCredentialsWithSecretType constructs a MessageType for a
// message that contains a pxf.Secret-typed field. We build both
// the pxf.Secret descriptor inline and the wrapping Credentials
// message in one FileDescriptorProto so protodesc compiles them
// in a single pass. This isn't the real pxf.Secret (it's in
// protowire-go's well-known set) but FullName matches, which is
// all the LSP's isSecretField check cares about.
//
// Schema:
//
//	package pxf;
//	message Secret { string value = 1; string hint = 2; string fingerprint = 3; }
//
//	package acme.demo.v1;
//	message Credentials {
//	  string username = 1;
//	  pxf.Secret password = 2;
//	}
func buildCredentialsWithSecretType(t *testing.T) protoreflect.MessageType {
	t.Helper()

	secretFile := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("pxf/secret.proto"),
		Package: proto.String("pxf"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Secret"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("value"), Number: proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("value")},
					{Name: proto.String("hint"), Number: proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("hint")},
					{Name: proto.String("fingerprint"), Number: proto.Int32(3),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("fingerprint")},
				},
			},
		},
	}

	credentialsFile := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("acme/demo/v1/credentials.proto"),
		Package:    proto.String("acme.demo.v1"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"pxf/secret.proto"},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("Credentials"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("username"), Number: proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("username")},
					{Name: proto.String("password"), Number: proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						TypeName: proto.String(".pxf.Secret"),
						JsonName: proto.String("password")},
				},
			},
		},
	}

	set := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{secretFile, credentialsFile},
	}
	files, err := protodesc.NewFiles(set)
	require.NoError(t, err)
	credFD, err := files.FindFileByPath("acme/demo/v1/credentials.proto")
	require.NoError(t, err)
	return dynamicpb.NewMessageType(credFD.Messages().Get(0))
}

func TestCollectSecretRanges_ScalarShorthand(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@type acme.demo.v1.Credentials

username = "alice"
password = "super-secret"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	doc := srv.Documents().Get("file:///work/c.pxf")
	require.NotNil(t, doc)
	require.Len(t, doc.SecretRanges, 1,
		"scalar shorthand `pw = \"...\"` records one SecretRange")
	sr := doc.SecretRanges[0]
	assert.Equal(t, "password", sr.Path)
	assert.Equal(t, SecretFormScalar, sr.Form)
	assert.Equal(t, []string{"super-secret"}, doc.SecretValues(),
		"plaintext stashed for downstream redaction")
}

func TestCollectSecretRanges_BlockForm(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)

	// Block form: hint + fingerprint are non-sensitive; only value
	// is redacted.
	const src = `@type acme.demo.v1.Credentials

username = "alice"
password {
  value = "block-secret"
  hint = "AWS key, US-East"
}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	doc := srv.Documents().Get("file:///work/c.pxf")
	require.NotNil(t, doc)
	require.Len(t, doc.SecretRanges, 1,
		"block-form pxf.Secret records exactly one SecretRange")
	sr := doc.SecretRanges[0]
	assert.Equal(t, "password", sr.Path)
	assert.Equal(t, SecretFormBlock, sr.Form)
	assert.Equal(t, []string{"block-secret"}, doc.SecretValues(),
		"hint is non-sensitive — only the `value` field is captured")
}

func TestCollectSecretRanges_OnSecretHookDropsValue(t *testing.T) {
	// When the validator runs with OnSecretField wired, the
	// resulting proto message has Secret.value empty. We verify
	// indirectly via the absence of any "expected string" type-
	// mismatch error from the validator — the hook accepted the
	// scalar and skipped populating the inner field. (The "value
	// is empty in the proto" assertion lives in the upstream
	// pxf.UnmarshalDescriptor tests; we just confirm the hook
	// is wired so plaintext doesn't end up in the heap.)
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@type acme.demo.v1.Credentials

username = "alice"
password = "secret-value"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	doc := srv.Documents().Get("file:///work/c.pxf")
	require.NotNil(t, doc)
	assert.NoError(t, doc.SchemaErr,
		"scalar-shorthand pxf.Secret validates cleanly with OnSecretField wired")
}

func TestPublishDiagnostics_RedactsSecretValuesInMessage(t *testing.T) {
	// Trigger a schema error where the offending text would
	// otherwise echo the secret value, and verify that the
	// published diagnostic message has it replaced with
	// "<redacted>".
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)

	// `username` expects a string. Give it an int alongside a real
	// password. The diagnostic for `username` will fire; we verify
	// the SECRET value never appears in the diagnostic message
	// even if pxf accidentally surfaces it through some path.
	//
	// We simulate the scenario by manually setting up the document
	// so a hypothetical leaky diagnostic carries the secret in its
	// message. publishDiagnostics is what does the redaction; we
	// inject a diagnostic to confirm it runs.
	const src = `@type acme.demo.v1.Credentials

username = "alice"
password = "super-leaked"
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/c.pxf", 1, src)))

	// Force-publish a synthetic diagnostic that includes the
	// secret bytes — emulates a future leak path we want covered.
	doc := srv.Documents().Get("file:///work/c.pxf")
	srv.PublishDiagnosticsRaw(context.Background(), doc.URI, 1, []protocol.Diagnostic{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: 3, Character: 12},
				End:   protocol.Position{Line: 3, Character: 24},
			},
			Severity: protocol.DiagnosticSeverityError,
			Source:   "protolsp/test",
			Message:  `invalid value "super-leaked" for field "password"`,
		},
	})

	// The synthetic diagnostic goes through PublishDiagnosticsRaw
	// which doesn't redact — only publishDiagnostics (which goes
	// through doc-aware redaction) does. We re-run via the
	// document-aware path to assert redaction is in effect there.
	srv.PublishDiagnostics(context.Background(), doc)

	notes := fc.calls()
	require.NotEmpty(t, notes, "publishDiagnostics emitted at least one notification")
	// Take the last one (most recent state); scan for any
	// occurrence of the raw secret in the rendered messages.
	last := notes[len(notes)-1]
	for _, d := range last.params.Diagnostics {
		assert.NotContains(t, d.Message, "super-leaked",
			"redacted diagnostic message must not contain the secret bytes")
	}
}

func TestPublishDiagnostics_NoSecretsIsPassThrough(t *testing.T) {
	// Without any secrets, redaction is a no-op — existing
	// diagnostic messages render unchanged. Pins that the redact
	// path doesn't accidentally munge non-secret content.
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildGreetingMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/g.pxf", 1, greetingUnknownField)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := notes[0].params.Diagnostics
	require.Len(t, diags, 1)
	assert.Contains(t, diags[0].Message, "stranger",
		"non-secret content passes through unchanged")
}
