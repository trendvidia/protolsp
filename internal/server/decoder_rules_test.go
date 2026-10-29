// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The body's schema errors come from protowire-go's decoder, so the
// editor shows what pxf validate shows. These pin the rules protowire-go
// v1.11.0 and v1.12.0 changed (#303): each failed before its release.
const decoderRulesSchema = `@proto """
syntax = "proto3";
package d.v1;
message M {
  optional int32 n = 1;
  string s = 2;
  map<string, int32> m = 3;
}
"""
@type d.v1.M
`

// null on an optional scalar clears it (protowire-go#182).
func TestDecoderRules_NullOnOptionalScalar(t *testing.T) {
	_, diags := openInline(t, decoderRulesSchema+"n = null\n")
	assert.Empty(t, messages(diags, "protolsp/schema"), "%v", diags)
}

// A singular field bound twice in one body is an error
// (protowire-go#184).
func TestDecoderRules_FieldBoundTwice(t *testing.T) {
	_, diags := openInline(t, decoderRulesSchema+"s = \"a\"\ns = \"b\"\n")
	require.Len(t, messages(diags, "protolsp/schema"), 1, "%v", diags)
}

// A map key bound twice in one body is an error (protowire-go#191,
// v1.12.0).
func TestDecoderRules_MapKeyBoundTwice(t *testing.T) {
	_, diags := openInline(t, decoderRulesSchema+"m = { \"a\": 1 }\nm = { \"a\": 2 }\n")
	require.Len(t, messages(diags, "protolsp/schema"), 1, "%v", diags)
}
