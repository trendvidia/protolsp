// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package proto exposes the on-disk .proto sources of protolsp's
// own descriptors as an embed.FS. The internal schema.Lookup uses
// them to serve go-to-definition results for protolsp's compiled-
// in types — `chameleon.v1.ChainSpec` and `protolsp.v1.WorkspaceConfig`
// — when the editor asks for the source bytes via
// SchemaService.GetSourceContent (or its LSP twin
// `protolsp/sourceContent`).
//
// Without an embed, an editor that go-to-defs into one of these
// types gets a `registry://` URI it can't load — the user lookup
// doesn't know the file and there's no on-disk path. We embed only
// the two files we care about; pxf/annotations.proto isn't an
// @type target, so its source isn't needed at runtime.
//
// Adding a new internal descriptor is a two-line change here plus
// the corresponding entry in internal/schema/internal_lookup.go's
// internalProtoFilePaths slice.
package proto

import "embed"

// Sources holds the .proto source bytes for protolsp's compiled-in
// descriptors. Keyed by the same path the descriptor reports via
// FileDescriptor.Path() — keep this aligned with
// internalProtoFilePaths in internal/schema/internal_lookup.go.
//
// pxf/annotations.proto is bundled too even though it isn't an
// @type target: the chameleon and protolsp schemas import it for
// (pxf.required) / (pxf.default) field options, so the schema
// recompile at startup needs the source to satisfy the import.
//
//go:embed protolsp/v1/workspace_config.proto chameleon/v1/chain.proto pxf/annotations.proto
var Sources embed.FS
