// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Remote resolution for RFC-001 declarations (#219 Phase C, second
// half) and for classic message/enum type references. The
// AST-first resolvers (proto_rfc001_nav.go, proto_cross_file.go)
// cover files protolsp can parse: the current buffer, on-disk
// imports via the protoindex, and registry files whose source is
// fetchable. This file covers the remainder — an import that
// resolves to a compiled FileDescriptor through the workspace's
// schema lookup but not to any parseable source.
//
// Two layers, best first:
//
//  1. Served source: when the lookup can GetSource the import, parse
//     it (registryImportAST's memoized fetch) and resolve exactly
//     like any other AST file — full doc comments, exact name-token
//     ranges, columns in the negotiated encoding.
//  2. The descriptor itself: descriptor-only files (registry serves
//     the descriptor but not the source — ad-hoc parents, pre-cached
//     deps). Classic messages and enums are real descriptors:
//     fd.Messages()/fd.Enums() find the declaration and
//     SourceLocations().ByDescriptor recovers the declaration site
//     when the descriptor kept its SourceCodeInfo. RFC-001
//     constructs have no descriptor representation; the protowire
//     compiler embeds them in FileOptions extensions
//     (protowire.schema.v1: file-level type_decls / functions /
//     annotation_decls, plus the use-site source_map). Each carried
//     declaration records its Name and its declaration-site
//     SourceLocation, which is what hover + go-to-definition need.
//
// A pre-v1.2 toolchain emits none of the extensions; both layers
// then miss and the caller's AST-only answer stands — the graceful
// degradation #219 requires. Extension payloads decode because this
// package links the gen/protowire/schema/v1 types into the global
// registry: any in-process descriptor unmarshal resolves fields
// 1327–1331 instead of parking them in unknown fields.

package analysis

import (
	"context"
	"fmt"
	"strings"

	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	schemav1 "github.com/trendvidia/protocompile/gen/protowire/schema/v1"
	"github.com/trendvidia/protocompile/source"

	"github.com/trendvidia/protolsp/protoast"
)

// rfc001DeclKind selects which declaration namespace a remote
// resolve searches. Annotations and functions occupy distinct
// syntactic positions, so a reference site always knows its kind.
// rfc001KindType is the type-slot namespace — a field type (or an
// RFC-001 type slot, which resolves identically) can name a classic
// message, a classic enum, or an RFC-001 `type` alias, and the
// remote resolve searches all three.
type rfc001DeclKind int

const (
	rfc001KindAnnotation rfc001DeclKind = iota
	rfc001KindType
	rfc001KindFunction
)

