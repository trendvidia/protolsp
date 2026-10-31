package analysis

import (
	"testing"

	"github.com/stretchr/testify/require"
	protocheck "github.com/trendvidia/protocheck/v2"
)

// The diagnostics renderer mirrors protocheck/v2's
// CodeFunctionUnimplemented as a string literal. This pins the two
// together.
func TestProtocheckCodeMirrorsV2Constant(t *testing.T) {
	require.Equal(t, protocheck.CodeFunctionUnimplemented, protocheckCodeFunctionUnimplemented)
}
