//go:build protocheck

package analysis

import (
	"testing"

	"github.com/stretchr/testify/require"
	protocheck "github.com/trendvidia/protocheck/v2"
)

// The tag-free diagnostics renderer mirrors protocheck/v2's
// CodeFunctionUnimplemented as a string literal so it can stay off
// the tagged-only import graph. This pins the two together.
func TestProtocheckCodeMirrorsV2Constant(t *testing.T) {
	require.Equal(t, protocheck.CodeFunctionUnimplemented, protocheckCodeFunctionUnimplemented)
}
