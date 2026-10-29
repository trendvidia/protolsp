// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	schemav1 "github.com/trendvidia/protocompile/gen/protowire/schema/v1"
)

// dnDeclsProto declares one of everything the export covers, under a
// package so both bare and qualified forms are expected.
const dnDeclsProto = `syntax = "proto3";

package acme.demo.v1;

annotation checked(by: string);

type UserID = string;

function is_uuid(s: string);

message Widget {
  string id = 1;
}

enum Color {
  COLOR_UNSPECIFIED = 0;
}
`

// newDeclaredNamesWorkspace stages rel→body files under a temp root
// (naive no-buf.yaml layout, so the root itself is the include path),
// builds the proto index, and returns the workspace + root.
func newDeclaredNamesWorkspace(t *testing.T, files map[string]string) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
	}
	w := New(nil)
	w.SetBufLint(false) // no buf subprocesses from Analyze in tests
	w.ws.SetWorkspaceRoot(uri.File(root))
	w.LoadProtoIndex()
	return w, root
}

// The on-disk declaration set surfaces with no documents open: every
// workspace-local indexed file contributes its top-level names, bare
// and package-qualified, sorted and deduplicated.
func TestDeclaredNames_LocalIndexFiles(t *testing.T) {
	w, _ := newDeclaredNamesWorkspace(t, map[string]string{
		"acme/decls.proto": dnDeclsProto,
	})

	got := w.DeclaredNames()
	assert.Equal(t, []string{"acme.demo.v1.checked", "checked"}, got.Annotations)
	assert.Equal(t, []string{"acme.demo.v1.is_uuid", "is_uuid"}, got.Functions)
	assert.Equal(t, []string{
		"Color", "UserID", "Widget",
		"acme.demo.v1.Color", "acme.demo.v1.UserID", "acme.demo.v1.Widget",
	}, got.Types)
}

// An open document's retained AST contributes keystroke-fresh names:
// a declaration that exists only in the unsaved buffer counts right
// after its analyze pass, alongside the on-disk set.
func TestDeclaredNames_OpenDocIsKeystrokeFresh(t *testing.T) {
	w, root := newDeclaredNamesWorkspace(t, map[string]string{
		"acme/decls.proto": dnDeclsProto,
	})

	// The buffer declares `fresh`; no file on disk has it.
	docPath := filepath.Join(root, "acme", "scratch.proto")
	doc := &Document{
		URI: uri.File(docPath),
		Source: []byte(`syntax = "proto3";

package acme.demo.v1;

annotation fresh;
`),
	}
	w.docs.Open(doc)
	w.Analyze(doc)

	got := w.DeclaredNames()
	assert.Equal(t, []string{"acme.demo.v1.checked", "acme.demo.v1.fresh", "checked", "fresh"},
		got.Annotations, "the unsaved buffer's declaration joins the on-disk set")
}

// A file without a package contributes bare names only.
func TestDeclaredNames_NoPackageMeansBareOnly(t *testing.T) {
	w, _ := newDeclaredNamesWorkspace(t, map[string]string{
		"nopkg.proto": `syntax = "proto3";

annotation loose;
`,
	})

	got := w.DeclaredNames()
	assert.Equal(t, []string{"loose"}, got.Annotations)
}

// An empty workspace yields the zero value — nil sets, which the
// editor-side consumer treats as "match nothing."
func TestDeclaredNames_EmptyWorkspace(t *testing.T) {
	w, _ := newDeclaredNamesWorkspace(t, nil)
	got := w.DeclaredNames()
	assert.Nil(t, got.Annotations)
	assert.Nil(t, got.Types)
	assert.Nil(t, got.Functions)
}

// carrierLookup is a schema.Lookup fake serving exactly one compiled
// FileDescriptor by path — the shape resolveRFC001RemoteDecl (and the
// declared-name export's remote branch) sees for a registry-known,
// index-unknown import.
type carrierLookup struct{ fd protoreflect.FileDescriptor }

