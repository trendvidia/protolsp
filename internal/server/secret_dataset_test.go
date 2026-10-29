// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/trendvidia/protocompile"
	"go.lsp.dev/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/trendvidia/protolsp/redact"
)

// loginSecretSchema has a pxf.Secret in every place a value can hold
// one: a singular field, a field of a sub-message, a repeated field
// and a map value. Same stand-in Secret as
// buildCredentialsWithSecretType: only the FullName matters.
const loginSecretSchema = `
syntax = "proto3";
package acme.demo.v1;
import "pxf/secret.proto";

message Db {
  string host = 1;
  pxf.Secret pw = 2;
}

message Login {
  string user = 1;
  pxf.Secret pw = 2;
  Db db = 3;
  repeated pxf.Secret keys = 4;
  map<string, pxf.Secret> tenants = 5;
}
`

func buildLoginWithSecretsType(t *testing.T) protoreflect.MessageType {
	t.Helper()
	c := &protocompile.Compiler{Resolver: &protocompile.SourceResolver{
		Accessor: protocompile.SourceAccessorFromMap(map[string]string{
			"login.proto":      loginSecretSchema,
			"pxf/secret.proto": "syntax = \"proto3\";\npackage pxf;\nmessage Secret { string value = 1; string hint = 2; string fingerprint = 3; }\n",
		}),
	}}
	files, err := c.Compile(context.Background(), "login.proto")
	require.NoError(t, err)
	md := files[0].Messages().ByName("Login")
	require.NotNil(t, md)
	return dynamicpb.NewMessageType(md)
}

// openLoginDoc opens src against the Login schema and returns the
// analyzed document.
func openLoginDoc(t *testing.T, src string) (*Server, *Document) {
	t.Helper()
	srv, _ := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildLoginWithSecretsType(t))
	srv.Lookups().Swap(lookup, nil)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/login.pxf", 1, src)))
	doc := srv.Documents().Get("file:///work/login.pxf")
	require.NotNil(t, doc)
	require.NoError(t, doc.ParseErr)
	return srv, doc
}

type secretSeen struct {
	path, value string
}

func secretsOf(doc *Document) []secretSeen {
	values := doc.SecretValues()
	out := make([]secretSeen, len(doc.SecretRanges))
	for i, sr := range doc.SecretRanges {
		out[i] = secretSeen{sr.Path, values[i]}
	}
	return out
}

// Every way a @dataset row can hold a pxf.Secret records it: a
// string or block cell under the Secret's own column, a dotted path
// to its value, a column into a sub-message that holds one, and list
// and map cells. Hint stays visible.
func TestCollectSecretRanges_DatasetCells(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []secretSeen
	}{
		{"string cell", "@dataset acme.demo.v1.Login (user, pw)\n(\"a\", \"s1\")\n(\"b\", \"s2\")\n",
			[]secretSeen{{"row1.pw", "s1"}, {"row2.pw", "s2"}}},
		{"block cell", "@dataset acme.demo.v1.Login (pw)\n({ value = \"s3\" hint = \"h\" })\n",
			[]secretSeen{{"row1.pw", "s3"}}},
		{"dotted value", "@dataset acme.demo.v1.Login (pw.value, pw.hint)\n(\"s4\", \"h\")\n",
			[]secretSeen{{"row1.pw", "s4"}}},
		{"sub-message paths", "@dataset acme.demo.v1.Login (db.host, db.pw)\n(\"h\", \"s5\")\n",
			[]secretSeen{{"row1.db.pw", "s5"}}},
		{"sub-message value path", "@dataset acme.demo.v1.Login (db.pw.value)\n(\"s6\")\n",
			[]secretSeen{{"row1.db.pw", "s6"}}},
		{"sub-message block cell", "@dataset acme.demo.v1.Login (db)\n({ host = \"h\" pw = \"s7\" })\n",
			[]secretSeen{{"row1.db.pw", "s7"}}},
		{"list cell", "@dataset acme.demo.v1.Login (keys)\n([\"k1\", { value = \"k2\" }])\n",
			[]secretSeen{{"row1.keys.0", "k1"}, {"row1.keys.1", "k2"}}},
		{"map cell", "@dataset acme.demo.v1.Login (tenants)\n({ \"acme\": \"t1\" })\n",
			[]secretSeen{{"row1.tenants.acme", "t1"}}},
		{"empty cell", "@dataset acme.demo.v1.Login (user, pw)\n(\"a\", )\n",
			[]secretSeen{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, doc := openLoginDoc(t, tc.src)
			assert.Equal(t, tc.want, secretsOf(doc))
		})
	}
}

// The range of a cell's secret covers its string literal, quotes
// included, so an editor masking it hides the whole "...".
func TestCollectSecretRanges_DatasetCellRange(t *testing.T) {
	_, doc := openLoginDoc(t, "@dataset acme.demo.v1.Login (user, pw)\n(\"a\", \"s3cret\")\n")
	require.Len(t, doc.SecretRanges, 1)
	assert.Equal(t, protocol.Range{
		Start: protocol.Position{Line: 1, Character: 6},
		End:   protocol.Position{Line: 1, Character: 14},
	}, doc.SecretRanges[0].Range)
}

// The body walk takes the same value forms the cells do: a Secret
// written as `= { ... }`, a message written as `= { ... }`, a list and
// a map. Before #286 it recorded only `pw = "x"` and `pw { ... }`.
func TestCollectSecretRanges_BodyValueForms(t *testing.T) {
	_, doc := openLoginDoc(t, `@type acme.demo.v1.Login

pw = { value = "b1" hint = "h" }
db = { host = "h" pw = "b2" }
keys = ["b3", { value = "b4" }]
tenants = { "acme": "b5" }
`)
	assert.Equal(t, []secretSeen{
		{"pw", "b1"}, {"db.pw", "b2"}, {"keys.0", "b3"}, {"keys.1", "b4"}, {"tenants.acme", "b5"},
	}, secretsOf(doc))
}

// A cell's secret reaches the emit-boundary redaction: the
// literal-secret warning is published on the cell, and a diagnostic
// message quoting the cell's text is redacted with the document's
// secret values, which is what every publish path applies. No
// diagnostic quotes a cell's text today; this pins that one would not
// leak it.
func TestPublishDiagnostics_DatasetSecret(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	lookup := newFakeLookup()
	lookup.add(buildLoginWithSecretsType(t))
	srv.Lookups().Swap(lookup, nil)
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, "file:///work/login.pxf", 1, "@dataset acme.demo.v1.Login (user, pw.value)\n(\"a\", \"leaky-cell\")\n")))

	notes := fc.calls()
	require.NotEmpty(t, notes)
	var warned *protocol.Diagnostic
	for i, d := range notes[len(notes)-1].params.Diagnostics {
		assert.NotContains(t, d.Message, "leaky-cell")
		if d.Source == "protolsp/security" {
			warned = &notes[len(notes)-1].params.Diagnostics[i]
		}
	}
	require.NotNil(t, warned, "a literal-secret warning is published for the cell")
	assert.Equal(t, uint32(1), warned.Range.Start.Line)
	assert.Contains(t, warned.Message, `"row1.pw"`)

	doc := srv.Documents().Get("file:///work/login.pxf")
	d := protocol.Diagnostic{Message: `column "pw.value": bad value "leaky-cell"`}
	redact.Diagnostic(&d, doc.SecretValues())
	assert.Equal(t, `column "pw.value": bad value "<redacted>"`, d.Message)
}