// resolveRFC001RemoteDecl resolves a declaration reference — an
// RFC-001 annotation/function, or (kind rfc001KindType) a
// classic message/enum/type-alias — through the workspace's schema
// lookup, for imports the AST-first resolvers couldn't source.
// Returns the hover markdown, a Location when one is buildable (a
// tracked namespace yields a registry:// target; an untracked
// origin can still hover but has nowhere to jump), and ok=false
// when nothing matched.
//
// Only imports the protoindex does NOT hold participate: an indexed
// import already had its AST-first chance, and re-resolving it here
// could only produce a worse answer.
//
// refScope is the reference site's lexical scope (protoRefScopeAt,
// #239), consumed by the type-slot kind only; "" falls back to the
// file's package. Annotation/function references pass "".
func (s *Workspace) resolveRFC001RemoteDecl(ctx context.Context, wsDir string, currentFile *protoast.File, kind rfc001DeclKind, name, refScope string) (string, *protocol.Location, bool) {
	if currentFile == nil || name == "" {
		return "", nil, false
	}
	lk := s.lookupForNamespaceInWorkspace("", wsDir)
	if lk == nil {
		return "", nil, false
	}
	if refScope == "" {
		refScope = filePackage(currentFile)
	}
	idx := s.SnapshotProtoIndex()
	for _, imp := range protoImports(currentFile) {
		if idx.Lookup(imp) != nil {
			continue
		}
		fd, origin, err := lk.FindFileByPathWithOrigin(imp)
		if err != nil || fd == nil {
			continue
		}
		// Candidate interpretations of the name inside this file's
		// package: type references may carry enclosing-message
		// segments; annotations and functions are top-level
		// only, so just the undotted package interpretations apply
		// — a bare name doesn't leak across packages.
		var scoped []string
		if kind == rfc001KindType {
			scoped = protoScopedCandidates(name, refScope, string(fd.Package()))
		} else {
			scoped = protoTopLevelNameCandidates(name, refScope, string(fd.Package()))
		}
		if len(scoped) == 0 {
			continue
		}
		// Layer 1 — served source. Memoized per lookup epoch; a
		// GetSource miss is cached too, so descriptor-only files
		// don't re-fetch per request. Needs a tracked namespace:
		// the parse cache and the result URI are both keyed by it.
		if origin != "" {
			if remote := s.registryImportAST(ctx, origin, wsDir, imp); remote != nil {
				if kind == rfc001KindType {
					if hit := findTypeByScopedCandidates(remote, scoped); !hit.IsZero() {
						return renderProtoTypeLookupHover(hit),
							registryProtoLocation(origin, imp, remote, hit.Node(), s.positionEncoding), true
					}
				} else {
					for _, leaf := range scoped {
						if md, node, ok := rfc001DeclCard(remote, kind, leaf); ok {
							return md, registryProtoLocation(origin, imp, remote, node, s.positionEncoding), true
						}
					}
				}
				continue
			}
		}
		// Layer 2 — the descriptor. Classic messages/enums are real
		// descriptors and are tried first, mirroring layer 1's
		// (and resolveProtoTypeRef's) message → enum → alias priority;
		// RFC-001 declarations live in the carrier options. Carrier
		// declarations are top-level only, so for type references only
		// a bare (undotted) candidate may name one — a dotted scoped
		// path can never be an alias.
		var (
			md  string
			rng protocol.Range
			ok  bool
		)
		if kind == rfc001KindType {
			md, rng, ok = classicDescriptorDecl(fd, scoped, imp)
			if !ok {
				for _, c := range scoped {
					if !strings.Contains(c, ".") {
						md, rng, ok = rfc001CarrierDecl(fd, kind, c, imp)
						break
					}
				}
			}
		} else {
			for _, leaf := range scoped {
				if md, rng, ok = rfc001CarrierDecl(fd, kind, leaf, imp); ok {
					break
				}
			}
		}
		if !ok {
			continue
		}
		var loc *protocol.Location
		if origin != "" {
			loc = &protocol.Location{
				URI:   protocol.DocumentURI(BuildRegistryURI(origin, imp)),
				Range: rng,
			}
		}
		return md, loc, true
	}
	return "", nil, false
}

// rfc001RemoteHover is the hover-shaped wrapper around
// resolveRFC001RemoteDecl: markdown → *protocol.Hover, misses → nil.
func (s *Workspace) rfc001RemoteHover(wsDir string, currentFile *protoast.File, kind rfc001DeclKind, name, refScope string) *protocol.Hover {
	md, _, ok := s.resolveRFC001RemoteDecl(context.Background(), wsDir, currentFile, kind, name, refScope)
	if !ok {
		return nil
	}
	return &protocol.Hover{Contents: protocol.MarkupContent{Kind: protocol.Markdown, Value: md}}
}

// rfc001RemoteLocation is the definition-shaped wrapper: a resolved
// declaration in an untracked origin still returns nil (there is no
// URI to jump to), matching "no definition" in the editor.
func (s *Workspace) rfc001RemoteLocation(wsDir string, currentFile *protoast.File, kind rfc001DeclKind, name, refScope string) *protocol.Location {
	_, loc, ok := s.resolveRFC001RemoteDecl(context.Background(), wsDir, currentFile, kind, name, refScope)
	if !ok {
		return nil
	}
	return loc
}

// rfc001DeclCard finds `leaf` among f's declarations of the given
// kind and returns the rendered hover card plus the name node (the
// go-to-definition target). Annotations and functions only — the
// type-slot kind resolves through findTypeByScopedCandidates instead
// , whose lookup renders via renderProtoTypeLookupHover.
func rfc001DeclCard(f *protoast.File, kind rfc001DeclKind, leaf string) (string, source.Spanner, bool) {
	switch kind {
	case rfc001KindAnnotation:
		if d := findAnnotationByName(f, leaf); !d.IsZero() {
			return renderProtoAnnotationHover(f, d).Contents.Value, d.Name(), true
		}
	case rfc001KindFunction:
		if d := findFunctionByName(f, leaf); !d.IsZero() {
			return renderProtoFunctionHover(f, d).Contents.Value, d.Name(), true
		}
	}
	return "", nil, false
}

// rfc001Carrier is the RFC-001 payload read from a compiled file's
// FileOptions. Zero-valued fields mean the extension wasn't present
// (classic proto, or a pre-v1.2 toolchain).
type rfc001Carrier struct {
	SourceMap   *schemav1.SourceMap
	TypeDecls   []*schemav1.TypeDecl
	Functions   []*schemav1.FunctionDecl
	Annotations []*schemav1.AnnotationDecl
}

