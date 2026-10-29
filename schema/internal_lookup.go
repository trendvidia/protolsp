// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Internal descriptors shipped with protolsp itself: the schemas
// users edit when they open `.protolsp.pxf` or `chain.pxf`. Without
// these, the LSP couldn't resolve `protolsp.v1.WorkspaceConfig` /
// `chameleon.v1.ChainSpec` from the user's registry (which has no
// reason to publish them), so opening either file produced a
// spurious "unknown type" diagnostic.
//
// The descriptors are generated from `proto/protolsp/v1/` and the
// vendored `proto/chameleon/v1/` via `buf generate`; the generated
// .pb.go files self-register into protoregistry.GlobalFiles at
// init(). This file walks that registry to expose the relevant
// types as a schema.Lookup.
//
// Why blank-import the generated packages: protoregistry.GlobalFiles
// only contains types from packages compiled into the binary. The
// blank imports below force the link.

package schema

import (
	"bytes"
	"context"
	"fmt"

	"github.com/trendvidia/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	internalproto "github.com/trendvidia/protolsp/proto"

	// Force-link the generated descriptor packages so any consumer
	// of GlobalTypes (e.g. logging, debug tools) still finds them.
	// The internal lookup's own FindMessageByName no longer depends
	// on GlobalFiles, since those descriptors are stripped of
	// SourceCodeInfo and produce go-to-def Locations that land at
	// the top of the .proto file.
	_ "github.com/trendvidia/protolsp/proto/chameleon/v1"
	_ "github.com/trendvidia/protolsp/proto/protolsp/v1"
)

// internalProtoFilePaths enumerates the .proto files whose
// descriptors the InternalLookup exposes. Used as a filter when
// walking protoregistry.GlobalFiles so we only surface protolsp's
// own types — not every protobuf type in the binary
// (descriptorpb, struct.proto, etc.).
//
// The vendored pxf/annotations.proto isn't included: it only
// declares FieldOptions extensions, not messages users would
// ever bind a .pxf document to via @type.
var internalProtoFilePaths = []string{
	"protolsp/v1/workspace_config.proto",
	"chameleon/v1/chain.proto",
}

// InternalOrigin is the namespace ID FindFileByPathWithOrigin
// returns for files served by the internal lookup. Distinct from
// "" (which means "ad-hoc parent, untracked") and from real
// registry namespace IDs (which are reverse-DNS by convention).
// Editors can render "from protolsp" instead of a registry name
// when they recognize this constant.
//
// URL-safe by design: this value rides inside the authority
// segment of a `registry://<origin>/<path>` URI when go-to-
// definition lands on an internally-shipped type, so it must
// survive percent-encoding round-trips through the renderer.
const InternalOrigin = "_internal_"

// internalLookup is the schema.Lookup backed by protolsp's
// internal descriptors. Stateless; safe to share across all Server
// instances in the process. Callers typically obtain one via
// [NewInternalLookup] at server construction and consult it as a
// fallback when the user's lookup can't resolve a name.
type internalLookup struct {
	// files holds the FileDescriptors for the internal .proto
	// files. Compiled from the embedded .proto sources via
	// protocompile so SourceLocations() returns real positions —
	// the GlobalFiles-registered descriptors strip SourceCodeInfo
	// and would land go-to-def at line 0 col 0.
	files []protoreflect.FileDescriptor
}

// NewInternalLookup returns the internal-descriptor lookup. Always
// non-nil, always serves the same set of types regardless of
// runtime config — these are baked into the binary at build time.
//
// Safe to call before any Server is constructed. Callers may
// share the result freely; the underlying state is read-only.
//
// Implementation: compiles the embedded .proto sources via
// protocompile with SourceInfoStandard so go-to-def lands at the
// definition's actual line/column instead of (0, 0). If the
// compile fails for any reason (shouldn't happen — the source
// is shipped with the binary and verified at build time) the
// lookup falls back to an empty slice; FindMessageByName returns
// NotFound for everything and feature-flagged callers degrade
// gracefully.
func NewInternalLookup() Lookup {
	files, err := compileInternalProtos()
	if err != nil {
		return &internalLookup{}
	}
	return &internalLookup{files: files}
}

