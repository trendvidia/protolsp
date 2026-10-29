// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package interp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFindAll_NoRefsReturnsNil(t *testing.T) {
	assert.Nil(t, FindAll(""))
	assert.Nil(t, FindAll("just a plain string"))
	assert.Nil(t, FindAll("a $variable not in braces"))
	assert.Nil(t, FindAll("a {bare} brace"))
}

func TestFindAll_SingleSegment(t *testing.T) {
	got := FindAll("${HOST}")
	assert.Len(t, got, 1)
	assert.Equal(t, "HOST", got[0].Path)
	assert.Equal(t, 0, got[0].Start)
	assert.Equal(t, 7, got[0].End)
}

func TestFindAll_DottedPath(t *testing.T) {
	got := FindAll("${env.PROTOREGISTRY_TOKEN}")
	assert.Len(t, got, 1)
	assert.Equal(t, "env.PROTOREGISTRY_TOKEN", got[0].Path)
}

func TestFindAll_DeepPath(t *testing.T) {
	got := FindAll("${database.replica.primary.host}")
	assert.Len(t, got, 1)
	assert.Equal(t, "database.replica.primary.host", got[0].Path)
}

func TestFindAll_MultipleInOneString(t *testing.T) {
	src := "url=${env.PROTOCOL}://${env.HOST}:${env.PORT}/${tenant.slug}"
	got := FindAll(src)
	assert.Len(t, got, 4)
	paths := []string{}
	for _, r := range got {
		paths = append(paths, r.Path)
	}
	assert.Equal(t, []string{"env.PROTOCOL", "env.HOST", "env.PORT", "tenant.slug"}, paths)
}

func TestFindAll_RejectsInvalidShapes(t *testing.T) {
	// Segments starting with digits are not identifiers.
	assert.Nil(t, FindAll("${1bad}"))
	// Trailing dot — last segment is empty, doesn't match the
	// identifier rule.
	assert.Nil(t, FindAll("${env.}"))
	// Unclosed.
	assert.Nil(t, FindAll("${env.HOST"))
	// Whitespace inside braces.
	assert.Nil(t, FindAll("${ env.HOST }"))
	// Dashes — not allowed in identifier segments.
	assert.Nil(t, FindAll("${env.MY-HOST}"))
}

func TestFindAll_OffsetsArePreciseForUnicodeSource(t *testing.T) {
	// The Ref offsets are BYTE offsets, not rune indices —
	// downstream callers use them to slice the original byte
	// buffer.
	src := "héllo ${env.HOST}"
	got := FindAll(src)
	assert.Len(t, got, 1)
	// "héllo " in UTF-8 is 7 bytes (h, é=2 bytes, l, l, o, space).
	assert.Equal(t, 7, got[0].Start)
	assert.Equal(t, "${env.HOST}", src[got[0].Start:got[0].End])
}

func TestReplaceAll_ResolveAllPaths(t *testing.T) {
	got := ReplaceAll("hello ${name}", func(path string) (string, bool) {
		if path == "name" {
			return "world", true
		}
		return "", false
	})
	assert.Equal(t, "hello world", got)
}

func TestReplaceAll_LeaveUnresolvedLiteral(t *testing.T) {
	// fn returns false → reference kept verbatim. This is the
	// "resolve env, leave field-refs alone" pattern
	// workspaceconfig.interpolate uses.
	got := ReplaceAll("a=${env.HOME} b=${field.x}", func(path string) (string, bool) {
		if strings.HasPrefix(path, "env.") {
			return "/home/me", true
		}
		return "", false
	})
	assert.Equal(t, "a=/home/me b=${field.x}", got)
}

func TestReplaceAll_NoRefsIsIdentity(t *testing.T) {
	got := ReplaceAll("plain text", func(string) (string, bool) {
		t.Fatal("fn must not be called when no refs are present")
		return "", false
	})
	assert.Equal(t, "plain text", got)
}

func TestReplaceAll_EmptyStringSubstitution(t *testing.T) {
	// Resolving to "" is the common os.Getenv case for unset
	// vars; the reference should disappear cleanly.
	got := ReplaceAll("prefix=${env.UNSET}-suffix", func(string) (string, bool) {
		return "", true
	})
	assert.Equal(t, "prefix=-suffix", got)
}
