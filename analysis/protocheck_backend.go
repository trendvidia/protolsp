// The seam that lets protocheck's expression engine live outside this
// module. Every engine protolsp can use is private, and a go.mod
// requirement on one breaks `go mod tidy` for anyone without access
// (#30). So this module never names it: the nested module at govm/
// registers a backend from an init function, and its binaries link it
// with a blank import. Without a backend, protocheck validation is
// off, which is the community build.

package analysis

import (
	"go.lsp.dev/protocol"

	protocheck "github.com/trendvidia/protocheck/v2"

	"github.com/trendvidia/protolsp/positions"
	"github.com/trendvidia/protolsp/protoast"
)

// ProtocheckBackend supplies the parts of protocheck validation that
// need an expression engine.
type ProtocheckBackend struct {
	// NewEngine constructs the engine that evaluates expressions
	// while validating .pxf data. Called once per validator build.
	NewEngine func() (protocheck.ExpressionEngine, error)
	// CheckExpressions returns diagnostics for broken protocheck
	// expressions in a parsed .proto file, with ranges in enc. It
	// receives a nil file when the parser bailed completely.
	CheckExpressions func(file *protoast.File, enc positions.Encoding) []protocol.Diagnostic
}

// protocheckBackend is written only by RegisterProtocheckBackend,
// before any Workspace exists, so reads need no lock.
var protocheckBackend ProtocheckBackend

// RegisterProtocheckBackend installs b for every Workspace created
// after it. Call it from an init function.
func RegisterProtocheckBackend(b ProtocheckBackend) {
	protocheckBackend = b
}

// HasProtocheckBackend reports whether a backend is registered.
func HasProtocheckBackend() bool {
	return protocheckBackend.NewEngine != nil
}

// analyzeProtocheckExpressions returns the backend's diagnostics for
// the file, or none with no backend.
func analyzeProtocheckExpressions(file *protoast.File, enc positions.Encoding) []protocol.Diagnostic {
	if protocheckBackend.CheckExpressions == nil {
		return nil
	}
	return protocheckBackend.CheckExpressions(file, enc)
}
