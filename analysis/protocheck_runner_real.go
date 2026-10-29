//go:build protocheck

// Trendvidia build's protocheck integration: construction and
// config-driven rebuild of the protocheck/v2 data validator behind
// the v2 engine seam (RFC-001 §9.1/§9.4). Expressions are evaluated
// by the GoVM engine from github.com/trendvidia/protocheck-govm —
// the only ExpressionEngine implementation available to protolsp —
// regardless of the config's `engine` selection; a mismatch is the
// editor's information (surfaced via protolsp/engineConfig), not a
// reason to disable validation.
//
// catalog_libraries are honored (§7): loaded through the shared
// protocompile/catalogs reference loader relative to the config
// file, registered on the engine, and used by runProtocheck to
// render violation messages in the client's locale.
//
// The §9.4 knobs deliberately NOT honored here:
//   - default_mode: the LSP always collects every violation; a
//     fail-fast validator would truncate diagnostics.
//   - strict_validation: protolsp never registers function
//     implementations, so strict mode would nil the validator for
//     any workspace whose libraries declare functions. Lenient mode
//     yields CodeFunctionUnimplemented violations instead, which
//     protocheckDataDiagnostics downgrades to Information.
//
// The OSS build compiles protocheck_runner_stub.go in this file's
// place; only the protocheck-tagged files pull in the proprietary
// dependencies.

package analysis

import (
	"context"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/trendvidia/protocompile/catalogs"

	protocheckgovm "github.com/trendvidia/protocheck-govm"
	protocheck "github.com/trendvidia/protocheck/v2"
)

// The tag-free protocheckCatalog interface mirrors protocheck's
// Catalog contract; this pins the two together at compile time.
var _ protocheckCatalog = (*protocheck.MapCatalog)(nil)

// newProtocheckValidator constructs the startup validator:
// descriptor-less, because no workspace root (and therefore no
// effective engine config) exists yet. The initialize-time
// RebuildProtocheckValidator swaps in the config-aware one.
func newProtocheckValidator(log *slog.Logger) (protocheckValidator, func()) {
	return buildProtocheckValidator(log, nil, nil)
}

// sortedLocales returns the catalog map's keys in sorted order so
// option registration is deterministic.
func sortedLocales(cats map[string]*catalogs.Catalog) []string {
	if len(cats) == 0 {
		return nil
	}
	out := make([]string, 0, len(cats))
	for locale := range cats {
		out = append(out, locale)
	}
	sort.Strings(out)
	return out
}

// rebuildProtocheck re-resolves the effective §9.4 config, resolves
// its function_libraries to descriptors, and — when the resulting
// fingerprint differs from what the current validator was built
// from — constructs and swaps in a replacement. Serialized by
// protocheckRebuildMu; the slow work happens off protocheckMu so
// in-flight validations proceed against the old validator until the
// swap.
func (s *Workspace) rebuildProtocheck(_ context.Context) {
	s.protocheckRebuildMu.Lock()
	defer s.protocheckRebuildMu.Unlock()
	cfg := s.EngineConfig()
	fds, key := s.resolveFunctionLibraryDescriptors(cfg)
	cats, catKey := s.loadProtocheckCatalogs(cfg)
	key += catKey
	if key == s.protocheckBaseline {
		return
	}
	v, closeFn := buildProtocheckValidator(s.log, fds, cats)
	s.protocheckBaseline = key
	var ifaceCats map[string]protocheckCatalog
	var catPaths []string
	if len(cats) > 0 {
		ifaceCats = make(map[string]protocheckCatalog, len(cats))
		for locale, cat := range cats {
			ifaceCats[locale] = protocheck.NewMapCatalog(cat.Entries)
			catPaths = append(catPaths, cat.Paths...)
		}
	}
	s.swapProtocheck(v, closeFn, ifaceCats, catPaths)
}

// loadProtocheckCatalogs loads the §7 locale catalogs named by the
// config's catalog_libraries through the shared reference loader
// (protocompile/catalogs) — relative paths resolve against the
// config file's directory, per spec. Load failure degrades to no
// catalogs with a warning (findings fall back to spec messages);
// the loader itself is all-or-nothing per §7, so there is no
// partial state to carry.
//
// The second return folds the loaded CONTENT into the rebuild
// fingerprint — an on-disk catalog edit changes the key even though
// the config didn't, so any rebuild trigger picks it up.
func (s *Workspace) loadProtocheckCatalogs(cfg EngineConfigResult) (map[string]*catalogs.Catalog, string) {
	if len(cfg.CatalogLibraries) == 0 {
		return nil, ""
	}
	baseDir := ""
	if cfg.Path != "" {
		baseDir = filepath.Dir(cfg.Path)
	}
	cats, err := catalogs.Load(baseDir, cfg.CatalogLibraries)
	if err != nil {
		s.log.Warn("protocheck: catalog libraries failed to load; violation messages fall back to spec defaults",
			slog.String("err", err.Error()))
		return nil, "catalogs-error\x00" + err.Error()
	}
	var key strings.Builder
	key.WriteString("catalogs\x00")
	locales := make([]string, 0, len(cats))
	for locale := range cats {
		locales = append(locales, locale)
	}
	sort.Strings(locales)
	for _, locale := range locales {
		cat := cats[locale]
		key.WriteString(locale)
		key.WriteByte('\x00')
		codes := make([]string, 0, len(cat.Entries))
		for code := range cat.Entries {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			key.WriteString(code)
			key.WriteByte('\x00')
			key.WriteString(cat.Entries[code])
			key.WriteByte('\x00')
		}
	}
	return cats, key.String()
}

