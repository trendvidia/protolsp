// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractValueAtPath_TopLevelString(t *testing.T) {
	v, ok := extractValueAtPath([]byte(`@type c4test.v1.AppConfig
api_url = "hi"
`), []string{"api_url"})
	require.True(t, ok)
	assert.Equal(t, `"hi"`, v)
}

func TestExtractValueAtPath_NestedAssignment(t *testing.T) {
	v, ok := extractValueAtPath([]byte(`@type c4test.v1.AppConfig
db { host = "abc" }
`), []string{"db", "host"})
	require.True(t, ok)
	assert.Equal(t, `"abc"`, v)
}

func TestExtractValueAtPath_BlockAtRootRendersPlaceholder(t *testing.T) {
	v, ok := extractValueAtPath([]byte(`@type c4test.v1.AppConfig
db { host = "x" }
`), []string{"db"})
	require.True(t, ok)
	assert.Equal(t, "{...}", v,
		"hovering on the block name itself renders {...}, not the inner fields")
}

func TestExtractValueAtPath_MissingPathReturnsFalse(t *testing.T) {
	_, ok := extractValueAtPath([]byte(`@type c4test.v1.AppConfig
api_url = "x"
`), []string{"nonexistent"})
	assert.False(t, ok)
}

func TestExtractValueAtPath_UnparseableReturnsFalse(t *testing.T) {
	_, ok := extractValueAtPath([]byte(`@type x { unclosed`), []string{"api_url"})
	assert.False(t, ok)
}
