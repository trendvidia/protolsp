// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"
)

func TestIsLiteralSecret(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"super-secret", true},
		{"hello", true},
		{"${env.X}", false},
		{"  ${env.X}  ", false}, // leading whitespace shouldn't fool the check
		{"", false},             // empty → not a literal worth warning about
		{"   ", false},          // whitespace-only same as empty
		{"$not-a-ref", true},    // single $ without brace = literal
		{"${vault.foo}", false}, // any ${...} prefix counts as reference
	}
	for _, c := range cases {
		t.Run(c.value, func(t *testing.T) {
			if got := isLiteralSecret(c.value); got != c.want {
				t.Errorf("isLiteralSecret(%q) = %v; want %v", c.value, got, c.want)
			}
		})
	}
}

func TestEnvVarNameFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"password", "PASSWORD"},
		{"credentials.password", "CREDENTIALS_PASSWORD"},
		{"outer.inner.api_key", "OUTER_INNER_API_KEY"},
		{"", ""},
		{"already_snake", "ALREADY_SNAKE"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := envVarNameFromPath(c.path); got != c.want {
				t.Errorf("envVarNameFromPath(%q) = %q; want %q", c.path, got, c.want)
			}
		})
	}
}
