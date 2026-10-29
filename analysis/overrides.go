// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// CP3 of the config-protocol design: protolsp/setOverrides lets the
// editor push transient session overrides (Tier 4) onto the running
// server. Overrides win over file (Tier 2), editor settings (Tier 3),
// and compiled-in defaults (Tier 1). They live in memory only —
// cleared on LSP restart by design. Persisting them is the user's
// job (put it in Tier 3 or Tier 2).
//
// After applying overrides, the server emits
// protolsp/effectiveConfigChanged with the *effective* changes —
// only sections whose resolved value actually shifted. Same-value
// overrides don't generate notification traffic.
//
// Section validation: unknown sections are rejected with
// InvalidParams. Value-type validation is lightweight in CP3 (all
// current sections are strings); when richer types arrive (chameleon
// block, dataset thresholds), the typeOf helper extends to match.

package analysis

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/trendvidia/protolsp/redact"
)

// MethodSetOverrides and MethodEffectiveConfigChanged are the CP3
// additions to the config-protocol custom-method surface.
const (
	MethodSetOverrides           = "protolsp/setOverrides"
	MethodEffectiveConfigChanged = "protolsp/effectiveConfigChanged"
)

// SetOverridesParams is the request shape. WorkspaceDir is reserved
// for multi-root (W4 of workspace-config) and ignored in CP3.
// Overrides maps section name → new value; explicit `null` clears
// any existing override (lower tiers retake the section).
type SetOverridesParams struct {
	WorkspaceDir string         `json:"workspaceDir,omitempty"`
	Overrides    map[string]any `json:"overrides"`
}

// SetOverridesApplied is one entry of the SetOverridesResult.applied
// map. PreviousSource lets editors render "Revert" actions that
// reset to the prior state without needing to know what it was.
type SetOverridesApplied struct {
	PreviousSource ConfigSource `json:"previousSource"`
	NewValue       any          `json:"newValue"`
}

// SetOverridesResult is the response shape — one entry per requested
// override, regardless of whether the effective value actually
// changed (the user asked; we tell them what they got).
type SetOverridesResult struct {
	Applied map[string]SetOverridesApplied `json:"applied"`
}

// EffectiveConfigChangedParams is the notification shape. Changes
// contains only sections whose effective value actually shifted —
// editors render status-bar / sidebar updates from this map.
type EffectiveConfigChangedParams struct {
	WorkspaceDir string                 `json:"workspaceDir"`
	Changes      map[string]ConfigValue `json:"changes"`
}