// compileInternalProtos runs the embedded .proto sources through
// protocompile against an embed.FS-backed resolver. Returns only
// the files listed in internalProtoFilePaths — pxf/annotations.proto
// is compiled to satisfy the import but not exposed as an @type
// target.
func compileInternalProtos() ([]protoreflect.FileDescriptor, error) {
	allPaths := append([]string{}, internalProtoFilePaths...)
	allPaths = append(allPaths, "pxf/annotations.proto")

	compiler := &protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(
			protocompile.ResolverFunc(func(path string) (protocompile.SearchResult, error) {
				data, rerr := internalproto.Sources.ReadFile(path)
				if rerr != nil {
					return protocompile.SearchResult{}, protoregistry.NotFound
				}
				return protocompile.SearchResult{Source: bytes.NewReader(data)}, nil
			}),
		),
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	linked, err := compiler.Compile(context.Background(), allPaths...)
	if err != nil {
		return nil, fmt.Errorf("compiling internal protos: %w", err)
	}
	var out []protoreflect.FileDescriptor
	keep := make(map[string]struct{}, len(internalProtoFilePaths))
	for _, p := range internalProtoFilePaths {
		keep[p] = struct{}{}
	}
	for _, lf := range linked {
		if lf == nil {
			continue
		}
		if _, ok := keep[lf.Path()]; !ok {
			continue
		}
		out = append(out, lf)
	}
	return out, nil
}

// FindMessageByName walks the internal files for a top-level
// message whose FullName equals `name`. Nested messages aren't
// searched (none in our schemas today); list the top level only
// for predictability.
//
// Wrapping the descriptor via dynamicpb (instead of returning the
// GlobalTypes-registered MessageType) keeps SourceCodeInfo
// reachable through mt.Descriptor().ParentFile().SourceLocations()
// — the GlobalTypes path strips it and breaks go-to-def cursor
// placement for internal types.
func (l *internalLookup) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	if l == nil {
		return nil, protoregistry.NotFound
	}
	for _, file := range l.files {
		messages := file.Messages()
		for i := 0; i < messages.Len(); i++ {
			md := messages.Get(i)
			if md.FullName() == name {
				return dynamicpb.NewMessageType(md), nil
			}
		}
	}
	return nil, protoregistry.NotFound
}

// RangeMessages iterates every top-level message across the
// internal files. Used by completion for the `@type` directive —
// users opening a fresh `.pxf` with no @type can complete to
// `protolsp.v1.WorkspaceConfig` even when no registry is bound.
func (l *internalLookup) RangeMessages(fn func(protoreflect.MessageType) bool) {
	if l == nil {
		return
	}
	for _, file := range l.files {
		messages := file.Messages()
		for i := 0; i < messages.Len(); i++ {
			md := messages.Get(i)
			if !fn(dynamicpb.NewMessageType(md)) {
				return
			}
		}
	}
}

// FindFileByPathWithOrigin returns the file descriptor and an
// origin label users can recognize as "shipped with the LSP" —
// distinct from real registry namespaces.
func (l *internalLookup) FindFileByPathWithOrigin(path string) (protoreflect.FileDescriptor, string, error) {
	if l == nil {
		return nil, "", protoregistry.NotFound
	}
	for _, file := range l.files {
		if file.Path() == path {
			return file, InternalOrigin, nil
		}
	}
	return nil, "", protoregistry.NotFound
}

// GetSource returns the original .proto source bytes for an
// internal path. Backed by the `proto` package's embed.FS so the
// editor's `registry://_internal_/<path>` content provider can
// display the source verbatim — same view the protolsp repo
// ships, no descriptor-reconstruction guesswork.
//
// NotFound (or any embed.FS error) wraps to a plain error rather
// than protoregistry.NotFound: the sourceContent handler maps
// it to an editor-visible InternalError so a missing file is
// debuggable rather than silently empty.
func (l *internalLookup) GetSource(_ context.Context, path string) ([]byte, error) {
	if l == nil {
		return nil, protoregistry.NotFound
	}
	// Confirm the path is one of the internal files first — the
	// embed.FS could in principle hold more than we want to expose,
	// and bouncing arbitrary path reads off the FS is a needless
	// disclosure surface.
	known := false
	for _, p := range internalProtoFilePaths {
		if p == path {
			known = true
			break
		}
	}
	if !known {
		return nil, protoregistry.NotFound
	}
	bytes, err := internalproto.Sources.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("internal lookup: reading %s: %w", path, err)
	}
	return bytes, nil
}

// IsStale always returns false — internal descriptors are baked
// into the binary; there's nothing to be out of date relative to.
func (l *internalLookup) IsStale() bool { return false }

// Compile-time check that internalLookup satisfies Lookup.
var _ Lookup = (*internalLookup)(nil)
