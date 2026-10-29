// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// W2 of the workspace-config design: a schema.Lookup backed by
// .proto files compiled directly from disk via protocompile. Lets a
// workspace declare `schema_sources { proto_files { paths = [...] } }`
// in .protolsp.pxf and get schema-aware editing without standing up a
// protoregistry.
//
// Compile failures surface as CompileError records; the server side
// turns them into diagnostics anchored on .protolsp.pxf so the user
// sees actionable errors in their editor.

package schema

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/trendvidia/protocompile"
	"github.com/trendvidia/protocompile/reporter"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// ProtoFilesOrigin is the namespace identifier reported by
// FindFileByPathWithOrigin for files served by a proto_files
// source. Distinct from "" (ad-hoc parent) and from real registry
// namespace IDs so editors can render "from local proto" provenance.
//
// The value rides inside the authority segment of
// `registry://<origin>/<path>` URIs that go-to-def returns to
// editors. Authority must be URI-safe per RFC 3986 (alphanumerics,
// hyphens, underscores, dots) — Monaco's URI parser silently
// rejects non-conforming authorities, which manifested as Cmd-click
// silently no-op'ing on go-to-def Locations from proto_files-backed
// schemas. Underscores around the name (matching the
// `_internal_` sentinel for internally-shipped types) signal
// "non-registry origin" while staying URI-safe.
const ProtoFilesOrigin = "_proto_files_"

// ProtoFilesConfig drives OpenProtoFiles. Sources mirrors the
// declaration-order list of `proto_files { ... }` blocks parsed
// out of .protolsp.pxf; each source contributes its `paths` as
// protocompile import roots AND as roots to walk for *.proto.
type ProtoFilesConfig struct {
	Sources []ProtoFilesSource
}

// ProtoFilesSource is one `proto_files { ... }` block. Paths are
// absolute directory paths (caller resolves relative-to-.protolsp.pxf
// before passing in).
type ProtoFilesSource struct {
	Paths []string
}

// IsEmpty reports whether the config contributes no paths — fast
// no-op for OpenProtoFiles.
func (c ProtoFilesConfig) IsEmpty() bool {
	for _, s := range c.Sources {
		if len(s.Paths) > 0 {
			return false
		}
	}
	return true
}

// CompileError describes one error raised by protocompile while
// loading a proto_files source. File is the path protocompile saw
// (relative to whichever import root contained it); Line / Column
// are 1-indexed source positions. Message is the underlying error
// without protocompile's added position prefix.
type CompileError struct {
	File    string
	Line    int
	Column  int
	Message string
}

// String renders the error in a single line for log output and
// diagnostic Message bodies: "<file>:<line>:<col>: <message>".
// Position parts collapse when zero.
func (e CompileError) String() string {
	if e.Line > 0 && e.Column > 0 {
		return fmt.Sprintf("%s:%d:%d: %s", e.File, e.Line, e.Column, e.Message)
	}
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.File, e.Message)
}

