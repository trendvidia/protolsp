// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package schema

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const greetingProto = `syntax = "proto3";
package myapp.v1;

// Greeting carries a salutation.
message Greeting {
  // who to greet.
  string name = 1;
  string locale = 2;
}
`

const importerProto = `syntax = "proto3";
package myapp.v1;

import "myapp/v1/greeting.proto";

message Envelope {
  Greeting greeting = 1;
}
`

func writeProto(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
	require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
}

func TestOpenProtoFiles_CompilesAndResolvesMessage(t *testing.T) {
	root := t.TempDir()
	writeProto(t, root, "myapp/v1/greeting.proto", greetingProto)

	lk, closer, errs, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{
		Sources: []ProtoFilesSource{{Paths: []string{root}}},
	}, nil)
	require.NoError(t, err)
	require.Empty(t, errs)
	require.NotNil(t, lk)
	require.NotNil(t, closer)
	t.Cleanup(func() { _ = closer.Close() })

	mt, err := lk.FindMessageByName(protoreflect.FullName("myapp.v1.Greeting"))
	require.NoError(t, err)
	require.NotNil(t, mt)
	assert.Equal(t, protoreflect.FullName("myapp.v1.Greeting"), mt.Descriptor().FullName())
	assert.Equal(t, 2, mt.Descriptor().Fields().Len())
}

// TestOpenProtoFiles_FieldDescriptorHasSourceLocation probes whether
// the compiled descriptors carry per-field source info. If this fails,
// go-to-def lands the cursor at (0, 0) instead of the field's
// definition line — that's the user-visible symptom of source info
// being stripped at compile time.
func TestOpenProtoFiles_FieldDescriptorHasSourceLocation(t *testing.T) {
	root := t.TempDir()
	writeProto(t, root, "myapp/v1/greeting.proto", greetingProto)

	lk, closer, _, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{
		Sources: []ProtoFilesSource{{Paths: []string{root}}},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, lk)
	t.Cleanup(func() { _ = closer.Close() })

	mt, err := lk.FindMessageByName(protoreflect.FullName("myapp.v1.Greeting"))
	require.NoError(t, err)
	require.NotNil(t, mt)

	field := mt.Descriptor().Fields().ByName("name")
	require.NotNil(t, field, "fixture has a `name` field")

	file := field.ParentFile()
	require.NotNil(t, file)
	loc := file.SourceLocations().ByDescriptor(field)
	assert.NotZero(t, loc.StartLine,
		"protocompile with SourceInfoStandard must populate per-field source locations; "+
			"a zero StartLine here causes go-to-def to land at the top of the .proto file")
}

func TestOpenProtoFiles_CrossFileImportResolves(t *testing.T) {
	root := t.TempDir()
	writeProto(t, root, "myapp/v1/greeting.proto", greetingProto)
	writeProto(t, root, "myapp/v1/envelope.proto", importerProto)

	lk, closer, errs, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{
		Sources: []ProtoFilesSource{{Paths: []string{root}}},
	}, nil)
	require.NoError(t, err)
	assert.Empty(t, errs)
	require.NotNil(t, lk)
	t.Cleanup(func() { _ = closer.Close() })

	mt, err := lk.FindMessageByName(protoreflect.FullName("myapp.v1.Envelope"))
	require.NoError(t, err)
	require.NotNil(t, mt)
}

func TestOpenProtoFiles_SyntaxErrorSurfacesAsCompileError(t *testing.T) {
	root := t.TempDir()
	writeProto(t, root, "myapp/v1/broken.proto", `syntax = "proto3";
package myapp.v1;
message  // missing brace
`)

	lk, closer, errs, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{
		Sources: []ProtoFilesSource{{Paths: []string{root}}},
	}, nil)
	require.NoError(t, err)
	require.NotEmpty(t, errs, "syntax error must surface")
	if lk != nil {
		t.Cleanup(func() { _ = closer.Close() })
	}
	// First error tagged with the file name (relative to the import root).
	assert.Equal(t, "myapp/v1/broken.proto", errs[0].File)
	assert.NotZero(t, errs[0].Line)
}

