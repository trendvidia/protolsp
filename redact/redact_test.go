// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package redact

import (
	"testing"

	"go.lsp.dev/protocol"
)

func TestIsSensitiveSection(t *testing.T) {
	cases := []struct {
		section string
		want    bool
	}{
		{"protolsp.registry.token", true},
		{"protolsp.registry.password", true},
		{"protolsp.registry.secret", true},
		{"protolsp.something.Token", true},   // case-insensitive
		{"protolsp.foo.PASSWORD", true},      // case-insensitive
		{"protolsp.registry.address", false}, // not on the list
		{"protolsp.registry.namespace", false},
		{"", false},     // empty section
		{"token", true}, // bare last segment
		{"deeply.nested.path.to.secret", true},
		{"tokens", false}, // suffix match — not "token" exactly
	}
	for _, c := range cases {
		t.Run(c.section, func(t *testing.T) {
			if got := IsSensitiveSection(c.section); got != c.want {
				t.Errorf("IsSensitiveSection(%q) = %v; want %v", c.section, got, c.want)
			}
		})
	}
}

func TestDiagnostic_RedactsSecretInMessage(t *testing.T) {
	d := &protocol.Diagnostic{
		Message: `invalid value "super-secret" for field "password"`,
	}
	Diagnostic(d, []string{"super-secret"})
	if d.Message != `invalid value "<redacted>" for field "password"` {
		t.Errorf("redacted diagnostic = %q; secret bytes were not replaced", d.Message)
	}
}

func TestDiagnostic_NilSafe(t *testing.T) {
	// Nil diagnostic and empty secrets shouldn't panic. The
	// publishDiagnostics path is shared; defensive checks here
	// avoid panics from quirky call sites.
	Diagnostic(nil, []string{"x"})
	d := &protocol.Diagnostic{Message: "msg"}
	Diagnostic(d, nil)
	Diagnostic(d, []string{})
	if d.Message != "msg" {
		t.Errorf("empty secrets must leave message untouched; got %q", d.Message)
	}
}

func TestString_RedactsAllOccurrences(t *testing.T) {
	// ReplaceAll matches every literal occurrence; multiple
	// distinct secrets all get scrubbed.
	in := `token=xK9p, key=ab-12, again xK9p`
	got := String(in, []string{"xK9p", "ab-12"})
	want := `token=<redacted>, key=<redacted>, again <redacted>`
	if got != want {
		t.Errorf("String redaction:\n got %q\nwant %q", got, want)
	}
}

func TestString_EmptySecretsIsIdentity(t *testing.T) {
	in := "no secrets here"
	if got := String(in, []string{"", ""}); got != in {
		t.Errorf("empty-string secret entries must be skipped; got %q", got)
	}
	if got := String(in, nil); got != in {
		t.Errorf("nil secrets slice must be identity; got %q", got)
	}
}
