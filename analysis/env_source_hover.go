// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// C7 part 2 of the chameleon-integration design: hover hint for
// fields supplied by an env-kind chain source at runtime. When
// the document is a PER_ENV chain layer and ANY layer in the
// env's resolve order references a source with `kind = "env"`,
// the field's hover gains a line:
//
//	Available env var: `MYAPP_DB_HOST` (set via env source `env_layer`)
//
// The env var name is computed via chain.EnvVarNameForPath — the
// same UPPER_SNAKE + prefix rule chameleon's runtime uses, so
// what users see at edit-time matches what the runtime will read.
//
// The hint is purely informational: an env source MIGHT supply
// the value at runtime, depending on whether the env var is
// actually set. The LSP can't observe that (and shouldn't try);
// the hint helps users discover which env var name corresponds
// to their field. Suppressing the hint when it'd be noise (no
// env source in chain, standalone doc, etc.) is the only gate.

package analysis

import (
	"fmt"

	"github.com/trendvidia/protolsp/chain"
)

// renderEnvSourceHint returns the markdown line surfacing the env
// var name for `path` when the doc's chain has an env-kind source,
// or "" when no hint applies. Caller (HoverFor) appends the
// result to the existing markdown.
//
// When multiple env-kind sources are present (rare but allowed),
// the FIRST one encountered in the resolve order wins. Tie-
// breaking by source order means users see a stable, reproducible
// hint without protolsp needing to surface every possible env
// var name (which would balloon the hover payload).
func (s *Workspace) renderEnvSourceHint(doc *Document, path string) string {
	if s == nil || doc == nil || doc.LayerInfo == nil || !doc.LayerInfo.IsLayer {
		return ""
	}
	if doc.LayerInfo.Strategy != "PER_ENV" || doc.LayerInfo.Environment == "" {
		return ""
	}
	docChain := s.ChainForDoc(doc)
	if docChain == nil || len(docChain.Sources) == 0 {
		return ""
	}
	for _, layer := range docChain.ResolveOrder(doc.LayerInfo.Environment) {
		if !docChain.LayerIsEnvSourced(layer) {
			continue
		}
		src := docChain.Sources[layer.Source]
		envVar := chain.EnvVarNameForPath(src.Options["prefix"], path)
		return fmt.Sprintf(
			"\nAvailable env var: `%s` (set via env source `%s` at runtime)\n",
			envVar, src.Name)
	}
	return ""
}