func TestOpenProtoFiles_RangeMessagesIncludesNested(t *testing.T) {
	root := t.TempDir()
	writeProto(t, root, "nested.proto", `syntax = "proto3";
package nest;

message Outer {
  message Inner {
    string x = 1;
  }
  Inner inner = 1;
}
`)

	lk, closer, errs, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{
		Sources: []ProtoFilesSource{{Paths: []string{root}}},
	}, nil)
	require.NoError(t, err)
	require.Empty(t, errs)
	t.Cleanup(func() { _ = closer.Close() })

	seen := map[protoreflect.FullName]bool{}
	lk.RangeMessages(func(mt protoreflect.MessageType) bool {
		seen[mt.Descriptor().FullName()] = true
		return true
	})
	assert.True(t, seen["nest.Outer"], "outer message visible to RangeMessages")
	assert.True(t, seen["nest.Outer.Inner"], "nested message visible to RangeMessages")
}

func TestOpenProtoFiles_EmptyConfigReturnsNil(t *testing.T) {
	lk, closer, errs, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{}, nil)
	require.NoError(t, err)
	assert.Nil(t, lk)
	assert.Nil(t, closer)
	assert.Empty(t, errs)
}

func TestOpenProtoFiles_NonExistentPathSkipsWithoutError(t *testing.T) {
	lk, closer, errs, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{
		Sources: []ProtoFilesSource{{Paths: []string{"/path/that/does/not/exist"}}},
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, lk, "missing path leaves no proto files; lookup is nil")
	assert.Nil(t, closer)
	assert.Empty(t, errs)
}

func TestOpenProtoFiles_GetSourceReturnsRawBytes(t *testing.T) {
	root := t.TempDir()
	writeProto(t, root, "myapp/v1/greeting.proto", greetingProto)

	lk, closer, _, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{
		Sources: []ProtoFilesSource{{Paths: []string{root}}},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, lk)
	t.Cleanup(func() { _ = closer.Close() })

	got, err := lk.GetSource(context.Background(), "myapp/v1/greeting.proto")
	require.NoError(t, err)
	assert.Equal(t, greetingProto, string(got))
}

func TestOpenProtoFiles_FindFileByPathWithOriginReportsProtoFilesOrigin(t *testing.T) {
	root := t.TempDir()
	writeProto(t, root, "myapp/v1/greeting.proto", greetingProto)

	lk, closer, _, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{
		Sources: []ProtoFilesSource{{Paths: []string{root}}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closer.Close() })

	fd, origin, err := lk.FindFileByPathWithOrigin("myapp/v1/greeting.proto")
	require.NoError(t, err)
	assert.Equal(t, ProtoFilesOrigin, origin)
	assert.Equal(t, "myapp/v1/greeting.proto", fd.Path())
}

func TestProtoFilesParents_ExposesNamespacedRegistriesForWithFallback(t *testing.T) {
	root := t.TempDir()
	writeProto(t, root, "myapp/v1/greeting.proto", greetingProto)
	lk, closer, _, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{
		Sources: []ProtoFilesSource{{Paths: []string{root}}},
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, lk)
	t.Cleanup(func() { _ = closer.Close() })

	files, types := ProtoFilesParents(lk)
	require.NotNil(t, files, "NamespacedFiles must be exposed for WithFallback")
	require.NotNil(t, types, "NamespacedTypes must be exposed for WithFallback")

	// The same descriptor that drives the lookup is visible to a
	// caller that would pass these into protoregistry/client.WithFallback.
	fd, err := files.FindFileByPath("myapp/v1/greeting.proto")
	require.NoError(t, err)
	assert.Equal(t, protoreflect.FullName("myapp.v1"), fd.Package())

	mt, err := types.FindMessageByName(protoreflect.FullName("myapp.v1.Greeting"))
	require.NoError(t, err)
	require.NotNil(t, mt)
}

func TestProtoFilesParents_NilForNonProtoFilesLookups(t *testing.T) {
	files, types := ProtoFilesParents(NewInternalLookup())
	assert.Nil(t, files)
	assert.Nil(t, types)
}

func TestOpenProtoFiles_IsStaleAlwaysFalse(t *testing.T) {
	root := t.TempDir()
	writeProto(t, root, "x.proto", `syntax="proto3"; package x; message M {}`)
	lk, closer, _, err := OpenProtoFiles(context.Background(), ProtoFilesConfig{
		Sources: []ProtoFilesSource{{Paths: []string{root}}},
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = closer.Close() })
	assert.False(t, lk.IsStale())
}