// rfc001CarrierFromDescriptor extracts the RFC-001 extensions from a
// FileDescriptor's options. Descriptors whose options aren't the
// concrete descriptorpb type (synthetic descriptors) yield the zero
// carrier.
func rfc001CarrierFromDescriptor(fd protoreflect.FileDescriptor) rfc001Carrier {
	opts, ok := fd.Options().(*descriptorpb.FileOptions)
	if !ok || opts == nil {
		return rfc001Carrier{}
	}
	var c rfc001Carrier
	if sm, ok := proto.GetExtension(opts, schemav1.E_SourceMap).(*schemav1.SourceMap); ok {
		c.SourceMap = sm
	}
	if td, ok := proto.GetExtension(opts, schemav1.E_TypeDecls).(*schemav1.FileTypeDecls); ok && td != nil {
		c.TypeDecls = td.GetDeclarations()
	}
	if fns, ok := proto.GetExtension(opts, schemav1.E_Functions).(*schemav1.FileFunctions); ok && fns != nil {
		c.Functions = fns.GetDeclarations()
	}
	if ad, ok := proto.GetExtension(opts, schemav1.E_AnnotationDecls).(*schemav1.FileAnnotationDecls); ok && ad != nil {
		c.Annotations = ad.GetDeclarations()
	}
	return c
}

// rfc001CarrierDecl finds `leaf` among the carrier declarations of
// the given kind and renders a hover card + declaration-site range
// from the carried metadata. The card has no doc comment — compiled
// options don't carry one — but the signature and provenance match
// the AST-rendered cards. Function cards additionally render the
// doc-bearing bracket options a v0.20.0+ compiler lowers into
// FunctionDecl.options.
func rfc001CarrierDecl(fd protoreflect.FileDescriptor, kind rfc001DeclKind, leaf, importPath string) (string, protocol.Range, bool) {
	c := rfc001CarrierFromDescriptor(fd)
	var (
		signature string
		loc       *schemav1.SourceLocation
		kindWord  string
		doc       string
	)
	switch kind {
	case rfc001KindAnnotation:
		for _, d := range c.Annotations {
			if d.GetName() == leaf {
				signature, loc, kindWord = carrierAnnotationSignature(d), d.GetLocation(), "annotation"
				break
			}
		}
	case rfc001KindType:
		for _, d := range c.TypeDecls {
			if d.GetName() == leaf {
				signature, loc, kindWord = carrierTypeSignature(d), d.GetLocation(), "type"
				break
			}
		}
	case rfc001KindFunction:
		for _, d := range c.Functions {
			if d.GetName() == leaf {
				signature, loc, kindWord = carrierFunctionSignature(d), d.GetLocation(), "function"
				doc = carrierFunctionDoc(d.GetOptions())
				break
			}
		}
	}
	if signature == "" {
		return "", protocol.Range{}, false
	}
	md := fmt.Sprintf("**%s %s**\nDefined in `%s`\n", kindWord, signature, importPath) + doc
	return md, carrierRange(loc), true
}

// carrierFunctionDoc renders the documentation-bearing bracket
// options of a carried function declaration — `deprecated`,
// `description`, `error_code` — in the slot the AST-rendered card
// gives the leading doc comment. Other options (openapi_map and
// friends) are engine vocabulary, not documentation, and stay out of
// the card. Empty until compiled by protocompile v0.20.0+; earlier
// carriers left FunctionDecl.options unpopulated.
func carrierFunctionDoc(opts map[string]*schemav1.AnnotationArg) string {
	if len(opts) == 0 {
		return ""
	}
	var b strings.Builder
	if opts["deprecated"].GetBoolValue() {
		b.WriteString("\n*Deprecated.*\n")
	}
	if desc := opts["description"].GetStringValue(); desc != "" {
		b.WriteString("\n" + desc + "\n")
	}
	if code := opts["error_code"].GetStringValue(); code != "" {
		b.WriteString("\nError code: `" + code + "`\n")
	}
	return b.String()
}

// carrierRange converts a carried declaration-site SourceLocation
// (1-indexed line, 1-indexed column in Unicode code points) to a
// 1-character LSP range. Without the source text there is nothing
// to re-count the column against, so the code-point count is used
// verbatim — exact under UTF-16 for BMP-only lines (all of ASCII),
// approximate past a non-BMP character. Text-backed resolutions
// never reach this: layer 1 re-parses and converts exactly under
// the negotiated encoding.
func carrierRange(loc *schemav1.SourceLocation) protocol.Range {
	if loc == nil || loc.GetLine() <= 0 {
		return protocol.Range{}
	}
	line := uint32(loc.GetLine() - 1) // #nosec G115 -- 1-indexed proto source lines
	col := uint32(0)
	if loc.GetColumn() > 0 {
		col = uint32(loc.GetColumn() - 1) // #nosec G115
	}
	return protocol.Range{
		Start: protocol.Position{Line: line, Character: col},
		End:   protocol.Position{Line: line, Character: col + 1},
	}
}

