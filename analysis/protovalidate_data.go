// Phase 1 of the protovalidate → protolsp integration: runtime
// validation of .pxf data against Buf's `(buf.validate.*)`
// annotations. Mirror of the protocheck data validator; same
// shape, same emission pipeline, distinct diagnostic source
// ("buf/validate") so editors can color / filter independently.
//
// Always tag-free since protovalidate-go is Apache 2.0 — this is
// the open-source path that ships in both the community and
// trendvidia builds of protolsp.
//
// Path-resolution semantics match protocheck's: `FieldPathString`
// produces the same dotted + bracketed form ("address.zip_code",
// "items[3].sku", `labels["env"].value`) so the existing
// resolveProtocheckPath handles both engines without duplication.
package analysis

import (
	"errors"

	"buf.build/go/protovalidate"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/proto"

	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// protovalidateFinding mirrors protocheckFinding for the
// protovalidate side. Separate type keeps the diagnostic sources
// + emission paths cleanly distinguished and leaves room for
// engine-specific metadata (e.g. CEL expression text, rule
// descriptor) to diverge in the future without breaking the
// protocheck path.
type protovalidateFinding struct {
	FieldPath    string
	ConstraintID string
	Message      string
	Pos          pxf.Position
}

// runProtovalidate invokes the protovalidate validator against
// an unmarshalled message and maps each violation's FieldPath to
// a source position via the parsed pxf AST. Caller holds doc.mu
// (same locking discipline as the rest of validate()).
// protovalidate.Validator is safe to call from any goroutine and
// caches compiled CEL programs across calls.
func (s *Workspace) runProtovalidate(doc *Document, msg proto.Message) []protovalidateFinding {
	if s.protovalidate == nil || msg == nil {
		return nil
	}
	err := s.protovalidate.Validate(msg)
	if err == nil {
		return nil
	}
	var verr *protovalidate.ValidationError
	if !errors.As(err, &verr) {
		// protovalidate also reports CompilationError /
		// RuntimeError for genuinely-broken constraint setups;
		// log + drop those — they're not data violations and
		// would surface as misleading diagnostics on the .pxf.
		s.log.Warn("protovalidate.Validate returned non-violation error",
			"uri", doc.URI, "err", err.Error())
		return nil
	}
	var entries []pxf.Entry
	if doc.Parsed != nil {
		entries = doc.Parsed.Entries
	}
	findings := make([]protovalidateFinding, 0, len(verr.Violations))
	for _, v := range verr.Violations {
		path := protovalidate.FieldPathString(v.Proto.GetField())
		pos, _ := resolveProtocheckPath(entries, path)
		findings = append(findings, protovalidateFinding{
			FieldPath:    path,
			ConstraintID: v.Proto.GetRuleId(),
			Message:      v.Proto.GetMessage(),
			Pos:          pos,
		})
	}
	return findings
}

// protovalidateDataDiagnostics renders the stored findings as
// LSP diagnostics with source "buf/validate".
func protovalidateDataDiagnostics(doc *Document) []protocol.Diagnostic {
	if len(doc.protovalidateFindings) == 0 {
		return nil
	}
	out := make([]protocol.Diagnostic, 0, len(doc.protovalidateFindings))
	for _, f := range doc.protovalidateFindings {
		out = append(out, protocol.Diagnostic{
			Range:    pxfPosToRange(f.Pos),
			Severity: protocol.DiagnosticSeverityError,
			Source:   optString("buf/validate"),
			Code:     protocol.String(f.ConstraintID),
			Message:  protocol.String(f.Message),
		})
	}
	return out
}
