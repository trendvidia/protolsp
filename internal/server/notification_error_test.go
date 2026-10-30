// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.lsp.dev/jsonrpc2"
	"go.lsp.dev/protocol"
)

// An error from a notification would close the jsonrpc2 connection, so
// one bad message would disconnect the editor. serve must log it and
// drop it. The same failure on a call is the caller's to see.
func TestServe_NotificationErrorDoesNotReachConnection(t *testing.T) {
	srv := New(nil)
	t.Cleanup(func() { _ = srv.Close() })

	bad := jsonrpc2.RawMessage(`{"textDocument":`)
	_, err := srv.serve(context.Background(), &testRequest{
		method: protocol.MethodTextDocumentHover, params: bad,
	})
	assert.NoError(t, err)

	_, err = srv.serve(context.Background(), &testRequest{
		method: protocol.MethodTextDocumentHover, params: bad, isCall: true,
	})
	assert.Error(t, err)
}
