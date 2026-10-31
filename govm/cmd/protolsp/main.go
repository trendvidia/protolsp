// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Command protolsp is cmd/protolsp with the GoVM protocheck backend linked in.
package main

import (
	_ "github.com/trendvidia/protolsp/govm"
	"github.com/trendvidia/protolsp/internal/cli"
)

func main() { cli.Stdio() }
