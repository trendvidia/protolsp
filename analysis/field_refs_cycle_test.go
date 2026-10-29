// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCyclePathThrough_TwoNodeCycle(t *testing.T) {
	deps := map[string][]string{
		"a": {"b"},
		"b": {"a"},
	}
	pathA := cyclePathThrough("a", deps)
	require.NotNil(t, pathA)
	require.GreaterOrEqual(t, len(pathA), 3,
		"path is start → ... → start with at least two distinct nodes")
	assert.Equal(t, "a", pathA[0])
	assert.Equal(t, "a", pathA[len(pathA)-1])
	assert.Contains(t, pathA, "b")
}

func TestCyclePathThrough_SelfLoop(t *testing.T) {
	deps := map[string][]string{
		"a": {"a"},
	}
	got := cyclePathThrough("a", deps)
	assert.Equal(t, []string{"a", "a"}, got)
}

func TestCyclePathThrough_NoCycle(t *testing.T) {
	deps := map[string][]string{
		"a": {"b"},
		"b": {"c"},
	}
	assert.Nil(t, cyclePathThrough("a", deps),
		"linear chain returns nil from cyclePathThrough")
}

func TestFindCyclePaths_OnlyCycleMembers(t *testing.T) {
	deps := map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
		"d": {"a"}, // points into the cycle but isn't part of it
	}
	got := findCyclePaths(deps)
	for _, name := range []string{"a", "b", "c"} {
		_, ok := got[name]
		assert.True(t, ok, "cycle member %q must appear in result", name)
	}
	_, dInResult := got["d"]
	assert.False(t, dInResult, "fan-in node `d` is not part of any cycle")
}
