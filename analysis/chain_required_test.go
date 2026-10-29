// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestCollectTopLevelFieldNames_TopLevelAssignmentsAndBlocks(t *testing.T) {
	got := collectTopLevelFieldNames([]byte(`@type myapp.v1.AppConfig
api_url = "https://example.com"
nested {
  inner = 1
}
db_password = "x"
`))
	assert.True(t, got["api_url"])
	assert.True(t, got["nested"], "top-level block counts as a set field")
	assert.True(t, got["db_password"])
	assert.False(t, got["inner"], "nested-block entries are NOT collected at the top level")
}

func TestCollectTopLevelFieldNames_UnparseableYieldsNil(t *testing.T) {
	got := collectTopLevelFieldNames([]byte(`@type x { unclosed`))
	assert.Empty(t, got, "broken PXF yields nil rather than a panic")
}

func TestCollectRequiredFieldNames_OrdersByDescriptor(t *testing.T) {
	mt := buildAppConfigMessageType(t)
	got := collectRequiredFieldNames(mt.Descriptor())
	require.Equal(t, []string{"api_url", "db_password"}, got,
		"required fields surface in descriptor declaration order")
}
