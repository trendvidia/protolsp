package analysis

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

func TestProtovalidateDataDiagnostics_EmptyFindings(t *testing.T) {
	doc := &Document{}
	require.Nil(t, protovalidateDataDiagnostics(doc))
}

func TestProtovalidateDataDiagnostics_RendersFindings(t *testing.T) {
	doc := &Document{
		protovalidateFindings: []protovalidateFinding{
			{
				FieldPath:    "age",
				ConstraintID: "int32.lte",
				Message:      "value must be at most 150",
				Pos:          pxf.Position{Line: 5, Column: 3},
			},
			{
				FieldPath:    "email",
				ConstraintID: "string.email",
				Message:      "value must be a valid email address",
				Pos:          pxf.Position{Line: 7, Column: 1},
			},
		},
	}
	diags := protovalidateDataDiagnostics(doc)
	require.Len(t, diags, 2)
	for _, d := range diags {
		require.Equal(t, "buf/validate", optText(d.Source))
		require.Equal(t, protocol.DiagnosticSeverityError, d.Severity)
	}
	require.Equal(t, protocol.String("int32.lte"), diags[0].Code)
	require.Equal(t, protocol.String("string.email"), diags[1].Code)
}