// resolveFunctionLibraryDescriptors maps the config's
// function_libraries import paths to linked descriptors via the
// schema lookups (no compile step — the lookup serves already-linked
// files). Unresolvable paths are logged and skipped: a typo or a
// not-yet-dialed registry must degrade to "library's functions
// unknown", never disable validation.
//
// The second return is the rebuild fingerprint: config provenance +
// configured paths + the subset that actually resolved. Including
// the resolved subset means a lookup swap that flips a miss into a
// hit changes the key (rebuild proceeds), while lookup churn with no
// libraries configured diff-skips.
func (s *Workspace) resolveFunctionLibraryDescriptors(cfg EngineConfigResult) ([]protoreflect.FileDescriptor, string) {
	var key strings.Builder
	for _, part := range []string{cfg.Path, cfg.Source, cfg.Error} {
		key.WriteString(part)
		key.WriteByte('\x00')
	}
	var fds []protoreflect.FileDescriptor
	for _, path := range cfg.FunctionLibraries {
		key.WriteString(path)
		key.WriteByte('\x00')
		fd := s.lookupFunctionLibrary(path)
		if fd == nil {
			s.log.Warn("protocheck: function library not resolvable; its declared functions stay unknown",
				slog.String("path", path))
			continue
		}
		fds = append(fds, fd)
		key.WriteString("+resolved\x00")
	}
	return fds, key.String()
}

// lookupFunctionLibrary resolves one library import path through the
// root workspace lookup, falling back to the internally-shipped
// descriptors (same order as go-to-definition's path resolution).
func (s *Workspace) lookupFunctionLibrary(path string) protoreflect.FileDescriptor {
	if lk := s.lookupMgr.Snapshot(); lk != nil {
		if fd, _, err := lk.FindFileByPathWithOrigin(path); err == nil && fd != nil {
			return fd
		}
	}
	if s.internalLookup != nil {
		if fd, _, err := s.internalLookup.FindFileByPathWithOrigin(path); err == nil && fd != nil {
			return fd
		}
	}
	return nil
}

// buildProtocheckValidator constructs a GoVM-backed protocheck/v2
// validator, registering the given library descriptors for §9.2
// init-time function verification (lenient: declared-but-
// unimplemented functions become CodeFunctionUnimplemented
// violations at call time). Failure degrades stepwise — first retry
// without descriptors (a broken library must never disable
// validation), then log and return (nil, nil) so the data pass
// short-circuits. The returned closer releases the validator's
// pooled VMs (Validator.Close closes the engine too).
func buildProtocheckValidator(log *slog.Logger, fds []protoreflect.FileDescriptor, cats map[string]*catalogs.Catalog) (protocheckValidator, func()) {
	engine, err := protocheckgovm.New()
	if err != nil {
		log.Warn("protocheck govm engine construction failed; data validation disabled",
			slog.String("err", err.Error()))
		return nil, nil
	}
	opts := []protocheck.Option{protocheck.WithExpressionEngine(engine)}
	if len(fds) > 0 {
		opts = append(opts, protocheck.WithDescriptors(fds...))
	}
	// Register the loaded catalogs with the engine too (§7 says
	// catalogs register "alongside function impls"). protolsp's own
	// rendering happens in runProtocheck — the ValidationError path
	// has no Report.Format — but a seam consumer handed this
	// validator gets spec-correct behavior.
	for _, locale := range sortedLocales(cats) {
		opts = append(opts, protocheck.WithCatalog(locale, protocheck.NewMapCatalog(cats[locale].Entries)))
	}
	v, err := protocheck.New(opts...)
	if err != nil && len(fds) > 0 {
		log.Warn("protocheck validator construction failed with function_libraries descriptors; retrying without",
			slog.String("err", err.Error()))
		v, err = protocheck.New(protocheck.WithExpressionEngine(engine))
	}
	if err != nil {
		log.Warn("protocheck validator construction failed; data validation disabled",
			slog.String("err", err.Error()))
		_ = engine.Close()
		return nil, nil
	}
	return v, func() { _ = v.Close() }
}
