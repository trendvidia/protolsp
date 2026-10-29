//go:build protocheck

// Trendvidia build's Phase 1 runtime validation against the
// proprietary protocheck/v2 library. The OSS counterpart in
// protocheck_data_stub.go returns nil findings; only the tagged
// files import github.com/trendvidia/protocheck/v2.

package analysis

import (
	"errors"

	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/proto"

	protocheck "github.com/trendvidia/protocheck/v2"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// runProtocheck invokes the server's validator against an
// unmarshalled message and maps each violation's field path to a
// source position via the parsed AST. Caller holds doc.mu (same
// locking discipline as the rest of validate()). The validator
// itself is safe to call from any goroutine; protocheckValidate
// pins the current validator for the duration of the call so a
// concurrent rebuild can't close it mid-eval.
func (s *Workspace) runProtocheck(doc *Document, msg proto.Message) []protocheckFinding {
	err, ok := s.protocheckValidate(msg)
	if !ok || err == nil {
		return nil
	}
	var verr *protocheck.ValidationError
	if !errors.As(err, &verr) {
		// protocheck contract guarantees ValidationError; anything
		// else is an unexpected wrapping we'd rather log and drop
		// than surface as a bogus diagnostic.
		s.log.Warn("protocheck.Validate returned unexpected error type",
			"uri", doc.URI, "err", err.Error())
		return nil
	}
	var entries []pxf.Entry
	if doc.Parsed != nil {
		entries = doc.Parsed.Entries
	}
	// §7 i18n: when the client sent a locale and the engine config
	// loaded a catalog for it, violation messages render through
	// the catalog (code + params → template); a miss — no catalog,
	// no entry for the code — falls back to the spec message.
	catalog, haveCatalog := s.protocheckRenderCatalog()
	findings := make([]protocheckFinding, 0, len(verr.Violations))
	for _, v := range verr.Violations {
		if v == nil || v.Cause == nil {
			continue
		}
		msg := v.Cause.FallbackMessage
		if haveCatalog {
			if rendered, ok := catalog.Render(v.Cause.Code, v.Cause.Params); ok {
				msg = rendered
			}
		}
		path := v.Path.String()
		pos, _ := resolveProtocheckPath(entries, path)
		findings = append(findings, protocheckFinding{
			FieldPath:    path,
			ConstraintID: v.Cause.Code,
			Message:      msg,
			ForKey:       v.ForKey,
			Pos:          pos,
			Rule:         s.resolveRuleLocation(doc, v),
		})
	}
	return findings
}

// resolveRuleLocation maps a violation's schema-side enrichment —
// SourceRef (the §8.3.1 rule identity) and Source (the declaration
// site) — to an editor location for "go to rule" related
// information (#268). Only RFC-001 carrier rules carry these today;
// protocheckpb Check rules yield nil and the finding renders
// without a link.
//
// The URI resolves through the same precedence go-to-definition
// uses (proto_files local path → workspace file → registry virtual
// doc → internal), which is safe under doc.mu — the definition
// handler calls it under the same lock. The range comes from the
// declaration-site Source (1-based; rendered as a point). When the
// rule's file is open in the editor, joining
// SourceRef.DescriptorPath through that document's rfc001SourceMap
// would give a full token range like the #262 overlay — deferred:
// it needs the OTHER document's lock under this one's, an ordering
// the workspace doesn't currently define.
func (s *Workspace) resolveRuleLocation(doc *Document, v *protocheck.EnrichedViolation) *protocol.Location {
	var file string
	if v.SourceRef != nil {
		file = v.SourceRef.File
	}
	if file == "" && v.Source != nil {
		file = v.Source.File
	}
	if file == "" {
		return nil
	}
	target, ok := s.resolveDefinitionURI(doc, file)
	if !ok {
		return nil
	}
	var rng protocol.Range
	if v.Source != nil && v.Source.Line > 0 {
		// Source is 1-based; Column counts bytes while LSP wants
		// the negotiated encoding — acceptable drift for a point
		// anchor (constraint declarations are overwhelmingly ASCII
		// up to the rule).
		pos := protocol.Position{Line: uint32(v.Source.Line - 1)}
		if v.Source.Column > 0 {
			pos.Character = uint32(v.Source.Column - 1)
		}
		rng = protocol.Range{Start: pos, End: pos}
	}
	return &protocol.Location{URI: target, Range: rng}
}
