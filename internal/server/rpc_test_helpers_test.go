// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"

	"github.com/trendvidia/protolsp/analysis"
	"github.com/trendvidia/protolsp/internal/lspcodec"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// testRequest stands in for *jsonrpc2.Request, which only a live
// connection can build.
type testRequest struct {
	method string
	params jsonrpc2.RawMessage
	isCall bool
}

func (r *testRequest) Method() string              { return r.method }
func (r *testRequest) Params() jsonrpc2.RawMessage { return r.params }
func (r *testRequest) IsCall() bool                { return r.isCall }

func encodeParams(params any) (jsonrpc2.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return lspcodec.Codec{}.Marshal(params)
}

func newTestCall(_ jsonrpc2.ID, method string, params any) (rpcRequest, error) {
	raw, err := encodeParams(params)
	if err != nil {
		return nil, err
	}
	return &testRequest{method: method, params: raw, isCall: true}, nil
}

func newTestNotification(method string, params any) (rpcRequest, error) {
	raw, err := encodeParams(params)
	if err != nil {
		return nil, err
	}
	return &testRequest{method: method, params: raw}, nil
}

type testReplier = func(ctx context.Context, result any, err error) error

// testHandle runs one message through the server and hands its outcome
// to reply, the way the connection would answer it.
func testHandle(s *Server, ctx context.Context, reply testReplier, req rpcRequest) error {
	result, err := s.serve(ctx, req)
	if !req.IsCall() {
		return nil
	}
	return reply(ctx, result, err)
}

func hoverBody(h *protocol.Hover) string {
	if h == nil {
		return ""
	}
	if c, ok := h.Contents.(*protocol.MarkupContent); ok && c != nil {
		return c.Value
	}
	return ""
}

func optValue(o protocol.Optional[string]) string {
	v, _ := o.Get()
	return v
}

var messageText = analysis.MessageText

func boolRef(b bool) *bool { return &b }

func testAny(v any) protocol.LSPAny {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func patternText(g protocol.GlobPattern) string {
	p, _ := g.(protocol.Pattern)
	return string(p)
}

func workspaceFolders(folders ...protocol.WorkspaceFolder) protocol.WorkspaceFoldersInitializeParams {
	return protocol.WorkspaceFoldersInitializeParams{
		WorkspaceFolders: protocol.NewNullable(folders),
	}
}

func fileEvents(in []*protocol.FileEvent) []protocol.FileEvent {
	out := make([]protocol.FileEvent, len(in))
	for i, e := range in {
		out[i] = *e
	}
	return out
}

// detailText reads a string field that the protocol types as either
// Optional or a pointer.
func detailText(v any) string {
	switch x := v.(type) {
	case protocol.Optional[string]:
		return optValue(x)
	case *string:
		if x != nil {
			return *x
		}
	}
	return ""
}
