// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDefaultFor_SectionDefaults pins the Tier 1 defaults directly:
// inline_secrets defaults to "warn" per the secret-handling design
// (S4); every registry section defaults to the empty string —
// protolsp's "no config → parse-only diagnostics" behavior.
func TestDefaultFor_SectionDefaults(t *testing.T) {
	cases := map[string]any{
		SectionRegistryAddress:       "",
		SectionRegistryNamespace:     "",
		SectionRegistryToken:         "",
		SectionRegistryCacheDir:      "",
		SectionSecurityInlineSecrets: "warn",
	}
	for section, want := range cases {
		t.Run(section, func(t *testing.T) {
			assert.Equal(t, want, defaultFor(section))
		})
	}

	// Every known section must be covered by the table above, so a
	// newly added section forces a decision about its default here.
	for _, section := range KnownSections {
		_, covered := cases[section]
		assert.True(t, covered, "KnownSections entry %q missing from the defaults table", section)
	}

	assert.Equal(t, "", defaultFor("protolsp.some.future.section"),
		"unknown sections default to the empty string")
}
