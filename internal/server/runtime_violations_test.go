// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Runtime-violation overlay: the protolsp/setRuntimeViolations
// custom notification feeds RFC-001 (file, descriptor_path)-keyed
// runtime violations into the diagnostics publish path as
// Information diagnostics with source "protolsp/runtime".

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// setRuntimeViolationsNotif builds the custom notification with a
// full-replace violation set for the document.
func setRuntimeViolationsNotif(t *testing.T, u string, violations []RuntimeViolation) jsonrpc2.Request {
	t.Helper()
	notif, err := jsonrpc2.NewNotification(MethodSetRuntimeViolations, SetRuntimeViolationsParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: protocol.DocumentURI(u)},
		Violations:   violations,
	})
	require.NoError(t, err)
	return notif
}

// lastDiagnostics returns the diagnostics of the most recent
// publishDiagnostics notification the fake conn recorded.
func lastDiagnostics(t *testing.T, fc *fakeConn) []protocol.Diagnostic {
	t.Helper()
	notes := fc.calls()
	require.NotEmpty(t, notes, "expected at least one publishDiagnostics")
	return notes[len(notes)-1].params.Diagnostics
}

// Receiving the notification triggers an immediate republish: the
// resolvable violations render as Information diagnostics with the
// dedicated "protolsp/runtime" source and source-map-anchored
// ranges; a key that doesn't resolve is silently dropped.
func TestSetRuntimeViolations_PublishesInformationDiagnostics(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	const u = "file:///work/rv.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001OpaqueCallProto)))
	require.Len(t, fc.calls(), 1, "didOpen publishes once")

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		setRuntimeViolationsNotif(t, u, []RuntimeViolation{
			{DescriptorPath: "t.M[t.check#0]", Message: "check failed for 3 records"},
			{DescriptorPath: "t.M[t.check#0]/arg#0/call#0", Message: "is_email rejected \"bob@\""},
			{DescriptorPath: "t.M[t.check#7]", Message: "never resolves"},
		})))

	notes := fc.calls()
	require.Len(t, notes, 2, "the notification republished immediately")
	diags := notes[1].params.Diagnostics
	require.Len(t, diags, 2, "resolvable keys render; the unresolvable one drops")

	assert.Equal(t, rng(9, 0, 9, 1), diags[0].Range, "annotation-use key anchors on the `@`")
	assert.Equal(t, protocol.DiagnosticSeverityInformation, diags[0].Severity)
	assert.Equal(t, "protolsp/runtime", diags[0].Source)
	assert.Equal(t, "check failed for 3 records", diags[0].Message)

	assert.Equal(t, rng(9, 7, 9, 15), diags[1].Range, "call key anchors on the callee ident")
	assert.Equal(t, protocol.DiagnosticSeverityInformation, diags[1].Severity)
	assert.Equal(t, "protolsp/runtime", diags[1].Source)
	assert.Equal(t, "is_email rejected \"bob@\"", diags[1].Message)
}

