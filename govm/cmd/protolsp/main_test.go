// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package main

import (
	"testing"

	"github.com/trendvidia/protolsp/analysis"
)

// Without the blank import this binary is the community build, and
// nothing else would say so.
func TestLinksProtocheckBackend(t *testing.T) {
	if !analysis.HasProtocheckBackend() {
		t.Fatal("no protocheck backend registered")
	}
}