func (l carrierLookup) FindMessageByName(protoreflect.FullName) (protoreflect.MessageType, error) {
	return nil, protoregistry.NotFound
}
func (l carrierLookup) RangeMessages(func(protoreflect.MessageType) bool) {}
func (l carrierLookup) FindFileByPathWithOrigin(path string) (protoreflect.FileDescriptor, string, error) {
	if l.fd != nil && path == l.fd.Path() {
		return l.fd, "testns", nil
	}
	return nil, "", protoregistry.NotFound
}
func (l carrierLookup) GetSource(context.Context, string) ([]byte, error) {
	return nil, protoregistry.NotFound
}
func (l carrierLookup) IsStale() bool { return false }

// carrierFileDescriptor compiles a descriptor for acme/remote.proto
// whose FileOptions carry the RFC-001 declarations — what a registry
// serves for a dep compiled by protocompile v1.2+.
func carrierFileDescriptor(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	opts := &descriptorpb.FileOptions{}
	proto.SetExtension(opts, schemav1.E_AnnotationDecls, &schemav1.FileAnnotationDecls{
		Declarations: []*schemav1.AnnotationDecl{{Name: "audited"}},
	})
	proto.SetExtension(opts, schemav1.E_TypeDecls, &schemav1.FileTypeDecls{
		Declarations: []*schemav1.TypeDecl{{Name: "Token", BaseTypeFqn: "string"}},
	})
	proto.SetExtension(opts, schemav1.E_Functions, &schemav1.FileFunctions{
		Declarations: []*schemav1.FunctionDecl{{Name: "is_token"}},
	})
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:        proto.String("acme/remote.proto"),
		Package:     proto.String("acme.remote.v1"),
		Syntax:      proto.String("proto3"),
		Options:     opts,
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Anchor")}},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name:  proto.String("Mode"),
			Value: []*descriptorpb.EnumValueDescriptorProto{{Name: proto.String("MODE_UNSPECIFIED"), Number: proto.Int32(0)}},
		}},
	}, nil)
	require.NoError(t, err)
	return fd
}

// An open document's import the index does NOT hold resolves through
// the workspace's schema lookup: the compiled carrier's declarations
// and the descriptor's top-level messages/enums all count. An import
// resolving nowhere contributes nothing.
func TestDeclaredNames_RemoteImportCarrier(t *testing.T) {
	w, root := newDeclaredNamesWorkspace(t, nil)
	w.Lookups().Swap(carrierLookup{fd: carrierFileDescriptor(t)}, nil)

	docPath := filepath.Join(root, "importer.proto")
	doc := &Document{
		URI: uri.File(docPath),
		Source: []byte(`syntax = "proto3";

package acme.app.v1;

import "acme/remote.proto";
import "acme/missing.proto";

@acme.remote.v1.audited
message Order {
  string id = 1;
}
`),
	}
	w.docs.Open(doc)
	w.Analyze(doc)

	got := w.DeclaredNames()
	assert.Equal(t, []string{"acme.remote.v1.audited", "audited"}, got.Annotations)
	assert.Equal(t, []string{"acme.remote.v1.is_token", "is_token"}, got.Functions)
	assert.Equal(t, []string{
		"Anchor", "Mode", "Order", "Token",
		"acme.app.v1.Order",
		"acme.remote.v1.Anchor", "acme.remote.v1.Mode", "acme.remote.v1.Token",
	}, got.Types)
}

// Nil-receiver and no-index calls degrade to the zero value / the
// open-doc-only set instead of panicking.
func TestDeclaredNames_NilSafety(t *testing.T) {
	var nilWS *Workspace
	assert.Equal(t, DeclaredNames{}, nilWS.DeclaredNames())

	// No LoadProtoIndex call: only open docs contribute.
	w := New(nil)
	w.SetBufLint(false)
	doc := &Document{
		URI:    uri.File(filepath.Join(t.TempDir(), "solo.proto")),
		Source: []byte("syntax = \"proto3\";\n\nannotation solo;\n"),
	}
	w.docs.Open(doc)
	w.Analyze(doc)
	assert.Equal(t, []string{"solo"}, w.DeclaredNames().Annotations)
}
