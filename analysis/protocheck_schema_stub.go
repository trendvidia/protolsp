//go:build !protocheck

// Community (OSS) build's no-op stub for Phase 0 protocheck
// expression validation. The real implementation in
// protocheck_schema_real.go uses the protocheck expression engine to type-check
// `(protocheck.field|message).check[].expression` strings; the
// OSS build returns no diagnostics here. Phase 0 for the
// protovalidate side (CEL expressions on
// `(buf.validate.*).cel`) lives in protovalidate_schema.go and
// is always compiled.

package analysis

import (
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

func analyzeProtocheckExpressions(_ *protoast.File, _ positions.Encoding) []protocol.Diagnostic {
	return nil
}
