// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentSafeEntryName(t *testing.T) {
	cases := map[string]bool{
		"loginButton":  true,
		"login_button": true,
		"a.b.c":        true, // dots are ident-part in keyed names
		"_hidden":      true,
		"row0":         true,
		"":             false,
		"log out":      false, // space → must quote
		"us-east-1":    false, // hyphen → must quote
		"0leading":     false, // digit start
		"true":         false, // value keyword
		"false":        false,
		"null":         false,
	}
	for in, want := range cases {
		assert.Equalf(t, want, identSafeEntryName(in), "identSafeEntryName(%q)", in)
	}
}

func TestNameTokenEnd(t *testing.T) {
	// Bare identifier: consumes the ident bytes, stops at the space.
	src := []byte("loginButton {")
	assert.Equal(t, len("loginButton"), nameTokenEnd(src, 0, false))

	// Quoted name: spans the whole literal including both quotes.
	q := []byte(`"log out" {`)
	assert.Equal(t, len(`"log out"`), nameTokenEnd(q, 0, true))

	// Quoted name with an escaped quote inside.
	esc := []byte(`"a\"b" {`)
	assert.Equal(t, len(`"a\"b"`), nameTokenEnd(esc, 0, true))

	// Out-of-range / malformed inputs report -1.
	assert.Equal(t, -1, nameTokenEnd(src, 99, false))
	assert.Equal(t, -1, nameTokenEnd([]byte("x"), 0, true)) // quoted flag but no opening quote
}

func TestIsValidKeyedEntryKey(t *testing.T) {
	assert.True(t, IsValidKeyedEntryKey("loginButton"))
	assert.True(t, IsValidKeyedEntryKey("us east"), "non-identifier keys are allowed (quoted on write)")
	assert.True(t, IsValidKeyedEntryKey("us-east-1"))
	assert.False(t, IsValidKeyedEntryKey(""), "empty key is not valid")
	assert.False(t, IsValidKeyedEntryKey("line\nbreak"), "control chars can't appear even quoted")
}
