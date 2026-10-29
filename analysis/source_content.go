// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/schema"
)

// MethodSourceContent is the custom LSP request the editor calls to
// fetch the bytes behind a `registry://` URI returned by
// go-to-definition. Namespaced under `protolsp/` to avoid collision
// with future LSP standard methods.
//
// Request:  { "uri": "registry://<namespace>/<file-path>" }
// Response: { "content": "<source bytes as utf-8>" }
//
// On a malformed URI or unknown path the server replies with a
// JSON-RPC error rather than empty content — the editor's content
// provider surfaces the message in the placeholder document so
// failures are debuggable.
const MethodSourceContent = "protolsp/sourceContent"

// registryURIScheme is the scheme used for virtual documents backed
// by a registry-fetched .proto source. The VS Code extension
// registers a TextDocumentContentProvider for this scheme that
// invokes [MethodSourceContent] via the language client.
const registryURIScheme = "registry://"

// SourceContentParams is the request shape for [MethodSourceContent].
type SourceContentParams struct {
	URI uri.URI `json:"uri"`
	// WorkspaceDir optionally scopes the fetch to a sub-workspace's
	// registry: the absolute directory of the sub-workspace
	// the preview was opened from. Empty selects the root lookup —
	// the pre-#215 behavior — so callers with no multi-root context
	// can omit it.
	WorkspaceDir string `json:"workspaceDir,omitempty"`
}

// SourceContentResult is the response shape for [MethodSourceContent].
// Content is sent as a string (UTF-8); .proto files are text by
// convention and the LSP framing is JSON, so binary-safe encoding
// isn't required.
type SourceContentResult struct {
	Content string `json:"content"`
}

// BuildRegistryURI constructs the URI go-to-definition returns for a
// registry-only file: `registry://<namespace>/<file-path>`.
//
// filePath is the FileDescriptor.Path() value — always relative
// within the schema (e.g. "acme/billing/v1/invoice.proto") and using
// forward slashes regardless of OS. The namespace component encodes
// which registry namespace contributed the file (per
// FindFileByPathWithOrigin). The LSP doesn't strictly need the
// namespace at fetch time (GetSource walks the chain), but it
// makes the URI self-describing in editor UI and logs.
func BuildRegistryURI(namespace, filePath string) uri.URI {
	return uri.URI(registryURIScheme + namespace + "/" + filePath)
}

// ParseRegistryURI extracts (namespace, filePath) from a
// `registry://<namespace>/<path>` URI. Returns ok=false for any URI
// not matching the expected shape — including URIs of other schemes
// the dispatcher might receive.
func ParseRegistryURI(u uri.URI) (namespace, filePath string, ok bool) {
	s := string(u)
	if !strings.HasPrefix(s, registryURIScheme) {
		return "", "", false
	}
	rest := s[len(registryURIScheme):]
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 || slash == len(rest)-1 {
		// Missing namespace, missing path, or trailing slash only.
		return "", "", false
	}
	return rest[:slash], rest[slash+1:], true
}

// LookupForNamespace picks the right schema.Lookup to serve a
// root-scoped registry:// source-content request. The `_internal_`
// sentinel origin routes to the compiled-in internalLookup;
// everything else falls through to the root user-bound lookup.
//
// This is the workspaceDir=="" case of
// [Workspace.lookupForNamespaceInWorkspace]; registry:// resolution
// originating from a sub-workspace goes through that doc/dir-scoped
// variant so a preview resolves against its own registry.
func (s *Workspace) LookupForNamespace(namespace string) schema.Lookup {
	return s.lookupForNamespaceInWorkspace(namespace, "")
}

// lookupForNamespaceInWorkspace picks the schema.Lookup to serve a
// registry:// request originating from workspaceDir. It mirrors
// [Workspace.LookupForDoc]'s routing so a preview opened from a
// sub-workspace resolves its content and cross-file imports against
// THAT sub-workspace's registry, not the root's:
//
//   - `_internal_` → the compiled-in internalLookup.
//   - a sub-workspace that declares its own schema source → that
//     sub's lookup (registry-backed if present, else proto_files).
//     No root fallback: the W4 isolation guarantee that LookupForDoc
//     enforces holds here too — a sub that declared a source never
//     borrows the root's.
//   - otherwise (root, an unregistered dir, or a sub with no source
//     of its own) → the root lookupMgr.Snapshot().
func (s *Workspace) lookupForNamespaceInWorkspace(namespace, workspaceDir string) schema.Lookup {
	if namespace == schema.InternalOrigin {
		return s.internalLookup
	}
	if sw := s.subWorkspaceForDir(workspaceDir); sw != nil {
		if lk := s.snapshotSubWorkspaceLookup(sw); lk != nil {
			return lk
		}
		if lk := s.snapshotSubWorkspaceProtoFiles(sw); lk != nil {
			return lk
		}
	}
	return s.lookupMgr.Snapshot()
}

// SourceContent error classes, exposed as errors.Is targets so
// transport shims (and embedders rendering placeholder documents)
// can distinguish caller mistakes from missing configuration
// without string matching.
var (
	// ErrNotRegistryURI: the URI isn't of the
	// registry://<namespace>/<file-path> shape.
	ErrNotRegistryURI = errors.New("not a registry:// URI")
	// ErrNoSchemaLookup: no schema lookup is configured for the
	// requested namespace, so there is nowhere to fetch from.
	ErrNoSchemaLookup = errors.New("no schema lookup configured")
)

// SourceContent fetches the source bytes behind a registry:// URI
// against the ROOT lookup: parse the namespace, route to the right
// lookup (compiled-in internal vs root user-bound, per
// [Workspace.LookupForNamespace]), and GetSource the file. The
// composed recipe behind [MethodSourceContent] and the RPC sidecar's
// GetSourceContent; exported so in-process embedders share it.
// Multi-root callers use [Workspace.SourceContentForWorkspace] to
// resolve against a sub-workspace's own registry.
//
// Errors wrap [ErrNotRegistryURI] (malformed URI — caller mistake),
// [ErrNoSchemaLookup] (nothing configured), or the underlying
// GetSource failure verbatim.
func (s *Workspace) SourceContent(ctx context.Context, u uri.URI) (SourceContentResult, error) {
	return s.SourceContentForWorkspace(ctx, u, "")
}

// SourceContentForWorkspace is [Workspace.SourceContent] scoped to a
// sub-workspace: workspaceDir is the absolute directory of the
// sub-workspace the preview was opened from, so a registry:// source
// fetched from a sub-workspace's own registry resolves against THAT
// registry rather than the root's. An empty workspaceDir
// selects the root lookup, making SourceContent the back-compatible
// root-scoped entry point.
func (s *Workspace) SourceContentForWorkspace(ctx context.Context, u uri.URI, workspaceDir string) (SourceContentResult, error) {
	namespace, filePath, ok := ParseRegistryURI(u)
	if !ok {
		return SourceContentResult{}, fmt.Errorf(
			"%w: expected registry://<namespace>/<file-path>, got %q", ErrNotRegistryURI, string(u))
	}
	lk := s.lookupForNamespaceInWorkspace(namespace, workspaceDir)
	if lk == nil {
		return SourceContentResult{}, fmt.Errorf(
			"%w; cannot fetch registry sources", ErrNoSchemaLookup)
	}
	content, err := lk.GetSource(ctx, filePath)
	if err != nil {
		return SourceContentResult{}, fmt.Errorf("fetching %s: %w", filePath, err)
	}
	return SourceContentResult{Content: string(content)}, nil
}
