// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Command protolsp-ws serves protolsp over WebSocket, the transport
// used by the hosted Monaco IDE. For desktop editors that prefer
// stdio (VS Code's vscode-languageclient/node, Neovim, Zed), use the
// protolsp binary instead.
package main

import "github.com/trendvidia/protolsp/internal/cli"

func main() { cli.WebSocket() }