// The overlay concatenates after the analyze pass's own findings —
// a document with a compile error keeps its Error diagnostic and,
// when the source map still resolves the key, gains the runtime
// Information diagnostic alongside it.
func TestSetRuntimeViolations_CoexistsWithCompileDiagnostics(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	const u = "file:///work/rv-coexist.proto"

	// Same RFC-001 shape as rfc001OpaqueCallProto plus a second
	// message referencing an undeclared type: protocompile reports
	// the error while the file still lowers a source map for M.
	const src = `syntax = "proto3";

package t;

annotation check(rule: expression);

function is_email(s: string);

@check(is_email(this))
message M {
  string value = 1;
}

message N {
  Missing x = 1;
}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, src)))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		setRuntimeViolationsNotif(t, u, []RuntimeViolation{
			{DescriptorPath: "t.M[t.check#0]", Message: "runtime says no"},
		})))

	diags := lastDiagnostics(t, fc)
	var sources []string
	for _, d := range diags {
		sources = append(sources, d.Source)
	}
	assert.Contains(t, sources, "protolsp/proto", "the compile error stays")
	assert.Contains(t, sources, "protolsp/runtime", "the overlay coexists with it")
	// The runtime diagnostic comes after the analyze snapshot.
	last := diags[len(diags)-1]
	assert.Equal(t, "protolsp/runtime", last.Source)
	assert.Equal(t, protocol.DiagnosticSeverityInformation, last.Severity)
	assert.Equal(t, "runtime says no", last.Message)
}

// Re-analysis republishes the overlay: an edit that keeps the mapped
// construct alive re-resolves the key against the fresh source map
// (the anchor may move) and keeps rendering the violation.
func TestSetRuntimeViolations_SurvivesReanalysisWhileConstructExists(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	const u = "file:///work/rv-survive.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001OpaqueCallProto)))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		setRuntimeViolationsNotif(t, u, []RuntimeViolation{
			{DescriptorPath: "t.M[t.check#0]", Message: "still failing"},
		})))

	// Insert a leading comment line: the construct survives, its
	// anchor shifts down one line.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, u, 2, "// runtime overlay fixture\n"+rfc001OpaqueCallProto)))

	diags := lastDiagnostics(t, fc)
	require.Len(t, diags, 1, "the violation survives re-analysis")
	assert.Equal(t, "protolsp/runtime", diags[0].Source)
	assert.Equal(t, protocol.DiagnosticSeverityInformation, diags[0].Severity)
	assert.Equal(t, "still failing", diags[0].Message)
	assert.Equal(t, rng(10, 0, 10, 1), diags[0].Range,
		"the range tracks the shifted anchor")
}

// An edit that removes the mapped construct drops the violation from
// subsequent publishes (the key no longer resolves); restoring the
// construct brings it back — the store keeps the runtime state, only
// the rendering follows the buffer.
func TestSetRuntimeViolations_DropsWhenConstructRemoved(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	const u = "file:///work/rv-drop.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001OpaqueCallProto)))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		setRuntimeViolationsNotif(t, u, []RuntimeViolation{
			{DescriptorPath: "t.M[t.check#0]", Message: "gone after edit"},
		})))
	require.Len(t, lastDiagnostics(t, fc), 1, "violation renders before the edit")

	// Remove the @check use — the descriptor_path key stops
	// resolving even though M itself remains.
	const withoutUse = `syntax = "proto3";

package t;

annotation check(rule: expression);

function is_email(s: string);

message M {
  string value = 1;
}
`
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, u, 2, withoutUse)))
	assert.Empty(t, lastDiagnostics(t, fc),
		"removing the construct drops the violation from the publish")

	// Undo: the construct returns, the key resolves again, and the
	// stored violation reappears without a new notification.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didChangeCall(t, u, 3, rfc001OpaqueCallProto)))
	diags := lastDiagnostics(t, fc)
	require.Len(t, diags, 1, "restoring the construct restores the overlay")
	assert.Equal(t, "gone after edit", diags[0].Message)
}

// A second notification replaces the document's whole set (full-
// replace semantics), and an empty set clears the overlay.
func TestSetRuntimeViolations_FullReplaceAndClear(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	const u = "file:///work/rv-replace.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001OpaqueCallProto)))

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		setRuntimeViolationsNotif(t, u, []RuntimeViolation{
			{DescriptorPath: "t.M[t.check#0]", Message: "first batch A"},
			{DescriptorPath: "t.M[t.check#0]/arg#0/call#0", Message: "first batch B"},
		})))
	require.Len(t, lastDiagnostics(t, fc), 2)

	// Replace: only the new set renders — nothing accumulates.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		setRuntimeViolationsNotif(t, u, []RuntimeViolation{
			{DescriptorPath: "t.M[t.check#0]", Message: "second batch"},
		})))
	diags := lastDiagnostics(t, fc)
	require.Len(t, diags, 1, "the second notification replaced the set")
	assert.Equal(t, "second batch", diags[0].Message)

	// Clear: an empty violations list removes the overlay.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		setRuntimeViolationsNotif(t, u, nil)))
	assert.Empty(t, lastDiagnostics(t, fc), "an empty set clears the overlay")
}

// The overlay joins on the v0.20.0 descriptor-path shapes:
// alias-chain ordinals in §6.4 base→derived order and map value-alias
// rules keyed on the map field itself. A producer still emitting the
// pre-v0.19 `…Entry.value[…]` shape misses and drops — stale keys
// must never mis-join onto another construct's range.
func TestSetRuntimeViolations_AliasChainAndMapAliasKeys(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	const u = "file:///work/rv-alias-map.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001AliasMapProto)))

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		setRuntimeViolationsNotif(t, u, []RuntimeViolation{
			{DescriptorPath: "t.M.x[t.check#0]", Message: "base rule failed"},
			{DescriptorPath: "t.M.x[t.check#2]", Message: "field-site rule failed"},
			{DescriptorPath: "t.M.vals[t.check#1]", Message: "map value rejected"},
			{DescriptorPath: "t.M.ValsEntry.value[t.check#0]", Message: "stale pre-v0.19 key"},
		})))

	diags := lastDiagnostics(t, fc)
	require.Len(t, diags, 3, "new-shape keys render; the stale Entry.value key drops")

	assert.Equal(t, rng(6, 16, 6, 17), diags[0].Range,
		"ordinal 0 anchors at the base-most alias's @check")
	assert.Equal(t, "base rule failed", diags[0].Message)
	assert.Equal(t, rng(10, 10, 10, 11), diags[1].Range,
		"the field-site rule follows the whole chain")
	assert.Equal(t, "field-site rule failed", diags[1].Message)
	assert.Equal(t, rng(7, 13, 7, 14), diags[2].Range,
		"map value-alias keys join on the map field's own path")
	assert.Equal(t, "map value rejected", diags[2].Message)
	for _, d := range diags {
		assert.Equal(t, "protolsp/runtime", d.Source)
		assert.Equal(t, protocol.DiagnosticSeverityInformation, d.Severity)
	}
}

// The EnrichedViolation pass-through fields (protowire#161/#125):
// a violation carrying sourceRef joins by its §8.3.1 rule
// identity — the positional descriptorPath is ignored — with the file
// half suffix-matched against the document's source map (producers
// carry module-relative paths, protolsp compiles under absolute
// ones); a sourceRef naming a different file drops. forKey renders a
// "(map key)" marker, the only way to tell a key violation from a
// value violation of the same entry-level rule.
func TestSetRuntimeViolations_SourceRefJoinAndForKey(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	const u = "file:///work/rv-sr.proto"
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001AliasMapProto)))

	require.NoError(t, srv.Handle(context.Background(), noopReply,
		setRuntimeViolationsNotif(t, u, []RuntimeViolation{
			{
				SourceRef: &RuntimeSourceRef{File: "work/rv-sr.proto", DescriptorPath: "t.M.x[t.check#0]"},
				Message:   "joined by source_ref",
			},
			{
				DescriptorPath: "t.M.x[t.check#7]", // bogus positional key
				SourceRef:      &RuntimeSourceRef{DescriptorPath: "t.M.x[t.check#1]"},
				Message:        "source_ref wins over positional",
			},
			{
				SourceRef: &RuntimeSourceRef{File: "other.proto", DescriptorPath: "t.M.x[t.check#0]"},
				Message:   "names another file",
			},
			{
				DescriptorPath: "t.M.KeysEntry.key[t.check#0]",
				ForKey:         true,
				Message:        "key too short",
			},
		})))

	diags := lastDiagnostics(t, fc)
	require.Len(t, diags, 3, "the wrong-file sourceRef drops; the rest render")

	assert.Equal(t, rng(6, 16, 6, 17), diags[0].Range,
		"the module-relative file half suffix-matches the absolute source-map file")
	assert.Equal(t, "joined by source_ref", diags[0].Message)

	assert.Equal(t, rng(7, 13, 7, 14), diags[1].Range,
		"the rule identity joins even when the positional key is stale")
	assert.Equal(t, "source_ref wins over positional", diags[1].Message)

	assert.Equal(t, rng(6, 16, 6, 17), diags[2].Range,
		"a key violation anchors at the same rule as value violations")
	assert.Equal(t, "key too short (map key)", diags[2].Message,
		"forKey is surfaced in the message")
}

// Violations for a document that isn't open store without publishing
// (nothing to publish against) and render on a later didOpen;
// malformed params are logged and dropped, never an error.
func TestSetRuntimeViolations_UnopenedDocAndBadParams(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	const u = "file:///work/rv-late-open.proto"

	// Not open yet: stored, no publish.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		setRuntimeViolationsNotif(t, u, []RuntimeViolation{
			{DescriptorPath: "t.M[t.check#0]", Message: "arrived before open"},
		})))
	assert.Empty(t, fc.calls(), "no document, no publish")

	// Malformed params: dropped without error.
	bad, err := jsonrpc2.NewNotification(MethodSetRuntimeViolations, []string{"nope"})
	require.NoError(t, err)
	require.NoError(t, srv.Handle(context.Background(), noopReply, bad))

	// Opening the document publishes the stored overlay.
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, u, 1, rfc001OpaqueCallProto)))
	diags := lastDiagnostics(t, fc)
	require.Len(t, diags, 1, "the pre-open violation renders on open")
	assert.Equal(t, "arrived before open", diags[0].Message)
	assert.Equal(t, "protolsp/runtime", diags[0].Source)
}
