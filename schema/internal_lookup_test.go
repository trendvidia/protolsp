// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestInternalLookup_ResolvesWorkspaceConfig(t *testing.T) {
	l := NewInternalLookup()
	mt, err := l.FindMessageByName("protolsp.v1.WorkspaceConfig")
	require.NoError(t, err)
	require.NotNil(t, mt)
	assert.Equal(t, protoreflect.FullName("protolsp.v1.WorkspaceConfig"),
		mt.Descriptor().FullName())
}

func TestInternalLookup_ResolvesChainSpec(t *testing.T) {
	l := NewInternalLookup()
	mt, err := l.FindMessageByName("chameleon.v1.ChainSpec")
	require.NoError(t, err)
	require.NotNil(t, mt)
	// Sanity-check a couple of declared fields so we know the
	// descriptor isn't a stub.
	desc := mt.Descriptor()
	assert.NotNil(t, desc.Fields().ByName("version"))
	assert.NotNil(t, desc.Fields().ByName("environments"))
	assert.NotNil(t, desc.Fields().ByName("layers"))
}

// TestInternalLookup_DescriptorCarriesSourceInfo guards against
// regression on the cursor-at-top-of-file go-to-def bug: descriptors
// returned by the internal lookup must have populated SourceLocations,
// or every go-to-def into a protolsp.v1.* / chameleon.v1.* type
// silently lands at line 0 col 0. Generated .pb.go files strip
// SourceCodeInfo; the fix is to compile from the embedded .proto
// sources via protocompile at startup.
func TestInternalLookup_DescriptorCarriesSourceInfo(t *testing.T) {
	l := NewInternalLookup()
	mt, err := l.FindMessageByName("protolsp.v1.WorkspaceConfig")
	require.NoError(t, err)
	require.NotNil(t, mt)

	field := mt.Descriptor().Fields().ByName("registry")
	require.NotNil(t, field, "fixture has a `registry` field")

	file := field.ParentFile()
	require.NotNil(t, file)
	assert.NotZero(t, file.SourceLocations().Len(),
		"FileDescriptor must have SourceCodeInfo for go-to-def cursor placement")
	loc := file.SourceLocations().ByDescriptor(field)
	assert.NotZero(t, loc.StartLine,
		"field's StartLine must be non-zero; cursor-at-(0,0) is the bug this test guards against")
}

func TestInternalLookup_ResolvesNestedMessageTypes(t *testing.T) {
	// The schemas declare related messages (Environment, LayerEntry,
	// RegistrySource, etc.). Confirm a representative one resolves.
	l := NewInternalLookup()
	mt, err := l.FindMessageByName("protolsp.v1.RegistrySource")
	require.NoError(t, err)
	require.NotNil(t, mt)
	assert.NotNil(t, mt.Descriptor().Fields().ByName("address"))
}

func TestInternalLookup_UnknownNameReturnsNotFound(t *testing.T) {
	l := NewInternalLookup()
	mt, err := l.FindMessageByName("acme.demo.v1.NotAThing")
	assert.Nil(t, mt)
	assert.Error(t, err)
}

func TestInternalLookup_RangeMessagesIncludesBothFiles(t *testing.T) {
	l := NewInternalLookup()
	var names []protoreflect.FullName
	l.RangeMessages(func(mt protoreflect.MessageType) bool {
		names = append(names, mt.Descriptor().FullName())
		return true
	})
	assert.Contains(t, names, protoreflect.FullName("protolsp.v1.WorkspaceConfig"))
	assert.Contains(t, names, protoreflect.FullName("chameleon.v1.ChainSpec"))
	assert.Contains(t, names, protoreflect.FullName("chameleon.v1.LayerEntry"))
}

func TestInternalLookup_FindFileReportsInternalOrigin(t *testing.T) {
	l := NewInternalLookup()
	fd, origin, err := l.FindFileByPathWithOrigin("protolsp/v1/workspace_config.proto")
	require.NoError(t, err)
	require.NotNil(t, fd)
	assert.Equal(t, InternalOrigin, origin,
		"internal files surface a stable namespace label distinct from real registries")
}

func TestInternalLookup_IsStaleAlwaysFalse(t *testing.T) {
	assert.False(t, NewInternalLookup().IsStale(),
		"baked-in descriptors are never stale")
}
