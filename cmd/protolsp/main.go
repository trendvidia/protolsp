// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Command protolsp is the Language Server for .pxf (Proto eXpressive
// Format) files. It speaks LSP over stdio against a single client
// (typically VS Code, Neovim, Zed, or similar). For the WebSocket
// variant used by the hosted Monaco IDE, see cmd/protolsp-ws (added in
// milestone M5).
package main

import "github.com/trendvidia/protolsp/internal/cli"

func main() { cli.Stdio() }
