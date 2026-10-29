// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	schemav1 "github.com/trendvidia/protocompile/gen/protowire/schema/v1"
)

// The carrier extractor must surface every RFC-001 FileOptions
// extension — including the use-site SourceMap (#219 criterion 1),
// which navigation doesn't consume yet but #220's runtime-error
// mapping will.
func TestRFC001CarrierFromDescriptor(t *testing.T) {
	opts := &descriptorpb.FileOptions{}
	proto.SetExtension(opts, schemav1.E_SourceMap, &schemav1.SourceMap{
		File: "acme/remote.proto",
		Entries: []*schemav1.SourceEntry{{
			Kind:           schemav1.EntryKind_FIELD_VALIDATE,
			DescriptorPath: "acme.remote.v1.Anchor.id[acme.remote.v1.audited#0]",
		}},
	})
	proto.SetExtension(opts, schemav1.E_TypeDecls, &schemav1.FileTypeDecls{
		Declarations: []*schemav1.TypeDecl{{Name: "Token", BaseTypeFqn: "string"}},
	})
	proto.SetExtension(opts, schemav1.E_Functions, &schemav1.FileFunctions{
		Declarations: []*schemav1.FunctionDecl{{Name: "is_token"}},
	})
	proto.SetExtension(opts, schemav1.E_AnnotationDecls, &schemav1.FileAnnotationDecls{
		Declarations: []*schemav1.AnnotationDecl{{
			Name:   "audited",
			Params: []*schemav1.AnnotationParam{{Name: "by", Type: schemav1.ParamType_STRING}},
		}},
	})
	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("acme/remote.proto"),
		Package:     proto.String("acme.remote.v1"),
		Syntax:      proto.String("proto3"),
		Options:     opts,
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Anchor")}},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)

	c := rfc001CarrierFromDescriptor(fd)
	require.NotNil(t, c.SourceMap, "SourceMap extension surfaces on the carrier")
	assert.Equal(t, "acme/remote.proto", c.SourceMap.GetFile())
	require.Len(t, c.SourceMap.GetEntries(), 1)
	assert.Equal(t, "acme.remote.v1.Anchor.id[acme.remote.v1.audited#0]",
		c.SourceMap.GetEntries()[0].GetDescriptorPath())
	require.Len(t, c.TypeDecls, 1)
	assert.Equal(t, "Token", c.TypeDecls[0].GetName())
	require.Len(t, c.Functions, 1)
	require.Len(t, c.Annotations, 1)

	// A pre-v1.2 descriptor (no extensions) yields the zero carrier.
	plain, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("plain.proto"),
		Package: proto.String("p"),
		Syntax:  proto.String("proto3"),
	}, nil)
	require.NoError(t, err)
	zero := rfc001CarrierFromDescriptor(plain)
	assert.Nil(t, zero.SourceMap)
	assert.Empty(t, zero.TypeDecls)
	assert.Empty(t, zero.Functions)
	assert.Empty(t, zero.Annotations)
}

// Carrier signature rendering matches the AST-rendered card shapes.
func TestRFC001CarrierSignatures(t *testing.T) {
	assert.Equal(t, "audited(by: string, level: acme.Level)",
		carrierAnnotationSignature(&schemav1.AnnotationDecl{
			Name: "audited",
			Params: []*schemav1.AnnotationParam{
				{Name: "by", Type: schemav1.ParamType_STRING},
				{Name: "level", Type: schemav1.ParamType_ENUM_OR_MESSAGE, TypeFqn: "acme.Level"},
			},
		}))
	assert.Equal(t, "required",
		carrierAnnotationSignature(&schemav1.AnnotationDecl{Name: "required"}))
	assert.Equal(t, "Token = string",
		carrierTypeSignature(&schemav1.TypeDecl{Name: "Token", BaseTypeFqn: "string"}))
	assert.Equal(t, "is_token(s: string)",
		carrierFunctionSignature(&schemav1.FunctionDecl{
			Name:   "is_token",
			Params: []*schemav1.FunctionParam{{Name: "s", Type: "string"}},
		}))
}