// ValidateOverrides walks the requested map and rejects unknown
// sections + type mismatches. Wraps both into one error message so
// editors get a single InvalidParams response with the full list of
// problems. nil / null values pass type validation (they signal
// "clear" and bypass the type check).
func ValidateOverrides(overrides map[string]any) error {
	if len(overrides) == 0 {
		return nil
	}
	known := map[string]bool{}
	for _, s := range KnownSections {
		known[s] = true
	}
	var problems []string
	// Iterate keys in deterministic order so error messages are
	// stable across runs.
	sections := SectionsOf(overrides)
	for _, section := range sections {
		if !known[section] {
			problems = append(problems, fmt.Sprintf("unknown section %q", section))
			continue
		}
		v := overrides[section]
		if v == nil {
			continue // explicit clear, always valid
		}
		if want := typeOfSection(section); !valueMatchesType(v, want) {
			problems = append(problems,
				fmt.Sprintf("section %q expects %s value; got %T", section, want, v))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid overrides: %v", problems)
}

// typeOfSection names the expected type for a known section.
// "string" is the broad type; "enum:a,b,c" narrows to a fixed
// set of allowed string values (only the listed values pass
// valueMatchesType).
func typeOfSection(section string) string {
	switch section {
	case SectionRegistryAddress, SectionRegistryNamespace,
		SectionRegistryToken, SectionRegistryCacheDir:
		return "string"
	case SectionSecurityInlineSecrets:
		return "enum:allow,warn,refuse"
	}
	return "string"
}

// valueMatchesType returns true when raw matches the named type.
// JSON-unmarshaled values arrive as Go types per encoding/json's
// defaults — strings become string, numbers become float64, etc.
//
// Recognizes:
//   - "string": any string value (including empty)
//   - "enum:<v1,v2,...>": string AND equals one of the listed values
func valueMatchesType(raw any, want string) bool {
	switch {
	case want == "string":
		_, ok := raw.(string)
		return ok
	case strings.HasPrefix(want, "enum:"):
		s, ok := raw.(string)
		if !ok {
			return false
		}
		for _, allowed := range strings.Split(want[len("enum:"):], ",") {
			if s == allowed {
				return true
			}
		}
		return false
	}
	return false
}

// ApplyOverridesLocked mutates Server.overrides. Caller MUST
// hold overridesMu. Used by dispatchSetOverrides which takes
// the lock once around the snapshot→apply→snapshot window so a
// concurrent getEffectiveConfig can't see a half-applied state.
func (s *Workspace) ApplyOverridesLocked(overrides map[string]any) {
	if s.cfg.overrides == nil {
		s.cfg.overrides = map[string]any{}
	}
	for section, value := range overrides {
		if value == nil {
			delete(s.cfg.overrides, section)
		} else {
			s.cfg.overrides[section] = value
		}
	}
}

// SnapshotSectionsLocked returns the effective config for the
// given sections, reading the override tier from s.overrides
// directly. Caller MUST hold overridesMu.
//
// Tier sources OTHER than override (settings, file, default)
// take their own mutexes (stateMu) internally; that's safe
// because stateMu and overridesMu are independent locks and
// neither is acquired by the other's holders.
func (s *Workspace) SnapshotSectionsLocked(sections []string) map[string]ConfigValue {
	out := make(map[string]ConfigValue, len(sections))
	for _, section := range sections {
		if v, ok := s.cfg.overrides[section]; ok {
			out[section] = ConfigValue{Value: v, Source: SourceOverride, Origin: "session"}
			continue
		}
		if v, ok := s.resolveFromSettings(section); ok {
			out[section] = ConfigValue{Value: v, Source: SourceSettings, Origin: section}
			continue
		}
		if v, ok := s.resolveFromFile(section); ok {
			out[section] = ConfigValue{Value: v, Source: SourceFile, Origin: section}
			continue
		}
		out[section] = ConfigValue{Value: defaultFor(section), Source: SourceDefault}
	}
	return out
}

// DiffSnapshots returns the subset of sections whose effective
// (value, source) pair changed between two snapshots. Equality is
// reflect-style on Value plus exact Source comparison — a
// same-value override-of-settings IS a change because the source
// shifted, but a no-op repeat-override is not.
func DiffSnapshots(before, after map[string]ConfigValue) map[string]ConfigValue {
	changes := map[string]ConfigValue{}
	for section, post := range after {
		pre := before[section]
		if pre.Source == post.Source && fmt.Sprintf("%v", pre.Value) == fmt.Sprintf("%v", post.Value) {
			continue
		}
		changes[section] = post
	}
	return changes
}

// SectionsOf returns the sorted keys of an overrides map. Sorting
// keeps validation errors + iteration order deterministic.
func SectionsOf(overrides map[string]any) []string {
	out := make([]string, 0, len(overrides))
	for k := range overrides {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NotifyEffectiveConfigChanged fires protolsp/effectiveConfigChanged
// with the given changes map. Same-shape redaction as
// getEffectiveConfig — sensitive sections render <redacted> by
// default. The editor's UI updates from this; no acknowledgment
// expected.
func (s *Workspace) NotifyEffectiveConfigChanged(ctx context.Context, workspaceDir string, changes map[string]ConfigValue) {
	if len(changes) == 0 {
		return
	}
	// Redact BEFORE handing off to the publisher so no Publisher
	// implementation (wire transport or in-process embedder) ever
	// sees unredacted sensitive sections; reveal stays pull-only
	// via getEffectiveConfig's explicit revealSecrets flag.
	s.pub.EffectiveConfigChanged(ctx, workspaceDir, RedactChangeMap(changes))
}

// RedactChangeMap returns a copy of changes with sensitive-named
// section values replaced by redact.Placeholder. Source + Origin
// are preserved so editors still know WHERE the change came from.
// revealSecrets has no meaning for notifications — the editor that
// wants the real value calls getEffectiveConfig with
// revealSecrets=true on demand.
func RedactChangeMap(changes map[string]ConfigValue) map[string]ConfigValue {
	out := make(map[string]ConfigValue, len(changes))
	for section, v := range changes {
		if v.Value != nil && v.Value != "" && redact.IsSensitiveSection(section) {
			v.Value = redact.Placeholder
		}
		out[section] = v
	}
	return out
}
