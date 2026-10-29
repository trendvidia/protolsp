// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package workspaceconfig owns the .protolsp.pxf file as both a
// Tier 2 configuration source and an editable settings store.
//
// Two consumption shapes, picked by caller intent:
//
//   - Read-for-effect (config.go, parser.go): Parse(content) →
//     *Config. Walks the PXF AST, resolves ${env.X} interpolation,
//     returns the materialized Registry / ProtoFiles / Security /
//     Chameleon blocks the server consumes when assembling the
//     effective workspace config.
//   - Read/write-as-settings (attrs.go, edit.go, io.go):
//     LoadDocument(path) / GetAttribute / SetAttribute /
//     WriteDocument. Operates on the raw PXF AST so the
//     configList / configGet / configSet RPCs can round-trip
//     individual attributes without disturbing comments or
//     unrelated blocks. AttributeRegistry is the canonical list
//     of settable fields; adding a top-level field to the proto
//     schema requires a matching entry here.
//
// The two shapes share the same on-disk file but deliberately
// don't share an in-memory representation. Parse drops position
// info and resolves interpolations; the AST keeps both, which is
// what Set needs to preserve formatting.
//
// Schema validation against a compiled-in
// protolsp.v1.WorkspaceConfig descriptor is intentionally not
// done — the package walks the AST directly so protolsp doesn't
// need a proto build step. The trade-off: malformed configs
// surface as "missing field" silently rather than as schema-aware
// diagnostics on the .protolsp.pxf file itself. AttributeRegistry's
// hand-maintained Kind metadata fills the gap at the Set boundary
// by rejecting type-mismatched values up front.
package workspaceconfig
