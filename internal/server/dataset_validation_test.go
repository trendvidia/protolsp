// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

// findDatasetSource collects every diagnostic whose Source is
// "protolsp/dataset" — small helper for R4 assertions.
func findDatasetSource(diags []protocol.Diagnostic) []protocol.Diagnostic {
	var out []protocol.Diagnostic
	for _, d := range diags {
		if optValue(d.Source) == "protolsp/dataset" {
			out = append(out, d)
		}
	}
	return out
}

func TestDatasetValidate_TypeMismatchInRowCell(t *testing.T) {
	// `qty` is int32; row 2 puts a string there.
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@dataset acme.demo.v1.Trade(id, qty)
("T-1", 10)
("T-2", "ten")
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findDatasetSource(notes[0].params.Diagnostics)
	require.Len(t, diags, 1, "exactly one dataset diagnostic (the bad qty cell)")
	d := diags[0]
	assert.Equal(t, protocol.DiagnosticSeverityError, d.Severity)
	assert.Contains(t, messageText(d.Message), "qty")
	assert.Contains(t, messageText(d.Message), "int32")
	assert.Contains(t, messageText(d.Message), "string",
		"message names both expected and got kinds")
}

func TestDatasetValidate_AllValidRowsEmitNoDiagnostics(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@dataset acme.demo.v1.Trade(id, qty)
("T-1", 10)
("T-2", 20)
("T-3", 30)
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Empty(t, findDatasetSource(notes[0].params.Diagnostics),
		"well-formed rows emit no dataset diagnostics")
}

func TestDatasetValidate_UnknownColumn(t *testing.T) {
	// `bogus` isn't a field on Trade.
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@dataset acme.demo.v1.Trade(id, bogus)
("T-1", "x")
("T-2", "y")
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findDatasetSource(notes[0].params.Diagnostics)
	// One diagnostic at the directive line — not one per row.
	require.Len(t, diags, 1,
		"unknown column is reported once on the directive, not per row")
	assert.Contains(t, messageText(diags[0].Message), "bogus")
	assert.Contains(t, messageText(diags[0].Message), "acme.demo.v1.Trade")
}

func TestDatasetValidate_UnknownTypeEmitsDirectiveDiagnostic(t *testing.T) {
	// `acme.totally.Unknown` not registered in the lookup.
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	// don't register Unknown
	srv.Lookups().Swap(lookup, nil)

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetUnknownSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findDatasetSource(notes[0].params.Diagnostics)
	require.Len(t, diags, 1,
		"unknown row type → one directive-line error, no per-row diagnostics")
	assert.Equal(t, protocol.DiagnosticSeverityError, diags[0].Severity)
	assert.Contains(t, messageText(diags[0].Message), "unknown row type")
	assert.Contains(t, messageText(diags[0].Message), "acme.totally.Unknown")
	assert.Contains(t, messageText(diags[0].Message), "row validation disabled")
}

func TestDatasetValidate_HugeRegionInfoDiagnostic(t *testing.T) {
	// Force the region to be huge via a very low threshold; the
	// validator should skip per-row checks and emit a single
	// information-severity note pointing at the CLI.
	srv, fc := newServerWithFakeConn(t)
	srv.SetHugeRegionBytes(50) // tiny threshold to force huge
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@dataset acme.demo.v1.Trade(id, qty)
("T-1", "definitely wrong, would be flagged if not huge")
("T-2", "also wrong")
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findDatasetSource(notes[0].params.Diagnostics)
	require.Len(t, diags, 1,
		"huge resolvable region emits one info-severity skip note, not per-row")
	assert.Equal(t, protocol.DiagnosticSeverityInformation, diags[0].Severity)
	assert.Contains(t, messageText(diags[0].Message), "huge")
	assert.Contains(t, messageText(diags[0].Message), "pxf validate",
		"info message points users at the CLI for whole-region checks")
}

func TestDatasetValidate_NoLookupSkipsValidation(t *testing.T) {
	// Without a schema, we can't validate rows. No diagnostics.
	srv, fc := newServerWithFakeConn(t)
	// Don't set srv.lookup.

	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, tradeDatasetSrc)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	assert.Empty(t, findDatasetSource(notes[0].params.Diagnostics),
		"no lookup → no dataset diagnostics")
}

func TestDatasetValidate_MultipleRowsMultipleErrors(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@dataset acme.demo.v1.Trade(id, qty)
("T-1", "first-bad")
("T-2", 20)
("T-3", "third-bad")
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findDatasetSource(notes[0].params.Diagnostics)
	assert.Len(t, diags, 2, "two bad cells produce two distinct diagnostics")
}

func TestDatasetValidate_NullOnAScalarIsAnError(t *testing.T) {
	// null may sit only on a field that can hold it, a message field
	// (draft § Booleans, Null, and Identifier Values; protowire#339).
	// qty is a plain int32, so the decoder rejects the cell, and so does
	// this check. An absent cell is fine anywhere.
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildTradeMessageType(t))
	srv.Lookups().Swap(lookup, nil)

	const src = `@dataset acme.demo.v1.Trade(id, qty)
("T-1", null)
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, "file:///work/t.pxf", 1, src)))

	notes := fc.calls()
	require.Len(t, notes, 1)
	diags := findDatasetSource(notes[0].params.Diagnostics)
	require.Len(t, diags, 1)
	assert.Equal(t, `column "qty" expects int32; got null`, messageText(diags[0].Message))
}

// Lower-level checks of the cell check happen indirectly through the
// dispatcher tests above (which exercise the type-mismatch paths
// against a real fake message type); analysis's
// TestCellCheck_AgreesWithTheDecoder holds each verdict against the
// decoder itself.
