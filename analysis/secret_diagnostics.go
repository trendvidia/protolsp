// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// S3 of the secret-handling design: emit a Warning diagnostic on
// every literal scalar pxf.Secret value, with a Quick Fix that
// rewrites the literal as an ${env.NAME} reference. The check
// runs against the SecretRange table populated by S1
// (collectSecretRanges); diagnosticsFor includes the warnings
// alongside the existing schema findings.
//
// Reference-syntax values (anything starting with `${`) are
// skipped — they're already non-literal at edit time and become
// the resolved value at chameleon-run time. Empty values are
// also skipped — they're typically unfinished input, and
// "replace empty literal with an env ref" is a confusing nudge.

package analysis

import (
	"fmt"
	"strings"

	"go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

const (
	// DiagDataKindLiteralSecret marks a diagnostic produced for
	// a literal pxf.Secret value. The code-action handler reads
	// the payload to build the env-reference quick fix.
	DiagDataKindLiteralSecret = "literal_secret"
)

// LiteralSecretData is the JSON shape of Diagnostic.Data for
// literal-secret warnings. FieldPath drives the env-var name
// derivation in the quick fix. The Range comes back via
// Diagnostic.Range; no need to duplicate it here.
type LiteralSecretData struct {
	Kind      string `json:"kind"`
	FieldPath string `json:"field_path"`
}

// literalSecretDiagnostics walks the document's SecretRanges and
// emits a diagnostic per literal value. Severity is governed by
// the effective inline_secrets policy (S4): allow → no diagnostic;
// warn → Warning (default); refuse → Error.
//
// Diagnostic source is "protolsp/security" — distinct from
// "protolsp/schema" so editors can color / filter security
// findings separately if they want.
func (s *Workspace) literalSecretDiagnostics(doc *Document) []protocol.Diagnostic {
	if doc == nil || len(doc.SecretRanges) == 0 {
		return nil
	}
	// .sops.pxf files are designed to carry literal secret values
	// — sops encrypts them at rest, and any editor that opens such
	// a file is showing the decrypted plaintext temporarily for
	// editing. Suggesting a vault reference (${env.X}) for those
	// values is wrong: the file IS the vault. Suppress the
	// diagnostic for the .sops.pxf convention so editors don't
	// surface a noisy warning that misrepresents the file's role.
	//
	// Suffix match is the simplest sufficient signal. Consumers
	// with a different convention can route their documents
	// through a different URI suffix or run analysis on a
	// transformed copy if they need this protection — revisit if
	// a non-pxfed consumer surfaces a concrete need.
	if strings.HasSuffix(string(doc.URI), ".sops.pxf") {
		return nil
	}
	policy := s.effectiveInlineSecretsPolicy()
	if policy == "allow" {
		return nil
	}
	severity := protocol.DiagnosticSeverityWarning
	if policy == "refuse" {
		severity = protocol.DiagnosticSeverityError
	}
	values := doc.secretValues // server-internal; safe to read directly here
	var out []protocol.Diagnostic
	for i, sr := range doc.SecretRanges {
		if i >= len(values) {
			break // defensive — slices should always be parallel
		}
		value := values[i]
		if !isLiteralSecret(value) {
			continue
		}
		envName := envVarNameFromPath(sr.Path)
		out = append(out, protocol.Diagnostic{
			Range:    sr.Range,
			Severity: severity,
			Source:   optString("protolsp/security"),
			Message: protocol.String(fmt.Sprintf(
				"literal secret value at %q — consider a vault reference (e.g. ${env.%s})",
				sr.Path, envName,
			)),
			Data: mustAny(LiteralSecretData{
				Kind:      DiagDataKindLiteralSecret,
				FieldPath: sr.Path,
			}),
		})
	}
	return out
}

// effectiveInlineSecretsPolicy resolves the inline_secrets policy
// through the four-tier config-protocol model. Returns one of
// "allow" / "warn" / "refuse"; never empty (the default tier
// supplies "warn"). Unrecognized values fall back to "warn".
func (s *Workspace) effectiveInlineSecretsPolicy() string {
	if s == nil {
		return "warn"
	}
	cv := s.ResolveSection(SectionSecurityInlineSecrets)
	v, ok := cv.Value.(string)
	if !ok {
		return "warn"
	}
	switch v {
	case "allow", "warn", "refuse":
		return v
	}
	return "warn"
}

// isLiteralSecret reports whether value is a literal (warning-
// worthy) vs a reference (already non-literal at edit time).
// Reference-syntax: leading `${...` after whitespace trim.
//
// Empty strings are NOT literals for this purpose — they're
// typically unfinished input and a "replace literal with env
// ref" warning would be confusing.
func isLiteralSecret(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "${") {
		return false
	}
	return true
}

// envVarNameFromPath derives an env var name from a dotted field
// path. Replaces dots with underscores and upper-cases the result.
// Examples:
//   - "password"             → "PASSWORD"
//   - "credentials.password" → "CREDENTIALS_PASSWORD"
//   - "outer.inner.api_key"  → "OUTER_INNER_API_KEY"
//
// Chameleon-configured workspaces with an env source prefix have a
// different convention (see chameleon-integration.md §"Env-source
// hover hint"). When chameleon integration lands, this can grow a
// workspace-aware variant; today's simple rule covers the common
// case and gives users a starting point they can edit.
func envVarNameFromPath(path string) string {
	if path == "" {
		return ""
	}
	return strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
}

// DecodeLiteralSecretData turns a Diagnostic.Data round-tripped
// through the LSP client back into the typed payload. Same shape
// as DecodeUnknownFieldData — handles the typed struct (in-process
// tests) and the map[string]any form (real JSON-RPC clients).
func DecodeLiteralSecretData(v interface{}) *LiteralSecretData {
	v = diagDataValue(v)
	if v == nil {
		return nil
	}
	if d, ok := v.(LiteralSecretData); ok && d.Kind == DiagDataKindLiteralSecret {
		return &d
	}
	if dp, ok := v.(*LiteralSecretData); ok && dp != nil && dp.Kind == DiagDataKindLiteralSecret {
		return dp
	}
	if m, ok := v.(map[string]interface{}); ok {
		kind, _ := m["kind"].(string)
		if kind != DiagDataKindLiteralSecret {
			return nil
		}
		path, _ := m["field_path"].(string)
		return &LiteralSecretData{Kind: kind, FieldPath: path}
	}
	return nil
}

// replaceLiteralWithEnvRefAction builds the quick-fix CodeAction
// that swaps a literal secret for an ${env.X} reference. The edit
// replaces the entire string literal (open quote through close
// quote) with `"${env.NAME}"` (with quotes — the value position
// in the source needs valid PXF syntax after the replacement).
func replaceLiteralWithEnvRefAction(doc *Document, diag protocol.Diagnostic, data LiteralSecretData) protocol.CodeAction {
	envName := envVarNameFromPath(data.FieldPath)
	edit := protocol.TextEdit{
		Range:   diag.Range,
		NewText: fmt.Sprintf(`"${env.%s}"`, envName),
	}
	return protocol.CodeAction{
		Title:       fmt.Sprintf("Replace literal with ${env.%s}", envName),
		Kind:        quickFixKind(),
		Diagnostics: []protocol.Diagnostic{diag},
		Edit: &protocol.WorkspaceEdit{
			Changes: map[uri.URI][]protocol.TextEdit{
				doc.URI: {edit},
			},
		},
	}
}
