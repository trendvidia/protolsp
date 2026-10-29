// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"

	"github.com/trendvidia/protolsp/positions"
)

func TestProtoScopedCandidates(t *testing.T) {
	// The #237 scope-peeling rules: interpretations of `name` against
	// each enclosing scope of refPkg, with targetPkg's prefix peeled.
	cases := []struct {
		name, refPkg, targetPkg string
		want                    []string
	}{
		// Bare names resolve through the scope chain only: a
		// bare reference can't land in an unrelated package.
		{"Foo", "acme.v1", "other.v1", nil},
		{"Foo", "acme.v1", "acme.v1", []string{"Foo"}},
		{"Foo", "", "", []string{"Foo"}},

		// Package-qualified leaf (#229 behavior preserved).
		{"other.lib.v1.Bar", "acme.v1", "other.lib.v1", []string{"Bar"}},
		{"other.lib.v1.Bar", "acme.v1", "wrong.pkg.v1", nil},

		// Outer-qualified nested references.
		{"other.lib.v1.Outer.Inner", "acme.v1", "other.lib.v1", []string{"Outer.Inner"}},
		{"Outer.Inner", "acme.v1", "acme.v1", []string{"Outer.Inner"}},
		{"Outer.Inner", "acme.v1", "other.v1", nil},
		// The whole qualifier can also be a deeper package.
		{"other.lib.v1.Outer.Inner", "acme.v1", "other.lib.v1.Outer", []string{"Inner"}},

		// Reference-site message scopes participate: refScope
		// carries the enclosing messages, innermost interpretations
		// first.
		{"Middle.Inner", "pkg.Outer", "pkg", []string{"Outer.Middle.Inner", "Middle.Inner"}},
		{"Inner", "pkg.Outer.Middle", "pkg", []string{"Outer.Middle.Inner", "Outer.Inner", "Inner"}},
		// No scope interpretation lands in the target package → no
		// candidates, even for a bare name.
		{"Inner", "pkg.Outer", "other", nil},

		// Enclosing-package scopes of refPkg participate, innermost
		// first: `b.X` written in package a.b can mean .a.b.b.X or
		// .a.b.X.
		{"b.X", "a.b", "a.b", []string{"b.X", "X"}},
		{"lib.Outer.Inner", "acme", "acme.lib", []string{"Outer.Inner"}},

		// A reference that names the target package itself isn't a type.
		{"other.lib", "acme.v1", "other.lib", nil},

		// Empty target package: every interpretation is a scope path.
		{"pkg.Outer.Inner", "", "", []string{"pkg.Outer.Inner"}},

		{"", "acme.v1", "acme.v1", nil},
	}
	for _, tc := range cases {
		got := protoScopedCandidates(tc.name, tc.refPkg, tc.targetPkg)
		assert.Equal(t, tc.want, got, "candidates for %q ref=%q target=%q", tc.name, tc.refPkg, tc.targetPkg)
	}
}

func TestProtoTopLevelNameCandidates(t *testing.T) {
	// The #245 filter for top-level-only namespaces (annotations,
	// functions): only undotted scope-chain interpretations survive.
	cases := []struct {
		name, refScope, targetPkg string
		want                      []string
	}{
		// Bare names bind in the same package and enclosing packages…
		{"audited", "acme.v1", "acme.v1", []string{"audited"}},
		{"audited", "acme.v1", "acme", []string{"audited"}},
		// …but never across unrelated packages (the #245 tightening).
		{"audited", "acme.v1", "other.v1", nil},

		// Qualified names work fully qualified and under any
		// enclosing-scope interpretation.
		{"other.v1.audited", "acme.v1", "other.v1", []string{"audited"}},
		{"demo.v1.audited", "acme.demo.v1", "acme.demo.v1", []string{"audited"}},
		{"other.v1.audited", "acme.v1", "wrong.v1", nil},

		// Dotted interpretations that would land inside a message are
		// filtered — these namespaces can't nest.
		{"Outer.audited", "acme.v1", "acme.v1", nil},
	}
	for _, tc := range cases {
		got := protoTopLevelNameCandidates(tc.name, tc.refScope, tc.targetPkg)
		assert.Equal(t, tc.want, got, "candidates for %q ref=%q target=%q", tc.name, tc.refScope, tc.targetPkg)
	}
}

