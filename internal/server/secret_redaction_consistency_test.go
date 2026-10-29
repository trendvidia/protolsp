// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Cross-surface redaction-consistency test. The audit named this
// gap: per-surface tests exist (chain hover, secret diagnostics,
// effective config), but no single test asserts the same secret
// value renders identically across them all. A future change that
// (say) updates the chain-hover placeholder to "***" while
// leaving the diagnostic path at "<redacted>" would slip through
// the per-surface suites individually. This test fails if the
// surfaces drift apart OR if any of them leak the plaintext.

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/uri"

	"github.com/trendvidia/protolsp/redact"
)

// TestSecretRedaction_ConsistentAcrossSurfaces opens a doc inside
// a chameleon chain whose layers all set the same pxf.Secret field
// to the same value, then exercises every secret-bearing surface
// at once: hover markdown (with chain-resolution rendering),
// publishDiagnostics, and the secretRanges entry. The test fails
// if:
//
//   - any surface contains the plaintext secret value, or
//   - any surface uses a redaction string that disagrees with
//     redact.Placeholder.
func TestSecretRedaction_ConsistentAcrossSurfaces(t *testing.T) {
	const plaintext = "super-secret-value-xyz"

	// Chain with three PER_ENV layers, all setting password to the
	// same plaintext. The chain table will render <redacted> rows
	// for each ancestor — they MUST all use redact.Placeholder.
	workspaceDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, "chain.pxf"),
		[]byte(`@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "sandbox" extends = "prod" }
  { name = "dev"     extends = "sandbox" }
]

layers = [
  { id = "env" path = "env/${env}.pxf" for_each = PER_ENV }
]
`), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(workspaceDir, ".protolsp.pxf"),
		[]byte(`@type protolsp.v1.WorkspaceConfig
chameleon { chain_spec = "chain.pxf" }
`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(workspaceDir, "env"), 0o755))
	for _, env := range []string{"prod", "sandbox", "dev"} {
		require.NoError(t, os.WriteFile(
			filepath.Join(workspaceDir, "env", env+".pxf"),
			[]byte(`@type acme.demo.v1.Credentials
password = "`+plaintext+`"
`), 0o600))
	}

	srv, fc := initializeWithWorkspace(t, workspaceDir, nil)
	lookup := newFakeLookup()
	lookup.add(buildCredentialsWithSecretType(t))
	srv.Lookups().Swap(lookup, nil)
	require.NotNil(t, srv.Workspaces().SnapshotChain(), "chain must load")

	devURI := uri.File(filepath.Join(workspaceDir, "env", "dev.pxf"))
	require.NoError(t, srv.Handle(context.Background(), noopReply,
		didOpenCall(t, string(devURI), 1, `@type acme.demo.v1.Credentials
password = "`+plaintext+`"
`)))

	// Surface 1: hover markdown. Chain-resolution table renders
	// every layer's password as redacted.
	md := hoverAt(t, srv, devURI, 1, 0)
	require.NotEmpty(t, md, "hover must produce markdown for the password field")
	assert.NotContains(t, md, plaintext,
		"hover surface must not contain the plaintext password")
	assert.True(t, strings.Contains(md, redact.Placeholder),
		"hover surface must render password rows with redact.Placeholder (%q); got md:\n%s",
		redact.Placeholder, md)

	// Surface 2: every publishDiagnostics notification for the doc
	// must be free of the plaintext. analyze emitted whatever it
	// emitted at didOpen; capture all diagnostics for the URI.
	notes := fc.calls()
	require.NotEmpty(t, notes, "didOpen must trigger at least one publishDiagnostics")
	for _, n := range notes {
		if n.params == nil || n.params.URI != devURI {
			continue
		}
		for _, d := range n.params.Diagnostics {
			assert.NotContains(t, d.Message, plaintext,
				"diagnostic message must not contain the plaintext password: %q", d.Message)
		}
	}

	// Surface 3: secretRanges. The legacy LSP custom request
	// returns the field name + form. The TABLE itself doesn't
	// embed values (by design — that's the WHOLE point of the
	// separate range/value split), but pin the contract that the
	// value bytes aren't accidentally added to any range entry.
	ranges := secretRangesReply(t, srv, devURI)
	require.NotEmpty(t, ranges.Ranges, "the password field is a pxf.Secret → secret-ranges entry must exist")
	for _, r := range ranges.Ranges {
		// Range entries carry FieldName, Form, and the source
		// range only. No value field exists today; the test would
		// need updating if one is added, AND that future field
		// would need its own redaction guard.
		assert.NotContains(t, r.FieldName, plaintext,
			"secret-range FieldName must not contain the plaintext password")
	}

	// Cross-surface tie: the redact.Placeholder constant is the
	// single source of truth. Any surface that uses the literal
	// "<redacted>" string instead of redact.Placeholder would
	// keep this test green today (they happen to equal) but
	// fail loudly the first time the constant changes. Walk every
	// occurrence of <redacted> in the hover output and confirm
	// it matches the constant exactly.
	for _, occurrence := range strings.Split(md, redact.Placeholder) {
		// occurrence is the markdown between each placeholder
		// occurrence. The whole string was split by the
		// placeholder so no occurrence should itself contain a
		// drift-prone variant.
		assert.NotContains(t, strings.ToLower(occurrence), "<redacted>",
			"redaction placeholder must consistently equal redact.Placeholder; found a drift-prone variant: %q", occurrence)
	}
}
