// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/protocol"
)

// TestConfigDiagnostics_MissingLayerFileHints pins the
// chain-spec-linter behavior: layer paths that don't resolve to any
// existing file on disk surface as Hint-severity diagnostics — the
// "I edited the layer path and forgot to update the dir" trap.
func TestConfigDiagnostics_MissingLayerFileHints(t *testing.T) {
	// Real on-disk workspace because the lint stats the filesystem.
	dir := t.TempDir()
	chainPath := filepath.Join(dir, "chain.pxf")
	const chainSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
environments = [
  { name = "prod" }
  { name = "dev" extends = "prod" }
]
layers = [
  { id = "base"       path = "layers/base.pxf"       for_each = ONCE   required = true }
  { id = "env"        path = "layers/env/${env}.pxf" for_each = PER_ENV }
  { id = "secrets-prod" path = "wrong/${env}.sops.pxf" for_each = ONCE source = "secrets" }
  { id = "env-runtime" path = "(env)"                for_each = ONCE source = "env" }
]
`
	require.NoError(t, os.WriteFile(chainPath, []byte(chainSpec), 0o600))
	// Materialize only the env layer for env=dev — base / prod env /
	// secrets-prod all miss. Expected outcome:
	//   - base layer       → Hint (no file)
	//   - env layer        → no hint (dev.pxf exists, that's enough)
	//   - secrets-prod     → Hint (no file across any env expansion)
	//   - env-runtime      → no hint (sentinel; explicitly skipped)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "layers/env"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "layers/env/dev.pxf"), nil, 0o600))

	diags := chainSpecDiagnostics(chainPath, []byte(chainSpec))

	hintCount := 0
	var hintMessages []string
	for _, d := range diags {
		if d.Severity == protocol.DiagnosticSeverityHint && optText(d.Source) == "protolsp/chainspec" {
			hintCount++
			hintMessages = append(hintMessages, MessageText(d.Message))
		}
	}
	assert.Equal(t, 2, hintCount,
		"two layers (base, secrets-prod) resolve to no extant file; one hint each")
	joined := strings.Join(hintMessages, " | ")
	assert.Contains(t, joined, `chain layer "base"`,
		"base layer's missing file fires a hint")
	assert.Contains(t, joined, `chain layer "secrets-prod"`,
		"secrets-prod's wrong-dir path fires a hint")
	assert.NotContains(t, joined, `chain layer "env"`,
		"env layer with at least one matching env doesn't fire")
	assert.NotContains(t, joined, `chain layer "env-runtime"`,
		"env-source sentinel layer is excluded")
}
