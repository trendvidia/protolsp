// Lifecycle plumbing for the protocheck data validator. The
// validator used to be built exactly once at New; wiring the §9.4
// engine config's function_libraries into construction (#260) made
// it rebuildable — the effective config isn't known until the
// workspace root lands, and both the config and the schema lookup
// serving its library import paths can change over the session.
//
// This file is tag-free: the swap/read primitives compile in both
// builds, while the actual (re)construction lives behind the
// `protocheck` build tag (protocheck_runner_real.go) with a no-op
// stub (protocheck_runner_stub.go).

package analysis

import (
	"context"
	"strings"

	"google.golang.org/protobuf/proto"
)

// protocheckCatalog mirrors protocheck/v2's Catalog interface (§7)
// so this tag-free file can hold and render locale catalogs without
// the tagged-only import; *protocheck.MapCatalog satisfies it by
// construction, and the tagged test suite pins the two together.
type protocheckCatalog interface {
	Render(code string, params map[string]any) (message string, ok bool)
}

// RebuildProtocheckValidator re-resolves the effective engine config
// and swaps in a validator built against it. Cheap when the config
// fingerprint is unchanged; no-op in the OSS build. Called from
// initialize, the config-edit cascade, protowire.config.textproto
// watched-file events, and lookup swaps.
func (s *Workspace) RebuildProtocheckValidator(ctx context.Context) {
	s.rebuildProtocheck(ctx)
}

// protocheckValidate runs the current validator against msg while
// holding the read lock, so a concurrent swap cannot close the
// validator under an in-flight evaluation. The bool reports whether
// a validator was installed at all.
func (s *Workspace) protocheckValidate(msg proto.Message) (error, bool) {
	s.protocheckMu.RLock()
	defer s.protocheckMu.RUnlock()
	if s.protocheck == nil {
		return nil, false
	}
	return s.protocheck.Validate(msg), true
}

// swapProtocheck installs a new validator (+ closer) and the §7
// locale catalogs it was built with, closing the displaced
// validator after releasing the lock. Safe: readers hold the RLock
// for the full Validate call, so once the write lock is acquired no
// in-flight evaluation references the old validator.
func (s *Workspace) swapProtocheck(v protocheckValidator, closeFn func(), catalogs map[string]protocheckCatalog, catalogPaths []string) {
	s.protocheckMu.Lock()
	old := s.protocheckClose
	s.protocheck = v
	s.protocheckClose = closeFn
	s.protocheckCatalogs = catalogs
	s.protocheckCatalogPaths = catalogPaths
	s.protocheckMu.Unlock()
	if old != nil {
		old()
	}
}

// closeProtocheck releases the current validator. Part of
// Workspace.Close teardown; idempotent.
func (s *Workspace) closeProtocheck() {
	s.swapProtocheck(nil, nil, nil, nil)
}

// SetClientLocale records the editor's UI locale (BCP 47, from
// InitializeParams.locale). Call before the first document lands —
// the render path reads the field without a lock, same discipline
// as SetPublisher.
func (s *Workspace) SetClientLocale(locale string) { s.clientLocale = locale }

// protocheckRenderCatalog picks the loaded catalog matching the
// client's locale, negotiating per BCP 47 semantics: case-
// insensitive exact match first, then the request's primary
// language subtag ("de-DE" → "de"), then any catalog sharing that
// primary subtag (smallest tag wins, for determinism). ok=false
// with no catalogs, no client locale, or no match — findings then
// render their spec fallback messages, the §7 miss behavior.
func (s *Workspace) protocheckRenderCatalog() (protocheckCatalog, bool) {
	s.protocheckMu.RLock()
	defer s.protocheckMu.RUnlock()
	return matchLocaleCatalog(s.protocheckCatalogs, s.clientLocale)
}

// protocheckCatalogPathsSnapshot returns the absolute paths of the
// catalog files the current validator's catalogs merged from. The
// watched-files classifier uses it to spot on-disk catalog edits.
func (s *Workspace) protocheckCatalogPathsSnapshot() []string {
	s.protocheckMu.RLock()
	defer s.protocheckMu.RUnlock()
	return s.protocheckCatalogPaths
}

func matchLocaleCatalog(catalogs map[string]protocheckCatalog, locale string) (protocheckCatalog, bool) {
	if len(catalogs) == 0 || locale == "" {
		return nil, false
	}
	want := strings.ToLower(locale)
	base, _, _ := strings.Cut(want, "-")
	var baseHit protocheckCatalog
	var baseHitTag string
	for tag, cat := range catalogs {
		lower := strings.ToLower(tag)
		if lower == want {
			return cat, true
		}
		if lower == base {
			// Primary-subtag match beats shared-primary matches;
			// remember it but keep scanning for an exact hit.
			baseHit, baseHitTag = cat, ""
			continue
		}
		if tagBase, _, _ := strings.Cut(lower, "-"); tagBase == base {
			if baseHitTag == "" && baseHit != nil {
				continue // an exact-base hit already won
			}
			if baseHit == nil || lower < baseHitTag {
				baseHit, baseHitTag = cat, lower
			}
		}
	}
	return baseHit, baseHit != nil
}
