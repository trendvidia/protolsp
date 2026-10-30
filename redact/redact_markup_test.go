// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package redact

import (
	"testing"

	"go.lsp.dev/protocol"
)

func TestDiagnostic_RedactsSecretInMarkupMessage(t *testing.T) {
	original := &protocol.MarkupContent{Kind: protocol.MarkupKindMarkdown, Value: "value `super-secret` is bad"}
	d := &protocol.Diagnostic{Message: original}
	Diagnostic(d, []string{"super-secret"})
	m, ok := d.Message.(*protocol.MarkupContent)
	if !ok {
		t.Fatalf("message type changed: %T", d.Message)
	}
	if m.Value != "value `<redacted>` is bad" {
		t.Errorf("redacted markup = %q", m.Value)
	}
	if m.Kind != protocol.MarkupKindMarkdown {
		t.Errorf("kind = %q", m.Kind)
	}
}