// carrierAnnotationSignature renders `name(param: type, …)` from a
// carried annotation declaration, matching the AST-rendered shape.
func carrierAnnotationSignature(d *schemav1.AnnotationDecl) string {
	if len(d.GetParams()) == 0 {
		return d.GetName()
	}
	parts := make([]string, 0, len(d.GetParams()))
	for _, p := range d.GetParams() {
		parts = append(parts, p.GetName()+": "+carrierParamTypeLabel(p))
	}
	return d.GetName() + "(" + strings.Join(parts, ", ") + ")"
}

// carrierParamTypeLabel renders an annotation parameter's type: the
// FQN when the parameter takes an enum/message, else the scalar
// keyword derived from the ParamType enum name (STRING → string).
func carrierParamTypeLabel(p *schemav1.AnnotationParam) string {
	if fqn := p.GetTypeFqn(); fqn != "" {
		return fqn
	}
	return strings.ToLower(strings.TrimPrefix(p.GetType().String(), "PARAM_TYPE_"))
}

// carrierTypeSignature renders `Name = base.type.Fqn` from a carried
// type-alias declaration.
func carrierTypeSignature(d *schemav1.TypeDecl) string {
	if base := d.GetBaseTypeFqn(); base != "" {
		return d.GetName() + " = " + base
	}
	return d.GetName()
}

// carrierFunctionSignature renders `name(param: type, …)` from a
// carried function declaration. Function parameter types are plain
// strings in the carrier (the RFC-001 grammar allows engine types).
func carrierFunctionSignature(d *schemav1.FunctionDecl) string {
	parts := make([]string, 0, len(d.GetParams()))
	for _, p := range d.GetParams() {
		parts = append(parts, p.GetName()+": "+p.GetType())
	}
	return d.GetName() + "(" + strings.Join(parts, ", ") + ")"
}

// classicDescriptorDecl finds the first scoped candidate among fd's
// message and enum descriptors and renders a hover card +
// declaration-site range. Every candidate descends the enclosing-
// message chain from the file's top level (#241 — the descriptor
// twin of findTypeByScopedCandidates' rooted matching). The card has
// no doc comment — none survives without SourceCodeInfo, and hovers
// don't re-derive one from it — but the shape matches the AST-
// rendered message/enum cards. The range comes from
// SourceLocations().ByDescriptor when the descriptor kept its
// SourceCodeInfo; without it the zero SourceLocation degrades to the
// top of the file, same as the pxf locationForDescriptor path.
func classicDescriptorDecl(fd protoreflect.FileDescriptor, candidates []string, importPath string) (string, protocol.Range, bool) {
	for _, scoped := range candidates {
		d, kindWord := descriptorByScopedPath(fd, scoped)
		if d == nil {
			continue
		}
		md := fmt.Sprintf("**%s %s**\nDefined in `%s`\n", kindWord, d.Name(), importPath)
		return md, rangeFromSourceLocation(fd.SourceLocations().ByDescriptor(d)), true
	}
	return "", protocol.Range{}, false
}

// descriptorByScopedPath descends fd's message descriptors along a
// dotted scope path rooted at the file's top level: every segment
// but the last names an enclosing message, the last names a message
// or enum nested directly inside it. The descriptor twin of
// findDeclByScopedPath. Returns (nil, "") when any segment misses.
func descriptorByScopedPath(fd protoreflect.FileDescriptor, scoped string) (protoreflect.Descriptor, string) {
	segs := strings.Split(scoped, ".")
	var parent protoreflect.MessageDescriptor
	msgs := fd.Messages()
	for _, seg := range segs[:len(segs)-1] {
		m := msgs.ByName(protoreflect.Name(seg))
		if m == nil || m.IsMapEntry() {
			return nil, ""
		}
		parent = m
		msgs = m.Messages()
	}
	last := protoreflect.Name(segs[len(segs)-1])
	if m := msgs.ByName(last); m != nil && !m.IsMapEntry() {
		return m, "message"
	}
	enums := fd.Enums()
	if parent != nil {
		enums = parent.Enums()
	}
	if e := enums.ByName(last); e != nil {
		return e, "enum"
	}
	return nil, ""
}
