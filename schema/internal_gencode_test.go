// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package schema

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	pxfpb "github.com/trendvidia/protolsp/proto/pxf"
)

// TestInternalGencode_MatchesSourceAnnotations checks the generated
// Go descriptors against the embedded .proto sources they were
// generated from: every field's (pxf.required) must agree. A
// chain.pb.go generated before pxf/annotations.proto renumbered its
// extensions (50000 → 1314) carried the option under a number
// nothing registers, so the Go descriptor reported no required
// fields (#12). This fails the next time a .pb.go goes stale that
// way.
func TestInternalGencode_MatchesSourceAnnotations(t *testing.T) {
	files, err := compileInternalProtos()
	require.NoError(t, err)

	checked, required := 0, 0
	for _, src := range files {
		var walk func(protoreflect.MessageDescriptors)
		walk = func(msgs protoreflect.MessageDescriptors) {
			for i := 0; i < msgs.Len(); i++ {
				md := msgs.Get(i)
				walk(md.Messages())
				d, err := protoregistry.GlobalFiles.FindDescriptorByName(md.FullName())
				require.NoError(t, err, "%s: no generated descriptor", md.FullName())
				gen := d.(protoreflect.MessageDescriptor)
				fields := md.Fields()
				for j := 0; j < fields.Len(); j++ {
					fd := fields.Get(j)
					genFD := gen.Fields().ByNumber(fd.Number())
					require.NotNil(t, genFD, "%s: missing from the generated descriptor", fd.FullName())
					want := pxfRequired(t, fd)
					assert.Equal(t, want, pxfRequired(t, genFD), "%s: (pxf.required) differs between source and gencode", fd.FullName())
					checked++
					if want {
						required++
					}
				}
			}
		}
		walk(src.Messages())
	}
	// A positive marker that the walk compared something real: the
	// sources declare (pxf.required) on 8 chameleon.v1 fields.
	assert.Equal(t, 8, required, "required fields seen in the sources")
	assert.Greater(t, checked, required)
}

// pxfRequired reads (pxf.required) off a field. The options are
// re-decoded against the global registry: a descriptor compiled from
// source carries its own extension types, which the generated
// E_Required does not match.
func pxfRequired(t *testing.T, fd protoreflect.FieldDescriptor) bool {
	t.Helper()
	raw, err := proto.Marshal(fd.Options())
	require.NoError(t, err)
	opts := &descriptorpb.FieldOptions{}
	require.NoError(t, proto.UnmarshalOptions{Resolver: protoregistry.GlobalTypes}.Unmarshal(raw, opts))
	return proto.GetExtension(opts, pxfpb.E_Required).(bool)
}
