// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package schema exposes the message-descriptor lookup protolsp uses
// to answer every schema-aware question (hover, go-to-definition,
// completion, validation). Three sources can populate it, layered
// into a single Lookup interface so consumers don't have to know
// which one shipped the descriptor:
//
//   - **Registry** (resolver.go) — the user's protoregistry. Reaches
//     out over gRPC, caches on disk, refreshes asynchronously. Most
//     production workspaces resolve schemas this way.
//   - **proto_files** (protofiles.go) — local .proto sources compiled
//     in-process via protocompile. Workspaces declare these in
//     .protolsp.pxf when there's no registry yet or for files that
//     aren't published anywhere.
//   - **Internal** (internal_lookup.go) — the descriptors protolsp
//     itself ships (protolsp.v1.*, chameleon.v1.*). Always available
//     so editing .protolsp.pxf / chain.pxf works without any
//     external dependency.
//
// All three implement the Lookup interface so the server can swap
// or layer them transparently. The registry path is the most
// complex (TLS + auth + cache), the others are pure
// descriptor-set readers.
package schema
