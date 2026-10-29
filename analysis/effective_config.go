// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// CP2 of the config-protocol design: protolsp/getEffectiveConfig
// returns the effective value for each configuration section along
// with its source tier (default / file / settings / override). The
// four-tier model:
//
//   Tier 1 (default)  — compiled into protolsp
//   Tier 2 (file)     — .protolsp.pxf (not implemented until W1)
//   Tier 3 (settings) — from initializationOptions seed or
//                       workspace/configuration pull (CP1)
//   Tier 4 (override) — protolsp/setOverrides (not implemented
//                       until CP3)
//
// CP2 today: Tier 1 and Tier 3 are real; Tier 2 and Tier 4 always
// return false on lookup. Adding them is purely additive — the
// resolution chain in ResolveSection picks up file / override
// sources as soon as their state exists.

package analysis

// MethodGetEffectiveConfig is the custom LSP request that returns
// the four-tier-resolved configuration for the running server. See
// docs/design/config-protocol.md §"Custom protolsp methods".
const MethodGetEffectiveConfig = "protolsp/getEffectiveConfig"

// ConfigSource names a tier in the four-tier model. JSON strings
// match the design doc.
type ConfigSource string

const (
	SourceDefault  ConfigSource = "default"
	SourceFile     ConfigSource = "file"
	SourceSettings ConfigSource = "settings"
	SourceOverride ConfigSource = "override"
)

// ConfigValue is the per-section payload in getEffectiveConfig
// responses. Value is the resolved value (typed per the section);
// Source names which tier contributed it. Origin is a short
// human-readable hint — the originating section name for file /
// settings, or "session" for an override.
type ConfigValue struct {
	Value  any          `json:"value"`
	Source ConfigSource `json:"source"`
	Origin string       `json:"origin,omitempty"`
}

// GetEffectiveConfigParams is the request shape. workspaceDir is
// reserved for multi-root support (W4 of workspace-config); CP2
// ignores it. sections lets clients narrow the response to a
// subset; empty means "all known sections."
//
// revealSecrets opts out of the default name-pattern redaction —
// editors that want to display real token / password / secret
// values (typically behind an explicit user action) pass true.
type GetEffectiveConfigParams struct {
	WorkspaceDir  string   `json:"workspaceDir,omitempty"`
	Sections      []string `json:"sections,omitempty"`
	RevealSecrets bool     `json:"revealSecrets,omitempty"`
}

// GetEffectiveConfigResult is the response shape. Values is keyed
// by section name. workspaceDir echoes the requested scope (empty
// for ambient until W4 lands).
type GetEffectiveConfigResult struct {
	WorkspaceDir string                 `json:"workspaceDir"`
	Values       map[string]ConfigValue `json:"values"`
}

// KnownSections enumerates every config section getEffectiveConfig
// knows about today. Drives the "all sections" default. New sections
// (chameleon.*, security.*, etc.) get added here as their features
// land.
var KnownSections = []string{
	SectionRegistryAddress,
	SectionRegistryNamespace,
	SectionRegistryToken,
	SectionRegistryCacheDir,
	SectionSecurityInlineSecrets,
}

// ResolveSection walks the four tiers from highest to lowest
// precedence and returns the first one with a non-empty value.
// Tier 2 (file) is stubbed today — resolveFromFile returns
// (nil, false) until W1 lands. Tier 4 (override) reads from
// Server.overrides which CP3 populates via protolsp/setOverrides.
//
// If no tier supplies a value, falls back to the Tier 1 default
// (typically the empty string).
func (s *Workspace) ResolveSection(section string) ConfigValue {
	if v, ok := s.resolveFromOverride(section); ok {
		return ConfigValue{Value: v, Source: SourceOverride, Origin: "session"}
	}
	if v, ok := s.resolveFromSettings(section); ok {
		return ConfigValue{Value: v, Source: SourceSettings, Origin: section}
	}
	if v, ok := s.resolveFromFile(section); ok {
		return ConfigValue{Value: v, Source: SourceFile, Origin: section}
	}
	return ConfigValue{Value: defaultFor(section), Source: SourceDefault}
}

// resolveFromSettings returns the current Tier 3 value for a section
// and whether the server has one. Settings values come from either
// the initializationOptions seed or a workspace/configuration pull
// — both populate Server.EditorConfig.
//
// "Has a value" means non-empty string. Empty/zero values defer to
// lower tiers (or the default).
func (s *Workspace) resolveFromSettings(section string) (any, bool) {
	if s == nil {
		return nil, false
	}
	cfg := s.SnapshotEditorConfig()
	reg := cfg.Registry
	switch section {
	case SectionRegistryAddress:
		if reg.Address != "" {
			return reg.Address, true
		}
	case SectionRegistryNamespace:
		if reg.Namespace != "" {
			return reg.Namespace, true
		}
	case SectionRegistryToken:
		if reg.Token != "" {
			return reg.Token, true
		}
	case SectionRegistryCacheDir:
		if reg.CacheDir != "" {
			return reg.CacheDir, true
		}
	case SectionSecurityInlineSecrets:
		if cfg.InlineSecrets != "" {
			return cfg.InlineSecrets, true
		}
	}
	return nil, false
}

// resolveFromFile returns the Tier 2 value for a section by reading
// from Server.fileConfig (parsed from .protolsp.pxf at initialize
// time per workspace-config W1). Returns (nil, false) when no file
// was loaded or the section's value is empty in the file.
func (s *Workspace) resolveFromFile(section string) (any, bool) {
	if s == nil {
		return nil, false
	}
	fc := s.SnapshotFileConfig()
	if fc == nil {
		return nil, false
	}
	reg := fc.Registry
	switch section {
	case SectionRegistryAddress:
		if reg.Address != "" {
			return reg.Address, true
		}
	case SectionRegistryNamespace:
		if reg.Namespace != "" {
			return reg.Namespace, true
		}
	case SectionRegistryToken:
		if reg.Token != "" {
			return reg.Token, true
		}
	case SectionRegistryCacheDir:
		if reg.CacheDir != "" {
			return reg.CacheDir, true
		}
	case SectionSecurityInlineSecrets:
		if fc.Security.InlineSecrets != "" {
			return fc.Security.InlineSecrets, true
		}
	}
	return nil, false
}

// resolveFromOverride returns the Tier 4 value for a section by
// reading from Server.overrides under the overrides mutex. Populated
// by protolsp/setOverrides (CP3); cleared on LSP restart by design
// (no persistence). Returns (nil, false) when no override is set,
// regardless of the underlying section's other tiers.
func (s *Workspace) resolveFromOverride(section string) (any, bool) {
	if s == nil {
		return nil, false
	}
	s.cfg.overridesMu.Lock()
	defer s.cfg.overridesMu.Unlock()
	if s.cfg.overrides == nil {
		return nil, false
	}
	v, ok := s.cfg.overrides[section]
	return v, ok
}

// defaultFor returns the Tier 1 default value for a section.
// Empty string for registry sections — protolsp's "no config →
// parse-only diagnostics" behavior is the default. Future sections
// (log_level = "info" etc.) get their defaults here.
func defaultFor(section string) any {
	switch section {
	case SectionSecurityInlineSecrets:
		// Default policy per the secret-handling design (S4): warn.
		// Editors that want allow / refuse opt in via Tier 2 or 3.
		return "warn"
	}
	return ""
}