// A carried function's doc-bearing bracket options — lowered into
// FunctionDecl.options by protocompile v0.20.0+ — render into
// the hover card in the doc-comment slot: deprecation marker first,
// then the description paragraph, then the error code. Non-doc
// options and pre-v0.20.0 carriers (empty options) add nothing.
func TestRFC001CarrierDecl_FunctionOptions(t *testing.T) {
	opts := &descriptorpb.FileOptions{}
	proto.SetExtension(opts, schemav1.E_Functions, &schemav1.FileFunctions{
		Declarations: []*schemav1.FunctionDecl{
			{
				Name:   "is_email",
				Params: []*schemav1.FunctionParam{{Name: "value", Type: "string"}},
				Options: map[string]*schemav1.AnnotationArg{
					"description": {Value: &schemav1.AnnotationArg_StringValue{StringValue: "Validates RFC-5322 addresses."}},
					"deprecated":  {Value: &schemav1.AnnotationArg_BoolValue{BoolValue: true}},
					"error_code":  {Value: &schemav1.AnnotationArg_StringValue{StringValue: "email.invalid"}},
					"openapi_map": {Value: &schemav1.AnnotationArg_StringValue{StringValue: "format=email"}},
				},
				Location: &schemav1.SourceLocation{File: "acme/fns.proto", Line: 9, Column: 10},
			},
			{Name: "bare"},
		},
	})
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    proto.String("acme/fns.proto"),
		Package: proto.String("acme.fns.v1"),
		Syntax:  proto.String("proto3"),
		Options: opts,
	}, nil)
	require.NoError(t, err)

	md, rng, ok := rfc001CarrierDecl(fd, rfc001KindFunction, "is_email", "acme/fns.proto")
	require.True(t, ok)
	assert.Equal(t, "**function is_email(value: string)**\n"+
		"Defined in `acme/fns.proto`\n"+
		"\n*Deprecated.*\n"+
		"\nValidates RFC-5322 addresses.\n"+
		"\nError code: `email.invalid`\n", md,
		"doc options render in order; openapi_map stays out of the card")
	assert.Equal(t, uint32(8), rng.Start.Line,
		"the v0.17.1 declaration-site location becomes the jump target")
	assert.Equal(t, uint32(9), rng.Start.Character)

	md, _, ok = rfc001CarrierDecl(fd, rfc001KindFunction, "bare", "acme/fns.proto")
	require.True(t, ok)
	assert.Equal(t, "**function bare()**\nDefined in `acme/fns.proto`\n", md,
		"empty options add nothing — the pre-v0.20.0 card shape is unchanged")
}

