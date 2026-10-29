// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trendvidia/protolsp/positions"
)

func TestApplyOneChange_FullDocumentReplacement(t *testing.T) {
	src := []byte("old content")
	got, err := applyOneChange(src, IncrementalChange{Text: "new content"}, positions.UTF16)
	require.NoError(t, err)
	assert.Equal(t, "new content", string(got))
}

func TestApplyOneChange_InsertAtCursor(t *testing.T) {
	src := []byte("hello world")
	got, err := applyOneChange(src, IncrementalChange{
		Range: &ProtocolRangeShape{
			Start: PositionShape{Line: 0, Character: 6},
			End:   PositionShape{Line: 0, Character: 6},
		},
		Text: "beautiful ",
	}, positions.UTF16)
	require.NoError(t, err)
	assert.Equal(t, "hello beautiful world", string(got))
}

func TestApplyOneChange_Replace(t *testing.T) {
	src := []byte("hello world")
	got, err := applyOneChange(src, IncrementalChange{
		Range: &ProtocolRangeShape{
			Start: PositionShape{Line: 0, Character: 6},
			End:   PositionShape{Line: 0, Character: 11},
		},
		Text: "Earth",
	}, positions.UTF16)
	require.NoError(t, err)
	assert.Equal(t, "hello Earth", string(got))
}

func TestApplyOneChange_Delete(t *testing.T) {
	src := []byte("hello cruel world")
	got, err := applyOneChange(src, IncrementalChange{
		Range: &ProtocolRangeShape{
			Start: PositionShape{Line: 0, Character: 5},
			End:   PositionShape{Line: 0, Character: 11},
		},
		Text: "",
	}, positions.UTF16)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(got))
}