func TestFindDeclByScopedPath(t *testing.T) {
	f := parseProtoSilently("scoped.proto", []byte(`syntax = "proto3";
package acme.v1;

message Outer {
  message Inner {
    string id = 1;
  }
  enum Mode {
    MODE_UNSPECIFIED = 0;
  }
  string name = 1;
}

enum Color {
  COLOR_UNSPECIFIED = 0;
}
`))
	require.NotNil(t, f)

	inner := findDeclByScopedPath(f, "Outer.Inner")
	require.False(t, inner.IsZero(), "nested message resolves by scoped path")
	assert.Equal(t, "Inner", inner.Name().Canonicalized())

	mode := findDeclByScopedPath(f, "Outer.Mode")
	require.False(t, mode.IsZero(), "nested enum resolves by scoped path")
	assert.Equal(t, "Mode", mode.Name().Canonicalized())

	outer := findDeclByScopedPath(f, "Outer")
	require.False(t, outer.IsZero(), "single-segment path matches top level")

	assert.True(t, findDeclByScopedPath(f, "Outer.Missing").IsZero(),
		"unknown nested name misses")
	assert.True(t, findDeclByScopedPath(f, "Color.Inner").IsZero(),
		"an enum can't enclose a nested type")
	assert.True(t, findDeclByScopedPath(f, "Outer.name").IsZero(),
		"fields aren't referenceable types")
	assert.True(t, findDeclByScopedPath(f, "Missing.Inner").IsZero(),
		"unknown outer segment misses")
	assert.True(t, findDeclByScopedPath(nil, "Outer.Inner").IsZero(),
		"nil file misses gracefully")
}

func TestProtoRefScopeAt(t *testing.T) {
	// The lexical scope at a cursor is the package plus the enclosing
	// message names, outermost first.
	src := `syntax = "proto3";
package acme.v1;

message Outer {
  message Middle {
    string deep = 1;
  }
  string shallow = 1;
}

enum Color {
  COLOR_UNSPECIFIED = 0;
}
`
	f := parseProtoSilently("scope.proto", []byte(src))
	require.NotNil(t, f)

	at := func(line, char uint32) string {
		path := protoFindAt(f, protocol.Position{Line: line, Character: char}, positions.UTF16)
		return protoRefScopeAt(f, path)
	}

	assert.Equal(t, "acme.v1.Outer.Middle", at(5, 8),
		"field inside a doubly nested message")
	assert.Equal(t, "acme.v1.Outer", at(7, 4),
		"field inside the outer message")
	assert.Equal(t, "acme.v1", at(11, 4),
		"enum bodies don't contribute scope segments")
	assert.Equal(t, "acme.v1", at(1, 0),
		"top level is just the package")
	assert.Equal(t, "acme.v1", protoRefScopeAt(f, nil),
		"nil path degrades to the package")

	noPkg := parseProtoSilently("nopkg.proto", []byte(`syntax = "proto3";
message Solo {
  string x = 1;
}
`))
	require.NotNil(t, noPkg)
	path := protoFindAt(noPkg, protocol.Position{Line: 2, Character: 4}, positions.UTF16)
	assert.Equal(t, "Solo", protoRefScopeAt(noPkg, path),
		"package-less files scope from the message chain alone")
}

func TestProtoPackageContinuation(t *testing.T) {
	// The next package segment toward targetPkg, when some
	// enclosing-scope interpretation of the qualifier is a strict
	// segment-aligned prefix of it.
	cases := []struct {
		qualifier, refScope, targetPkg string
		want                           string
		ok                             bool
	}{
		{"acme", "acme.demo.v1", "acme.demo.v1", "demo", true},
		{"acme.demo", "acme.demo.v1", "acme.demo.v1", "v1", true},
		// Scope-relative: `demo` written in acme.demo.v1 names
		// acme.demo via the `acme` scope.
		{"demo", "acme.demo.v1", "acme.demo.v1", "v1", true},
		// Whole-package qualifiers aren't strict prefixes.
		{"acme.demo.v1", "acme.demo.v1", "acme.demo.v1", "", false},
		// Prefixes must be segment-aligned: `ac` is not a package step.
		{"ac", "other", "acme.demo.v1", "", false},
		{"acme", "other", "unrelated.v1", "", false},
		{"", "acme", "acme.demo.v1", "", false},
	}
	for _, tc := range cases {
		got, ok := protoPackageContinuation(tc.qualifier, tc.refScope, tc.targetPkg)
		assert.Equal(t, tc.ok, ok, "ok for %q ref=%q target=%q", tc.qualifier, tc.refScope, tc.targetPkg)
		assert.Equal(t, tc.want, got, "segment for %q ref=%q target=%q", tc.qualifier, tc.refScope, tc.targetPkg)
	}
}

