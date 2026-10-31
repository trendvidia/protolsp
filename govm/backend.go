// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package govm registers the GoVM protocheck backend with protolsp.
// Import it for its side effect. It is a separate module because govm
// and protocheck-govm are private, and a requirement on them in
// protolsp's own go.mod would break `go mod tidy` for everyone else.
package govm

import (
	protocheckgovm "github.com/trendvidia/protocheck-govm"
	protocheck "github.com/trendvidia/protocheck/v2"

	"github.com/trendvidia/protolsp/analysis"
)

func init() {
	analysis.RegisterProtocheckBackend(analysis.ProtocheckBackend{
		NewEngine: func() (protocheck.ExpressionEngine, error) {
			return protocheckgovm.New()
		},
		CheckExpressions: analyzeProtocheckExpressions,
	})
}
