// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// @sensitive(class) support (protowire#163): workspace-
// harvested class-name completion inside the class argument, and the
// reserved `protowire.` prefix rejection surfacing through the
// compile pass (enforced compiler-side since protocompile v0.21.0 —
// protolsp adds no custom diagnostic).

package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// setupSensitiveWorkspace stages the protowire standard-library
// annotations file plus one schema already using two sensitivity
// classes — the vocabulary the completion harvests.
func setupSensitiveWorkspace(t *testing.T) (dir string, srv *Server, fc *fakeConn) {
	t.Helper()
	root := t.TempDir()

	mustWrite(t, filepath.Join(root, "buf.yaml"), `version: v2
modules:
  - path: proto
`)
	mustWrite(t, filepath.Join(root, "proto", "protowire", "schema", "v1", "annotations.proto"), `syntax = "proto3";

package protowire.schema.v1;

annotation sensitive(class: string = "");
`)
	mustWrite(t, filepath.Join(root, "proto", "acme", "existing.proto"), `syntax = "proto3";

package acme.app.v1;

import "protowire/schema/v1/annotations.proto";

message Customer {
  string ssn = 1 @protowire.schema.v1.sensitive("pii");
  string token = 2 @protowire.schema.v1.sensitive(class = "secret");
}
`)

	srv, fc = newServerWithFakeConn(t)
	srv.Workspaces().SetWorkspaceRoot(uri.URI("file://" + filepath.ToSlash(root)))
	srv.LoadProtoIndex()
	return "file://" + filepath.ToSlash(filepath.Join(root, "proto", "acme")), srv, fc
}

const sensitiveUserHeader = `syntax = "proto3";

package acme.app.v1;

import "protowire/schema/v1/annotations.proto";

message Payment {
`

// Inside the class string literal of a @sensitive use, completion
// offers the workspace's harvested class names bare (sorted, unique);
// outside the literal but inside the parens the items carry quotes.
// Function names never surface in either position.
func TestProtoCompletion_SensitiveClassNames(t *testing.T) {
	dir, srv, _ := setupSensitiveWorkspace(t)
	u := dir + "/payment.proto"

	// Line 7: cursor inside the empty string literal, char 50
	// (the `(` sits at char 48).
	src := sensitiveUserHeader + `  string card = 1 @protowire.schema.v1.sensitive("");` + "\n}\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, u, 1, src)))

	list := completionFromHandle(t, srv, u, 7, 50)
	require.NotNil(t, list, "class completion fires inside the literal")
	var labels []string
	for _, it := range list.Items {
		labels = append(labels, it.Label)
		assert.Equal(t, "sensitivity class (workspace)", detailText(it.Detail))
	}
	assert.Equal(t, []string{"pii", "secret"}, labels,
		"harvested classes, bare inside the string, sorted")

	// Cursor between the empty parens (char 49): quoted items,
	// ready to insert as the whole argument.
	src2 := sensitiveUserHeader + `  string card = 1 @protowire.schema.v1.sensitive();` + "\n}\n"
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, u, 2, src2)))
	list = completionFromHandle(t, srv, u, 7, 49)
	require.NotNil(t, list)
	labels = nil
	for _, it := range list.Items {
		labels = append(labels, it.Label)
	}
	assert.Equal(t, []string{`"pii"`, `"secret"`}, labels,
		"outside the literal the items carry their quotes")
}

// An org's own annotation that happens to be named `sensitive`
// resolves to a different FQN and keeps the ordinary argument
// completion — no class items.
func TestProtoCompletion_LocalSensitiveAnnotationUnaffected(t *testing.T) {
	dir, srv, _ := setupSensitiveWorkspace(t)
	u := dir + "/local.proto"

	src := `syntax = "proto3";

package acme.app.v1;

annotation sensitive(class: string = "");

message M {
  string x = 1 @sensitive("");
}
`
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, u, 1, src)))

	// Line 7, inside the literal (char 27: `  string x = 1 @sensitive("` …).
	list := completionFromHandle(t, srv, u, 7, 27)
	if list != nil {
		for _, it := range list.Items {
			assert.NotEqual(t, "sensitivity class (workspace)", detailText(it.Detail),
				"acme.app.v1.sensitive is not the protowire marker")
		}
	}
}

// The reserved `protowire.` class prefix is rejected by the compile
// pass (protocompile v0.21.0) and surfaces as a protolsp/proto Error
// on the offending literal; a legitimate class publishes clean.
func TestDidOpen_SensitiveReservedClassPrefix(t *testing.T) {
	cases := []struct {
		name     string
		use      string
		wantDiag bool
	}{
		{name: "reserved prefix diagnoses", use: `@protowire.schema.v1.sensitive("protowire.pii")`, wantDiag: true},
		{name: "reserved bare word diagnoses", use: `@protowire.schema.v1.sensitive(class = "protowire")`, wantDiag: true},
		{name: "org class is clean", use: `@protowire.schema.v1.sensitive("pii")`, wantDiag: false},
		{name: "lookalike prefix is clean", use: `@protowire.schema.v1.sensitive("protowirex.pii")`, wantDiag: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir, srv, fc := setupSensitiveWorkspace(t)
			u := dir + "/reserved.proto"
			src := sensitiveUserHeader + "  string card = 1 " + c.use + ";\n}\n"
			require.NoError(t, testHandle(srv, context.Background(), noopReply,
				didOpenCall(t, u, 1, src)))

			var diags []protocol.Diagnostic
			for _, n := range fc.calls() {
				if string(n.params.URI) == u {
					diags = n.params.Diagnostics
				}
			}
			var reserved *protocol.Diagnostic
			for i := range diags {
				if strings.Contains(messageText(diags[i].Message), "reserved") {
					reserved = &diags[i]
					break
				}
			}
			if !c.wantDiag {
				assert.Nil(t, reserved, "no reserved-prefix finding expected; got %+v", diags)
				return
			}
			require.NotNil(t, reserved, "expected a reserved-prefix diagnostic; got %+v", diags)
			assert.Equal(t, "protolsp/proto", optValue(reserved.Source),
				"the compiler's finding flows through the ordinary compile channel")
			assert.Equal(t, protocol.DiagnosticSeverityError, reserved.Severity)
		})
	}
}
