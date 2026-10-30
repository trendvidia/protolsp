// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package pxfpb is an alias of [github.com/trendvidia/protos/pxf],
// which owns the generated code for pxf/annotations.proto. It
// registers nothing itself: protocompile v0.36.0 links that package,
// and two registrations of pxf/annotations.proto panic at init
// (trendvidia/protos#1). buf.gen.yaml leaves this directory out so
// `buf generate` does not bring the duplicate back.
//
// The .proto beside this file stays: chain.proto imports it, and
// proto.Sources embeds it for the startup schema recompile.
//
// Deprecated: import github.com/trendvidia/protos/pxf.
package pxfpb

import protos "github.com/trendvidia/protos/pxf"

var (
	E_Default                  = protos.E_Default
	E_Key                      = protos.E_Key
	E_Required                 = protos.E_Required
	File_pxf_annotations_proto = protos.File_pxf_annotations_proto
)
