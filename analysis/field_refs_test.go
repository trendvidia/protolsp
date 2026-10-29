// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// buildAppConfigWithNestedDb constructs an AppConfig message with
// scalar fields + a nested DbConfig submessage, exercising the
// dotted-path resolver:
//
//	package c4test.v1;
//	message DbConfig {
//	  string host = 1;
//	  int32  port = 2;
//	}
//	message AppConfig {
//	  string   api_url = 1;
//	  bool     debug   = 2;
//	  DbConfig db      = 3;
//	}
func buildAppConfigWithNestedDb(t *testing.T) protoreflect.MessageType {
	t.Helper()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("c4test/v1/app_config.proto"),
		Package: proto.String("c4test.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("DbConfig"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("host"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("host"),
					},
					{
						Name:     proto.String("port"),
						Number:   proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("port"),
					},
				},
			},
			{
				Name: proto.String("AppConfig"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{
						Name:     proto.String("api_url"),
						Number:   proto.Int32(1),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("apiUrl"),
					},
					{
						Name:     proto.String("debug"),
						Number:   proto.Int32(2),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						JsonName: proto.String("debug"),
					},
					{
						Name:     proto.String("db"),
						Number:   proto.Int32(3),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						TypeName: proto.String(".c4test.v1.DbConfig"),
						JsonName: proto.String("db"),
					},
				},
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)
	// AppConfig is the second message (DbConfig is first); pick it
	// explicitly so the test name is descriptive.
	for i := 0; i < fd.Messages().Len(); i++ {
		m := fd.Messages().Get(i)
		if m.Name() == "AppConfig" {
			return dynamicpb.NewMessageType(m)
		}
	}
	t.Fatal("AppConfig not found in file descriptor")
	return nil
}

func TestResolveFieldPath_ExistingAtAllDepths(t *testing.T) {
	mt := buildAppConfigWithNestedDb(t)
	desc := mt.Descriptor()
	cases := []string{"api_url", "debug", "db", "db.host", "db.port"}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			ok, isCtx := resolveFieldPath(p, desc)
			assert.True(t, ok, "should resolve")
			assert.False(t, isCtx)
		})
	}
}

func TestResolveFieldPath_RejectsUnknownPaths(t *testing.T) {
	mt := buildAppConfigWithNestedDb(t)
	desc := mt.Descriptor()
	cases := []string{"nope", "db.bogus", "db.host.deeper", "api_url.field"}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			ok, isCtx := resolveFieldPath(p, desc)
			assert.False(t, ok, "should NOT resolve")
			assert.False(t, isCtx)
		})
	}
}

func TestResolveFieldPath_EnvAndCtxAreContextRefs(t *testing.T) {
	mt := buildAppConfigWithNestedDb(t)
	desc := mt.Descriptor()
	for _, p := range []string{"env.X", "ctx.region", "env"} {
		t.Run(p, func(t *testing.T) {
			ok, isCtx := resolveFieldPath(p, desc)
			assert.False(t, ok)
			assert.True(t, isCtx,
				"caller should skip diagnostics for env / ctx prefixes")
		})
	}
}

func TestResolveFieldPath_EmptyOrNilIsRejected(t *testing.T) {
	mt := buildAppConfigWithNestedDb(t)
	desc := mt.Descriptor()
	ok, _ := resolveFieldPath("", desc)
	assert.False(t, ok)
	ok, _ = resolveFieldPath("api_url", nil)
	assert.False(t, ok)
}