// OpenProtoFiles walks every directory in cfg.Sources for *.proto
// files, compiles them via protocompile, and exposes the linked
// descriptors as a schema.Lookup. Returns (nil, nil, nil, nil)
// when cfg is empty or no .proto files were found — both cases are
// "no proto_files source contributed anything," handled identically
// at the call site (workspace falls through to the next source).
//
// Compile errors are returned in the third slot; a non-empty slice
// doesn't necessarily mean (nil, nil) — protocompile may still
// produce a partial set the caller can use. The fourth slot
// (error) is reserved for setup failures (path walking, fs errors)
// that prevent compilation from running at all.
//
// The returned io.Closer is a no-op stub; included for parity with
// schema.Open so callers can store the result uniformly. The
// underlying state is in-process memory only — no goroutines,
// no network sockets, nothing to release.
func OpenProtoFiles(ctx context.Context, cfg ProtoFilesConfig, logger *slog.Logger) (Lookup, io.Closer, []CompileError, error) {
	if logger == nil {
		logger = slog.Default()
	}
	var (
		importPaths  []string
		relToAbs     = map[string]string{}
		compileFiles []string
	)
	for _, src := range cfg.Sources {
		for _, root := range src.Paths {
			absRoot, err := filepath.Abs(root)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("resolving proto_files path %q: %w", root, err)
			}
			if info, statErr := os.Stat(absRoot); statErr != nil || !info.IsDir() {
				// Missing / not-a-directory is non-fatal: log + skip.
				// A typo in `paths` shouldn't disable schema-awareness
				// for the entries that DO exist.
				logger.Warn("proto_files: skipping path that isn't a directory",
					slog.String("path", absRoot))
				continue
			}
			importPaths = append(importPaths, absRoot)
			walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				if !strings.HasSuffix(strings.ToLower(d.Name()), ".proto") {
					return nil
				}
				rel, relErr := filepath.Rel(absRoot, path)
				if relErr != nil {
					return relErr
				}
				rel = filepath.ToSlash(rel)
				// First import-root wins on path collisions: the
				// protocompile resolver searches roots in order and
				// returns the first hit. Mirror that in our map so
				// GetSource reads the same file the compiler did.
				if _, ok := relToAbs[rel]; !ok {
					relToAbs[rel] = path
					compileFiles = append(compileFiles, rel)
				}
				return nil
			})
			if walkErr != nil {
				return nil, nil, nil, fmt.Errorf("walking proto_files path %q: %w", absRoot, walkErr)
			}
		}
	}
	if len(compileFiles) == 0 {
		return nil, nil, nil, nil
	}
	sort.Strings(compileFiles)

	var (
		compileErrs []CompileError
		errMu       sync.Mutex
	)
	rep := reporter.NewReporter(
		func(e reporter.ErrorWithPos) error {
			pos := e.GetPosition()
			msg := "compile error"
			if u := e.Unwrap(); u != nil {
				msg = u.Error()
			}
			errMu.Lock()
			compileErrs = append(compileErrs, CompileError{
				File:    pos.Filename,
				Line:    pos.Line,
				Column:  pos.Col,
				Message: msg,
			})
			errMu.Unlock()
			// Returning nil tells protocompile to continue past this
			// error — a typo in one .proto shouldn't drop the whole
			// descriptor set. (protocompile ≥ v0.16.0 honors this on
			// the experimental pipeline and returns partial results
			// with nil placeholders alongside ErrInvalidSource.)
			return nil
		},
		nil, // warnings dropped for now; protolsp doesn't surface them
	)

	compiler := &protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			ImportPaths: importPaths,
		}),
		Reporter:       rep,
		SourceInfoMode: protocompile.SourceInfoStandard,
	}
	linked, err := compiler.Compile(ctx, compileFiles...)
	if err != nil && len(compileErrs) == 0 {
		// Compile bailed without a position-tagged error — surface
		// it as a file-level CompileError so the caller still gets
		// something to render on .protolsp.pxf.
		compileErrs = append(compileErrs, CompileError{
			Message: err.Error(),
		})
	}

	files := protoregistry.NewNamespacedFiles(nil)
	types := protoregistry.NewNamespacedTypes(nil)
	for _, lf := range linked {
		if lf == nil {
			continue
		}
		// Skip the well-known files protocompile.WithStandardImports
		// injects — they're already in GlobalFiles via the upstream
		// google.golang.org/protobuf runtime and registering them
		// again here would collide.
		if isStandardImport(lf.Path()) {
			continue
		}
		if regErr := files.RegisterFile(lf); regErr != nil {
			// Duplicate registration shows up when two proto_files
			// blocks scan overlapping paths. Not fatal — first wins.
			logger.Debug("proto_files: skipping duplicate file registration",
				slog.String("path", lf.Path()),
				slog.String("err", regErr.Error()))
			continue
		}
		registerFileMessages(types, lf, logger)
	}

	return &protoFilesLookup{
		files:      files,
		types:      types,
		fileSource: relToAbs,
	}, noopCloser{}, compileErrs, nil
}

// registerFileMessages walks every message (top-level and nested)
// in fd and registers a dynamic MessageType for it into types.
// Duplicate registration logs at Debug and continues; W3's
// WithFallback path expects every name reachable once.
func registerFileMessages(types *protoregistry.NamespacedTypes, fd protoreflect.FileDescriptor, logger *slog.Logger) {
	visit(fd.Messages(), func(md protoreflect.MessageDescriptor) {
		if err := types.RegisterMessage(dynamicpb.NewMessageType(md)); err != nil {
			logger.Debug("proto_files: skipping duplicate message registration",
				slog.String("name", string(md.FullName())),
				slog.String("err", err.Error()))
		}
	})
}

