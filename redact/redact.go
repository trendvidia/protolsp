// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package redact holds the helpers that prevent secret bytes from
// leaking through LSP wire payloads and server logs. Two redaction
// surfaces ship today:
//
//   - IsSensitiveSection — name-pattern redaction for config
//     sections the server returns via protolsp/getEffectiveConfig.
//     Reveals to the user that a value exists without exposing it.
//   - Diagnostic / String — SecretRange-aware substitution for
//     diagnostic messages and hover markdown that might otherwise
//     embed pxf.Secret values verbatim.
//
// Both are deliberately conservative: false positives (over-
// redaction) are fine, false negatives (a real secret leaking
// through) are not.
package redact

import (
	"strings"

	"go.lsp.dev/protocol"
)

// Placeholder rendered in place of a redacted value.
const Placeholder = "<redacted>"

// IsSensitiveSection reports whether a config section name should
// have its value redacted by default. Matches sections whose final
// dotted segment is "token", "password", or "secret" (case-
// insensitive). Editors opt in to seeing the real value via
// `revealSecrets: true` on protolsp/getEffectiveConfig.
//
// The list is name-pattern based today. Once the secret-handling
// design's SecretRange table lands, we'll also consult schema-typed
// `pxf.Secret` fields — that's a richer signal that doesn't depend
// on naming conventions. Both layers are intentional defense in
// depth.
func IsSensitiveSection(section string) bool {
	if section == "" {
		return false
	}
	last := section
	if idx := strings.LastIndexByte(section, '.'); idx >= 0 {
		last = section[idx+1:]
	}
	switch strings.ToLower(last) {
	case "token", "password", "secret":
		return true
	}
	return false
}

// Diagnostic mutates d's Message field in place, replacing any
// substring that matches an entry in secretValues with
// Placeholder. d.Range and other fields are untouched — the
// diagnostic still tells the user WHERE the problem is, just not
// WHAT value tripped it. Safe to call on every published
// diagnostic; a nil/empty secretValues slice is a no-op.
//
// Matching is literal substring (ReplaceAll). Short / common
// secret values (e.g. "x") could in principle false-match unrelated
// message text; the practical mitigation is that LSP error messages
// quote source bytes only when those bytes triggered the error, so
// the overlap is the intended one. If false-positives become real,
// future passes can match by source-range overlap instead of
// substring.
func Diagnostic(d *protocol.Diagnostic, secretValues []string) {
	if d == nil || len(secretValues) == 0 {
		return
	}
	switch m := d.Message.(type) {
	case protocol.String:
		d.Message = protocol.String(String(string(m), secretValues))
	case *protocol.MarkupContent:
		if m != nil {
			d.Message = &protocol.MarkupContent{Kind: m.Kind, Value: String(m.Value, secretValues)}
		}
	}
}

// String redacts any secretValues substring in s and returns the
// result. Used by hover markdown, completion details, code-action
// preview text, and slog message formatting — anywhere
// source-positioned content could otherwise echo a secret.
//
// O(N*M) in the number of secrets × source length; secrets are
// always few (one per pxf.Secret field in the document) so this
// is fine.
func String(s string, secretValues []string) string {
	if s == "" || len(secretValues) == 0 {
		return s
	}
	for _, v := range secretValues {
		if v == "" {
			continue
		}
		s = strings.ReplaceAll(s, v, Placeholder)
	}
	return s
}
