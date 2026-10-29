// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// TestFormatValueRepr_AllValueKinds is TC6 from the audit
// roadmap: formatValueRepr's non-scalar branches (Ident,
// Timestamp, Duration, Bytes, Null, List, Block) were
// unexercised by existing tests. Merged-value hover rendering
// would have silently fallen through to the "<value>" default
// case for those types on the C6 path.
func TestFormatValueRepr_AllValueKinds(t *testing.T) {
	cases := []struct {
		name string
		v    pxf.Value
		want string
	}{
		{"string is quoted", &pxf.StringVal{Value: `hi`}, `"hi"`},
		{"int returns raw", &pxf.IntVal{Raw: "42"}, "42"},
		{"float returns raw", &pxf.FloatVal{Raw: "3.14"}, "3.14"},
		{"float keeps its precision suffix", &pxf.FloatVal{Raw: "0.1", Prec: "53"}, "0.1p53"},
		{"int keeps its precision suffix", &pxf.IntVal{Raw: "1", Prec: "64"}, "1p64"},
		{"bool true", &pxf.BoolVal{Value: true}, "true"},
		{"bool false", &pxf.BoolVal{Value: false}, "false"},
		{"bytes is placeholder", &pxf.BytesVal{Value: []byte("x")}, "<bytes>"},
		{"null", &pxf.NullVal{}, "null"},
		{"ident returns name", &pxf.IdentVal{Name: "ENUM_VAL"}, "ENUM_VAL"},
		{"timestamp returns raw", &pxf.TimestampVal{Raw: "2026-05-18T00:00:00Z"}, "2026-05-18T00:00:00Z"},
		{"duration returns raw", &pxf.DurationVal{Raw: "30s"}, "30s"},
		{"list flattens to [...]", &pxf.ListVal{Elements: []pxf.Value{&pxf.IntVal{Raw: "1"}}}, "[...]"},
		{"block flattens to {...}", &pxf.BlockVal{}, "{...}"},
		// The `default: return "<value>"` branch is structurally
		// unreachable — pxf.Value has unexported methods, so every
		// implementer lives inside protowire-go and the switch above
		// enumerates all of them. Adding a Nil pxf.Value would also
		// crash on the type switch before reaching the default. Left
		// as defensive code; no test target.
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, formatValueRepr(c.v))
		})
	}
}