func TestProtoMinimalScopedLabel(t *testing.T) {
	// The shortest suffix that binds exactly the labeled
	// declaration from the cursor's enclosing-message chain — with
	// shadowed suffixes skipped.
	f := parseProtoSilently("minimal.proto", []byte(`syntax = "proto3";
package acme.v1;

message A {
  message Dup {
    string a = 1;
  }
}

message B {
  message Dup {
    string b = 1;
  }
  message Solo {}
}
`))
	require.NotNil(t, f)

	cases := []struct {
		scoped, cursorChain, want string
	}{
		{"B.Dup", "B", "Dup"},
		{"B.Solo", "B", "Solo"},
		// A.Dup's bare suffix is shadowed by B.Dup from inside B —
		// the full path is the only label that binds it there.
		{"A.Dup", "B", "A.Dup"},
		{"A.Dup", "A", "Dup"},
		// Top level: no chain to peel against.
		{"A.Dup", "", "A.Dup"},
		{"B", "B", "B"},
	}
	for _, tc := range cases {
		got := protoMinimalScopedLabel(f, tc.scoped, tc.cursorChain)
		assert.Equal(t, tc.want, got, "label for %q at chain %q", tc.scoped, tc.cursorChain)
	}
}

func TestProtoGroupScopes(t *testing.T) {
	// A proto2 group declares a message named after it; its body is
	// that message's scope. Both the scope derivation and the
	// rooted path walk must treat groups as message scopes — without
	// this, dropping the lenient any-depth fallback would strand
	// references written inside (or naming types inside) group bodies.
	f := parseProtoSilently("groups.proto", []byte(`syntax = "proto2";
package g.v1;

message M {
  optional group Grp = 1 {
    optional string x = 2;
  }
}
`))
	require.NotNil(t, f)

	grp := findDeclByScopedPath(f, "M.Grp")
	require.False(t, grp.IsZero(), "a group resolves as a scoped-path target")
	assert.Equal(t, "Grp", grp.Name().Canonicalized())

	path := protoFindAt(f, protocol.Position{Line: 5, Character: 20}, positions.UTF16)
	assert.Equal(t, "g.v1.M.Grp", protoRefScopeAt(f, path),
		"a group body contributes its message's scope segment")
}

func TestProtoExtendScopePassthrough(t *testing.T) {
	// An extend block is not a scope: a proto2 group declared
	// inside one defines a message in the ENCLOSING scope, so the
	// rooted walk must scan extend bodies transparently — at the top
	// level and inside messages alike.
	f := parseProtoSilently("extends.proto", []byte(`syntax = "proto2";
package p;

message Extendee {
  extensions 100 to 199;
}

message Holder {
  extend Extendee {
    optional group Extra = 100 {
      optional string x = 1;
    }
  }
}

extend Extendee {
  optional group TopExtra = 101 {
    optional string y = 1;
  }
}
`))
	require.NotNil(t, f)

	extra := findDeclByScopedPath(f, "Holder.Extra")
	require.False(t, extra.IsZero(),
		"a group declared in a message-level extend resolves in the message's scope")
	assert.Equal(t, "Extra", extra.Name().Canonicalized())

	top := findDeclByScopedPath(f, "TopExtra")
	require.False(t, top.IsZero(),
		"a group declared in a file-level extend resolves at the top level")

	// The extend block itself contributes no scope segment: the
	// cursor inside the group's body scopes to p.Holder.Extra.
	path := protoFindAt(f, protocol.Position{Line: 10, Character: 25}, positions.UTF16)
	assert.Equal(t, "p.Holder.Extra", protoRefScopeAt(f, path),
		"extend contributes no segment; the group contributes its message's")
}