// classicDescriptorDecl resolves classic messages and enums straight
// off the compiled descriptor — nested scopes included, map
// entries skipped — with declaration-site ranges from SourceCodeInfo
// when the descriptor kept it.
func TestClassicDescriptorDecl(t *testing.T) {
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("acme/classic.proto"),
		Package: proto.String("acme.classic.v1"),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Anchor"),
			NestedType: []*descriptorpb.DescriptorProto{
				{Name: proto.String("Inner")},
				{
					Name: proto.String("LabelsEntry"),
					Options: &descriptorpb.MessageOptions{
						MapEntry: proto.Bool(true),
					},
					Field: []*descriptorpb.FieldDescriptorProto{
						{
							Name: proto.String("key"), Number: proto.Int32(1),
							Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
							Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						},
						{
							Name: proto.String("value"), Number: proto.Int32(2),
							Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
							Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
						},
					},
				},
			},
			EnumType: []*descriptorpb.EnumDescriptorProto{{
				Name: proto.String("Mode"),
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: proto.String("MODE_UNSPECIFIED"), Number: proto.Int32(0)},
				},
			}},
		}},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Color"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("COLOR_UNSPECIFIED"), Number: proto.Int32(0)},
			},
		}},
		SourceCodeInfo: &descriptorpb.SourceCodeInfo{
			Location: []*descriptorpb.SourceCodeInfo_Location{
				{Path: []int32{4, 0}, Span: []int32{5, 0, 9, 1}},   // message Anchor
				{Path: []int32{5, 0}, Span: []int32{12, 0, 14, 1}}, // enum Color
			},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	require.NoError(t, err)

	md, rng, ok := classicDescriptorDecl(fd, []string{"Anchor"}, "acme/classic.proto")
	require.True(t, ok, "top-level message resolves")
	assert.Contains(t, md, "**message Anchor**")
	assert.Contains(t, md, "Defined in `acme/classic.proto`")
	assert.Equal(t, uint32(5), rng.Start.Line, "range comes from SourceCodeInfo")
	assert.Equal(t, uint32(9), rng.End.Line)

	md, rng, ok = classicDescriptorDecl(fd, []string{"Color"}, "acme/classic.proto")
	require.True(t, ok, "top-level enum resolves")
	assert.Contains(t, md, "**enum Color**")
	assert.Equal(t, uint32(12), rng.Start.Line)

	// Candidates are rooted: a bare name no longer matches a
	// declaration nested inside another message — protoc would reject
	// the reference, so resolution misses too.
	_, _, ok = classicDescriptorDecl(fd, []string{"Inner"}, "acme/classic.proto")
	assert.False(t, ok, "bare names don't match at depth")
	_, _, ok = classicDescriptorDecl(fd, []string{"Mode"}, "acme/classic.proto")
	assert.False(t, ok, "bare names don't match nested enums either")

	// Outer-qualified scoped paths descend the enclosing-message
	// chain: both the nested message and the nested enum.
	// Without a SourceCodeInfo entry their range degrades to the top
	// of the file.
	md, rng, ok = classicDescriptorDecl(fd, []string{"Anchor.Inner"}, "acme/classic.proto")
	require.True(t, ok, "Outer.Inner scoped path resolves")
	assert.Contains(t, md, "**message Inner**")
	assert.Equal(t, uint32(0), rng.Start.Line)

	md, _, ok = classicDescriptorDecl(fd, []string{"Anchor.Mode"}, "acme/classic.proto")
	require.True(t, ok, "Outer.Enum scoped path resolves")
	assert.Contains(t, md, "**enum Mode**")

	// A later candidate wins after an earlier miss.
	md, _, ok = classicDescriptorDecl(fd, []string{"Missing.Inner", "Anchor.Inner"}, "acme/classic.proto")
	require.True(t, ok, "first matching candidate wins")
	assert.Contains(t, md, "**message Inner**")

	// Synthetic map-entry messages aren't referenceable types.
	_, _, ok = classicDescriptorDecl(fd, []string{"LabelsEntry"}, "acme/classic.proto")
	assert.False(t, ok, "map entries are skipped")
	_, _, ok = classicDescriptorDecl(fd, []string{"Anchor.LabelsEntry"}, "acme/classic.proto")
	assert.False(t, ok, "map entries are skipped on scoped paths too")

	_, _, ok = classicDescriptorDecl(fd, []string{"Missing"}, "acme/classic.proto")
	assert.False(t, ok, "unknown names miss gracefully")
	_, _, ok = classicDescriptorDecl(fd, []string{"Anchor.Missing"}, "acme/classic.proto")
	assert.False(t, ok, "unknown nested names miss gracefully")
	_, _, ok = classicDescriptorDecl(fd, nil, "acme/classic.proto")
	assert.False(t, ok, "no candidates, no match")
}

// carrierRange converts 1-indexed declaration sites to 0-indexed
// 1-character LSP ranges, and degrades missing locations to the
// zero range (editor opens the file at the top).
func TestRFC001CarrierRange(t *testing.T) {
	got := carrierRange(&schemav1.SourceLocation{Line: 6, Column: 12})
	assert.Equal(t, uint32(5), got.Start.Line)
	assert.Equal(t, uint32(11), got.Start.Character)
	assert.Equal(t, uint32(12), got.End.Character)
	assert.Equal(t, carrierRange(nil), carrierRange(&schemav1.SourceLocation{}),
		"nil and zero locations both yield the zero range")
}