// visit walks msgs recursively and invokes fn for every message
// descriptor it encounters. Used by registerFileMessages and the
// public RangeMessages traversal.
func visit(msgs protoreflect.MessageDescriptors, fn func(protoreflect.MessageDescriptor)) {
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		fn(md)
		visit(md.Messages(), fn)
	}
}

// isStandardImport flags the google.protobuf well-known type paths
// that protocompile.WithStandardImports compiles into every result.
// They self-register via the upstream protobuf runtime's init();
// re-registering into our local *protoregistry.Files would error.
func isStandardImport(path string) bool {
	return strings.HasPrefix(path, "google/protobuf/")
}

// protoFilesLookup adapts a NamespacedFiles + NamespacedTypes pair
// (populated from disk-compiled descriptors) into the schema.Lookup
// interface. The Namespaced* shape — rather than the plain Files
// type — is what protoregistry/client.WithFallback wants, so W3
// can compose proto_files behind a user registry without an
// adapter layer.
type protoFilesLookup struct {
	files      *protoregistry.NamespacedFiles
	types      *protoregistry.NamespacedTypes
	fileSource map[string]string // proto path → absolute disk path
}

// ProtoFilesParents reports the NamespacedFiles + NamespacedTypes
// behind a proto_files lookup so W3 can plug them in as
// WithFallback parents on a sibling protoregistry/client.Resolver.
// Returns (nil, nil) for anything that isn't a proto_files lookup.
func ProtoFilesParents(l Lookup) (*protoregistry.NamespacedFiles, *protoregistry.NamespacedTypes) {
	pf, ok := l.(*protoFilesLookup)
	if !ok || pf == nil {
		return nil, nil
	}
	return pf.files, pf.types
}

// ProtoFilesLocalPath reports the absolute on-disk path the
// proto_files compile read protoPath from. Returns ("", false) for
// anything that isn't a proto_files lookup or for paths the compile
// didn't see (registry-attributed / well-known imports).
//
// Descriptor paths coming out of the compile are search-path-
// relative ("myapp/v1/greeting.proto"), not workspace-relative —
// this map is the only place the original disk location survives,
// which is what lets go-to-definition hand back a file:// URI to
// the user's working copy instead of a read-only registry://
// preview (#185). The map is populated once at OpenProtoFiles time
// and never mutated, so concurrent reads are safe.
func ProtoFilesLocalPath(l Lookup, protoPath string) (string, bool) {
	pf, ok := l.(*protoFilesLookup)
	if !ok || pf == nil {
		return "", false
	}
	abs, ok := pf.fileSource[protoPath]
	return abs, ok
}

func (l *protoFilesLookup) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	if l == nil {
		return nil, protoregistry.NotFound
	}
	return l.types.FindMessageByName(name)
}

func (l *protoFilesLookup) RangeMessages(fn func(protoreflect.MessageType) bool) {
	if l == nil {
		return
	}
	l.types.RangeMessages(fn)
}

func (l *protoFilesLookup) FindFileByPathWithOrigin(path string) (protoreflect.FileDescriptor, string, error) {
	if l == nil {
		return nil, "", protoregistry.NotFound
	}
	fd, err := l.files.FindFileByPath(path)
	if err != nil {
		return nil, "", err
	}
	return fd, ProtoFilesOrigin, nil
}

func (l *protoFilesLookup) GetSource(_ context.Context, filePath string) ([]byte, error) {
	if l == nil {
		return nil, protoregistry.NotFound
	}
	abs, ok := l.fileSource[filePath]
	if !ok {
		return nil, protoregistry.NotFound
	}
	data, err := os.ReadFile(abs) //#nosec G304 -- path comes from compile-time-resolved import paths
	if err != nil {
		return nil, fmt.Errorf("reading proto source %s: %w", abs, err)
	}
	return data, nil
}

// IsStale always returns false: proto_files lookups don't have a
// network backing they could be "out of date" with.
func (l *protoFilesLookup) IsStale() bool { return false }

// noopCloser keeps the OpenProtoFiles signature parallel to
// schema.Open — every Lookup-producing constructor returns a
// closer the caller stores. proto_files has nothing to release.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// Compile-time check.
var _ Lookup = (*protoFilesLookup)(nil)
