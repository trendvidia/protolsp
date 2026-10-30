// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package lspcodec encodes JSON-RPC payloads for the server's connections.
package lspcodec

import (
	"encoding/json"
	"reflect"
	"strings"

	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// Codec encodes JSON-RPC payloads. go.lsp.dev/protocol keeps its own
// codec private to protocol.NewServer, which also dispatches handlers
// concurrently; the server needs them in order, so it builds its own
// connection. LSP types go through protocol.Marshal, which understands
// their unions. Everything else, the protolsp/* results among them,
// stays on encoding/json so its omitempty and null behavior holds.
type Codec struct{}

var _ jsonrpc2.Codec = Codec{}

// Marshal implements [jsonrpc2.Codec].
func (Codec) Marshal(v any) ([]byte, error) {
	switch m := v.(type) {
	case jsonrpc2.RawMessage:
		if m == nil {
			return []byte("null"), nil
		}
		return m, nil
	case json.RawMessage:
		if m == nil {
			return []byte("null"), nil
		}
		return m, nil
	}
	if isProtocolType(reflect.TypeOf(v)) {
		return protocol.Marshal(v)
	}
	return json.Marshal(v)
}

// Unmarshal implements [jsonrpc2.Codec].
func (Codec) Unmarshal(data []byte, v any) error {
	switch p := v.(type) {
	case *jsonrpc2.RawMessage:
		*p = append(jsonrpc2.RawMessage(nil), data...)
		return nil
	case *json.RawMessage:
		*p = append(json.RawMessage(nil), data...)
		return nil
	}
	if isProtocolType(reflect.TypeOf(v)) {
		return protocol.Unmarshal(data, v)
	}
	return json.Unmarshal(data, v)
}

func isProtocolType(t reflect.Type) bool {
	for t != nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			t = t.Elem()
		default:
			return strings.HasPrefix(t.PkgPath(), "go.lsp.dev/protocol")
		}
	}
	return false
}
