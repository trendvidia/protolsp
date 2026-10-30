// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package lspcodec

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

// A custom protolsp/* result keeps encoding/json's behavior: omitempty
// drops an empty field and a nil slice is null. json/v2, which
// protocol.Marshal falls back to, differs on both.
func TestMarshal_CustomResultsStayOnEncodingJSON(t *testing.T) {
	type result struct {
		Name  string   `json:"name,omitempty"`
		Items []string `json:"items"`
	}
	b, err := Codec{}.Marshal(result{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"items":null}`, string(b))
}

func TestMarshal_LSPTypesEncodeUnions(t *testing.T) {
	b, err := Codec{}.Marshal(&protocol.Hover{
		Contents: &protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: "x"},
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"contents":{"kind":"markdown","value":"x"}}`, string(b))
}

func TestMarshal_NilIsNull(t *testing.T) {
	b, err := Codec{}.Marshal(nil)
	require.NoError(t, err)
	assert.Equal(t, "null", string(b))
}

func TestUnmarshal_DecodesUnionsInLSPTypes(t *testing.T) {
	var d protocol.Diagnostic
	require.NoError(t, Codec{}.Unmarshal([]byte(`{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"message":"m"}`), &d))
	assert.Equal(t, protocol.String("m"), d.Message)
}

func TestUnmarshal_RawMessageCopies(t *testing.T) {
	in := []byte(`{"a":1}`)
	var raw json.RawMessage
	require.NoError(t, Codec{}.Unmarshal(in, &raw))
	in[2] = 'b'
	assert.JSONEq(t, `{"a":1}`, string(raw))
}
