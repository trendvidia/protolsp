// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Package chain implements protolsp's edit-time view of a chameleon
// chain spec (C1 of the chameleon-integration design).
//
// Scope is intentionally narrow: parse a chain.pxf, build the
// environment ancestry, and expand the declared layers into a flat
// LayerInstance list the LSP can match files against. NOTHING here
// runs the chameleon resolver — no sops, no env-var reads, no
// network. The package is a lightweight reimplementation of the
// shape chameleon needs at edit time, decoupled from the runtime
// module so protolsp's release cadence isn't pinned to chameleon's.
//
// See docs/design/chameleon-integration.md §"Architecture →
// Where the integration code lives" for the rationale.
package chain

import (
	"errors"
	"fmt"
	"strings"

	"github.com/trendvidia/protolsp/safeio"
	"github.com/trendvidia/protowire-go/encoding/pxf"
)

// ExpectedType is the @type directive every chain.pxf must carry.
// Mismatches surface as a Diagnostic; load still succeeds so
// chainStatus can show the user what went wrong.
const ExpectedType = "chameleon.v1.ChainSpec"

// ActiveChain is the loaded + expanded view of a chain spec. Built
// once by Load when the workspace initializes (or by tests via the
// exported Expand helper); held by the server for the session.
//
// Errors collects everything from parse + expansion that the LSP
// should surface to the editor — missing parents, ancestry cycles,
// missing @type. Returning a partial ActiveChain even on errors
// matches the design's "configured but broken" affordance in
// chainStatus.
type ActiveChain struct {
	// Path is the absolute filesystem path the spec was loaded
	// from. Empty for in-memory ActiveChain values produced by
	// tests via Expand directly.
	Path string

	// Spec is the parsed-but-untransformed view of the chain.pxf
	// body. Exposed so future phases (C3+) can read fields the
	// expansion ignores, like context_keys.
	Spec *Spec

	// Environments maps each declared environment to its
	// root-first ancestry. Built by Expand.
	Environments map[string]EnvironmentInfo

	// Sources indexes the chain spec's named sources by name.
	// Empty when the spec has no `sources` block (sources are
	// optional in the runtime — undeclared names default to a
	// well-known `default` fs source).
	Sources map[string]SourceRef

	// Layers is the flat list of layer instances after expansion.
	// PER_ENV layers materialize as one entry per environment;
	// ONCE layers as a single entry with Environment="".
	Layers []LayerInstance

	// Errors collects load + expansion issues. Non-fatal: a
	// chain with errors still carries whatever expanded
	// successfully.
	Errors []Diagnostic
}

// Spec is the parsed-but-untransformed view of a chain.pxf.
type Spec struct {
	Version            string
	DefaultEnvironment string
	Environments       []Environment
	Layers             []LayerEntry
	Sources            []SourceRef
}

// SourceRef mirrors chameleon.v1.SourceRef from the spec: a named
// physical backing for one or more layer entries. Kind values
// recognized today by protolsp's edit-time integration:
//
//   - "fs": file system. The default if a layer references an
//     undeclared source name; Options["root"] is the base dir.
//   - "env": process environment variables. No file on disk;
//     Options["prefix"] is prepended to UPPER_SNAKE field paths
//     when computing env var names.
//
// Other kinds parse fine but are treated as opaque — protolsp's
// chain-aware features will only react to "env" specially. The
// runtime may grow more kinds; protolsp adds support as needed.
type SourceRef struct {
	Name    string
	Kind    string
	Options map[string]string
}

// Environment is one declared environment from the chain spec's
// `environments` list. Single-parent ancestry only; Extends is
// the empty string for root environments.
type Environment struct {
	Name    string
	Extends string
	// KeyName names the entry in a sops PXF AgeKeyFile whose age
	// recipient is used when encrypting layers for this
	// environment. Empty means "inherit from parent / fall back to
	// the file's default recipient" — see chameleon v0.22.1.
	// Resolved by the sops library's RecipientFromKeyFileByName
	// against SOPS_AGE_KEY_FILE; protolsp surfaces it through the
	// chain status so editors can render the per-env recipient
	// without each one re-parsing the key file.
	KeyName string
}

// LayerEntry is one declared layer from the chain spec's `layers`
// list, untransformed. Expansion produces zero or more
// LayerInstance values per entry.
type LayerEntry struct {
	ID        string
	Path      string
	ForEach   string // "ONCE" | "PER_ENV" | "" (treated as ONCE)
	Required  bool
	Encrypted bool
	Source    string
	// PathPos is the position of the `path = "…"` assignment within
	// the chain spec. Diagnostics about a specific layer's path
	// anchor here so editors point the squiggle at the right line.
	// Zero when the entry was constructed without parsing (tests).
	PathPos pxf.Position
}

// EnvironmentInfo is the resolved ancestry for an environment.
// Root-first: "prod" → ["prod"], "dev" extending "sandbox"
// extending "prod" → ["prod", "sandbox", "dev"].
type EnvironmentInfo struct {
	Name      string
	Ancestors []string
}

// LayerInstance is one materialized layer after expansion. The LSP
// matches open documents against LayerInstance.ResolvedPath to
// decide whether a file is a chain layer (C2).
//
// For ONCE-strategy layers, Environment is "". For PER_ENV, it's
// the environment whose name substituted into ${env}.
type LayerInstance struct {
	ID           string
	Source       string
	PathTemplate string
	ResolvedPath string
	Environment  string
	Strategy     string
	Encrypted    bool
}

// Diagnostic is a non-fatal load- or expansion-time issue. Line and
// Column are zero when the issue isn't tied to a specific token
// (e.g. an ancestry cycle).
type Diagnostic struct {
	Message string
	Line    int
	Column  int
}

// Load reads, parses, and expands a chain.pxf from disk. Returns
// the populated ActiveChain (with any non-fatal issues in Errors)
// or an error if the file can't be read or parsed as PXF at all.
//
// Convenience wrapper around [ParseAt] that does the file read.
// Callers with the content already in memory (e.g. the LSP's
// open-document store) should call ParseAt directly.
func Load(path string) (*ActiveChain, error) {
	content, err := safeio.ReadFileLimit(path, safeio.MaxConfigFileBytes)
	if err != nil {
		return nil, fmt.Errorf("reading chain spec at %s: %w", path, err)
	}
	return ParseAt(path, content)
}

// ParseAt parses chain spec bytes into an ActiveChain, anchoring
// it at `path` for resolve-order calculations + Path metadata.
// Used by live-reload paths that read chain.pxf out of the LSP's
// open-document store rather than disk — so unsaved edits to the
// spec drive chain-aware features in other docs immediately.
//
// Returns an error only for unparseable PXF; semantic issues
// (missing parent, ancestry cycles, missing @type, ...) populate
// the returned ActiveChain.Errors and the chain still loads with
// whatever expanded successfully.
func ParseAt(path string, content []byte) (*ActiveChain, error) {
	spec, diags, err := parse(content)
	if err != nil {
		return nil, fmt.Errorf("parsing chain spec at %s: %w", path, err)
	}
	envs, layers, expandDiags := Expand(spec)
	sources := make(map[string]SourceRef, len(spec.Sources))
	for _, src := range spec.Sources {
		sources[src.Name] = src
	}
	return &ActiveChain{
		Path:         path,
		Spec:         spec,
		Environments: envs,
		Layers:       layers,
		Sources:      sources,
		Errors:       append(diags, expandDiags...),
	}, nil
}

// parse walks the chain.pxf AST into a Spec. Returns an error only
// for unparseable PXF; semantic issues (missing @type, bad field
// shapes) surface as Diagnostics so partial specs still load.
func parse(content []byte) (*Spec, []Diagnostic, error) {
	doc, err := pxf.Parse(content)
	if err != nil {
		return nil, nil, err
	}
	if doc == nil {
		return nil, nil, errors.New("pxf.Parse returned nil document")
	}
	spec := &Spec{}
	var diags []Diagnostic
	if doc.TypeURL == "" {
		diags = append(diags, Diagnostic{
			Message: "missing @type directive; expected `@type " + ExpectedType + "`",
		})
	} else if doc.TypeURL != ExpectedType {
		diags = append(diags, Diagnostic{
			Message: fmt.Sprintf("unexpected @type %q; expected %q", doc.TypeURL, ExpectedType),
		})
	}
	for _, entry := range doc.Entries {
		assign, ok := entry.(*pxf.Assignment)
		if !ok {
			continue
		}
		switch assign.Key {
		case "version":
			if s, ok := stringValue(assign.Value); ok {
				spec.Version = s
			}
		case "default_environment":
			if s, ok := stringValue(assign.Value); ok {
				spec.DefaultEnvironment = s
			}
		case "environments":
			lv, ok := assign.Value.(*pxf.ListVal)
			if !ok {
				diags = append(diags, Diagnostic{
					Line:    assign.Pos.Line,
					Column:  assign.Pos.Column,
					Message: "environments must be a list of {name=..., extends=...} blocks",
				})
				continue
			}
			spec.Environments = parseEnvironments(lv)
		case "layers":
			lv, ok := assign.Value.(*pxf.ListVal)
			if !ok {
				diags = append(diags, Diagnostic{
					Line:    assign.Pos.Line,
					Column:  assign.Pos.Column,
					Message: "layers must be a list of layer blocks",
				})
				continue
			}
			spec.Layers = parseLayers(lv)
		case "sources":
			bv, ok := assign.Value.(*pxf.BlockVal)
			if !ok {
				diags = append(diags, Diagnostic{
					Line:    assign.Pos.Line,
					Column:  assign.Pos.Column,
					Message: "sources must be a map { \"name\": { kind = ..., options = ... } }",
				})
				continue
			}
			spec.Sources = parseSources(bv)
		}
		// context_keys is parsed at the runtime layer but ignored
		// here — protolsp's edit-time features don't currently
		// substitute ${ctx.X} (treated as opaque context interp).
	}
	return spec, diags, nil
}

func parseEnvironments(lv *pxf.ListVal) []Environment {
	var out []Environment
	for _, elem := range lv.Elements {
		bv, ok := elem.(*pxf.BlockVal)
		if !ok {
			continue
		}
		var env Environment
		for _, entry := range bv.Entries {
			assign, ok := entry.(*pxf.Assignment)
			if !ok {
				continue
			}
			s, ok := stringValue(assign.Value)
			if !ok {
				continue
			}
			switch assign.Key {
			case "name":
				env.Name = s
			case "extends":
				env.Extends = s
			case "key_name":
				env.KeyName = s
			}
		}
		if env.Name != "" {
			out = append(out, env)
		}
	}
	return out
}

func parseLayers(lv *pxf.ListVal) []LayerEntry {
	var out []LayerEntry
	for _, elem := range lv.Elements {
		bv, ok := elem.(*pxf.BlockVal)
		if !ok {
			continue
		}
		var layer LayerEntry
		for _, entry := range bv.Entries {
			assign, ok := entry.(*pxf.Assignment)
			if !ok {
				continue
			}
			switch assign.Key {
			case "id":
				if s, ok := stringValue(assign.Value); ok {
					layer.ID = s
				}
			case "path":
				if s, ok := stringValue(assign.Value); ok {
					layer.Path = s
					layer.PathPos = assign.Pos
				}
			case "for_each":
				// Accept either an unquoted identifier (the
				// canonical form, e.g. `for_each = PER_ENV`) or a
				// quoted string for forgiving authoring.
				if ident, ok := assign.Value.(*pxf.IdentVal); ok {
					layer.ForEach = ident.Name
				} else if s, ok := stringValue(assign.Value); ok {
					layer.ForEach = s
				}
			case "required":
				if b, ok := assign.Value.(*pxf.BoolVal); ok {
					layer.Required = b.Value
				}
			case "encrypted":
				if b, ok := assign.Value.(*pxf.BoolVal); ok {
					layer.Encrypted = b.Value
				}
			case "source":
				if s, ok := stringValue(assign.Value); ok {
					layer.Source = s
				}
			}
		}
		if layer.Path != "" {
			out = append(out, layer)
		}
	}
	return out
}

// parseSources walks the sources-map's BlockVal entries, each of
// which is a MapEntry (`"name": { ... }` syntax) whose value is a
// BlockVal with `kind` + `options` assignments. Unknown sub-keys
// are ignored — keeps protolsp future-proof against runtime
// additions.
func parseSources(bv *pxf.BlockVal) []SourceRef {
	var out []SourceRef
	for _, entry := range bv.Entries {
		me, ok := entry.(*pxf.MapEntry)
		if !ok {
			continue
		}
		body, ok := me.Value.(*pxf.BlockVal)
		if !ok {
			continue
		}
		src := SourceRef{Name: me.Key}
		for _, sub := range body.Entries {
			assign, ok := sub.(*pxf.Assignment)
			if !ok {
				continue
			}
			switch assign.Key {
			case "kind":
				if s, ok := stringValue(assign.Value); ok {
					src.Kind = s
				}
			case "options":
				if obv, ok := assign.Value.(*pxf.BlockVal); ok {
					src.Options = parseStringMap(obv)
				}
			}
		}
		if src.Name != "" {
			out = append(out, src)
		}
	}
	return out
}

// parseStringMap unwraps a BlockVal of `"key": "value"` MapEntries
// into a plain map[string]string. Non-string values are skipped
// silently — runtime is responsible for stricter validation; we
// surface whatever we can parse and defer the rest.
func parseStringMap(bv *pxf.BlockVal) map[string]string {
	out := make(map[string]string)
	for _, entry := range bv.Entries {
		me, ok := entry.(*pxf.MapEntry)
		if !ok {
			continue
		}
		if sv, ok := stringValue(me.Value); ok {
			out[me.Key] = sv
		}
	}
	return out
}

// LayerIsEnvSourced reports whether the given layer references a
// source declared as `kind = "env"`. Returns false when the chain
// has no sources block, the layer has no source, or the named
// source isn't env-kind. C7 of chameleon-integration uses this to
// decide where env-source affordances apply (hover hint, required-
// field suppression, chain-resolution placeholder).
func (ac *ActiveChain) LayerIsEnvSourced(layer LayerInstance) bool {
	if ac == nil || layer.Source == "" {
		return false
	}
	src, ok := ac.Sources[layer.Source]
	return ok && src.Kind == "env"
}

// EnvVarNameForPath returns the env var name an env source would
// consult for a dotted field path. Mirrors chameleon's
// source/env.go envNameFor exactly:
//
//	envName = prefix + UPPER_SNAKE(path)
//
// Where UPPER_SNAKE replaces `.` with `_` and uppercases. Empty
// prefix (the default) yields the bare uppercase form.
//
// Example: prefix="MYAPP_", path="db.host" → "MYAPP_DB_HOST".
func EnvVarNameForPath(prefix, path string) string {
	return prefix + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
}

// Expand computes ancestry and the materialized layer list for a
// parsed Spec. Exported so tests can drive expansion without going
// through the filesystem.
//
// PER_ENV layers materialize once per declared environment (the
// runtime later picks the subset of ancestors at resolve time;
// pre-computing per env is what the LSP needs for layer-detection).
// ONCE layers materialize as a single instance with Environment="" —
// EXCEPT when the path template contains `${env}`, in which case we
// emit one instance per environment with Environment="" still
// (matching ONCE semantics: no env binding), because the LSP needs
// every concrete path the runtime could load to be a candidate for
// IsLayer matching.
//
// Each ResolvedPath is prefixed with the source's `root` option when
// the layer references an fs-kind source declared in `spec.Sources`.
// Without this, a layer like `{source = "secrets", path =
// "${env}.pxf"}` resolves to `dev.pxf` instead of `layers/secrets/dev.pxf`
// and the open-file matcher misses every file under custom roots.
//
// Layer entries that omit `source =` bind to the declared "default"
// source when one exists (effectiveSourceName), mirroring the
// runtime's implicit-default rule; the emitted LayerInstance.Source
// carries the resolved name so downstream consumers (LayerInfo,
// LayerIsEnvSourced) see the binding the runtime would use.
func Expand(spec *Spec) (map[string]EnvironmentInfo, []LayerInstance, []Diagnostic) {
	if spec == nil {
		return nil, nil, nil
	}
	envByName := make(map[string]Environment, len(spec.Environments))
	for _, e := range spec.Environments {
		envByName[e.Name] = e
	}
	sources := make(map[string]SourceRef, len(spec.Sources))
	for _, s := range spec.Sources {
		sources[s.Name] = s
	}
	var diags []Diagnostic
	envs := make(map[string]EnvironmentInfo, len(spec.Environments))
	for _, e := range spec.Environments {
		ancestors, derr := buildAncestry(e.Name, envByName)
		if derr != "" {
			diags = append(diags, Diagnostic{Message: derr})
		}
		envs[e.Name] = EnvironmentInfo{Name: e.Name, Ancestors: ancestors}
	}
	var layers []LayerInstance
	for _, le := range spec.Layers {
		strategy := strings.ToUpper(le.ForEach)
		if strategy == "" {
			strategy = "ONCE"
		}
		srcName := effectiveSourceName(sources, le.Source)
		switch strategy {
		case "PER_ENV":
			for _, e := range spec.Environments {
				resolved := applySourceRoot(sources, srcName,
					strings.ReplaceAll(le.Path, "${env}", e.Name))
				layers = append(layers, LayerInstance{
					ID:           le.ID,
					Source:       srcName,
					PathTemplate: le.Path,
					ResolvedPath: resolved,
					Environment:  e.Name,
					Strategy:     strategy,
					Encrypted:    le.Encrypted,
				})
			}
		default: // ONCE and anything we don't recognize.
			if strings.Contains(le.Path, "${env}") {
				// Materialize one instance per env so the LSP has
				// every concrete candidate for IsLayer matching, even
				// though ResolveOrder will only emit the leaf-env one
				// at resolve time.
				for _, e := range spec.Environments {
					resolved := applySourceRoot(sources, srcName,
						strings.ReplaceAll(le.Path, "${env}", e.Name))
					layers = append(layers, LayerInstance{
						ID:           le.ID,
						Source:       srcName,
						PathTemplate: le.Path,
						ResolvedPath: resolved,
						Environment:  "",
						Strategy:     strategy,
						Encrypted:    le.Encrypted,
					})
				}
			} else {
				layers = append(layers, LayerInstance{
					ID:           le.ID,
					Source:       srcName,
					PathTemplate: le.Path,
					ResolvedPath: applySourceRoot(sources, srcName, le.Path),
					Environment:  "",
					Strategy:     strategy,
					Encrypted:    le.Encrypted,
				})
			}
		}
	}
	return envs, layers, diags
}

// effectiveSourceName resolves the source name a layer entry binds
// to. A declared name wins. An empty `source =` mirrors the
// runtime's implicit binding: layers without a source resolve
// against the well-known `default` source WHEN one is declared —
// pxfed's chameleon template ships exactly this shape (a single
// "default" fs source + layer entries that omit `source =`).
// Without the mapping, Expand left those ResolvedPaths bare and
// every chain-aware feature silently degraded to the permissive
// fs-root fallback (#207). No declared "default" keeps "" so the
// bare-path behavior survives for spec-less / sourceless chains.
func effectiveSourceName(sources map[string]SourceRef, declared string) string {
	if declared != "" {
		return declared
	}
	if _, ok := sources["default"]; ok {
		return "default"
	}
	return ""
}

// applySourceRoot prepends the fs-source root to a layer's path when
// the named source is declared with kind="fs" and a non-empty
// `root` option. Returns the path unchanged when the source isn't
// declared, isn't fs-kind, or has no root. Callers resolve the
// implicit-default binding via effectiveSourceName first;
// sourceName == "" here means "no source at all" (bare path).
// Paths are normalized to drop a leading `./` so the result joins
// cleanly with the workspace dir.
func applySourceRoot(sources map[string]SourceRef, sourceName, path string) string {
	if sourceName == "" {
		return path
	}
	src, ok := sources[sourceName]
	if !ok || src.Kind != "fs" {
		return path
	}
	root := src.Options["root"]
	if root == "" {
		return path
	}
	root = strings.TrimPrefix(root, "./")
	return strings.TrimSuffix(root, "/") + "/" + path
}

// ResolveOrder returns the layer-instance load order that the
// chameleon runtime would produce for a single environment X. C3 of
// chameleon-integration uses this for required-field weighting:
// walking these instances + reading their contents tells us whether
// some chain layer sets a required field, which gates the leaf-file
// diagnostic.
//
// Ordering mirrors chameleon's runtime semantics:
//   - PER_ENV layer entry → one LayerInstance per ancestor in
//     root-first order ([prod, sandbox, dev] for env=dev).
//   - ONCE layer entry → a single instance per entry, with any
//     "${env}" template substituted with the leaf env name. (At
//     runtime ONCE layers are loaded once per resolve call, with
//     the caller's env interpolated.)
//
// Unknown env yields nil. Spec-less ActiveChain yields nil. The
// returned slice references the spec; callers should treat it as
// read-only.
func (ac *ActiveChain) ResolveOrder(env string) []LayerInstance {
	if ac == nil || ac.Spec == nil {
		return nil
	}
	info, ok := ac.Environments[env]
	if !ok {
		return nil
	}
	var out []LayerInstance
	for _, le := range ac.Spec.Layers {
		strategy := strings.ToUpper(le.ForEach)
		if strategy == "" {
			strategy = "ONCE"
		}
		srcName := effectiveSourceName(ac.Sources, le.Source)
		switch strategy {
		case "PER_ENV":
			for _, anc := range info.Ancestors {
				resolved := applySourceRoot(ac.Sources, srcName,
					strings.ReplaceAll(le.Path, "${env}", anc))
				out = append(out, LayerInstance{
					ID:           le.ID,
					Source:       srcName,
					PathTemplate: le.Path,
					ResolvedPath: resolved,
					Environment:  anc,
					Strategy:     strategy,
					Encrypted:    le.Encrypted,
				})
			}
		default: // ONCE — single instance, "${env}" substituted with leaf env.
			resolved := applySourceRoot(ac.Sources, srcName,
				strings.ReplaceAll(le.Path, "${env}", env))
			out = append(out, LayerInstance{
				ID:           le.ID,
				Source:       srcName,
				PathTemplate: le.Path,
				ResolvedPath: resolved,
				Environment:  "",
				Strategy:     strategy,
				Encrypted:    le.Encrypted,
			})
		}
	}
	return out
}

// buildAncestry walks the extends chain from `name` to the root,
// returning the ancestry root-first plus an optional error string
// describing the first problem (cycle or missing parent). On error
// the partial path collected so far is still returned — the LSP
// uses it for status display.
func buildAncestry(name string, envs map[string]Environment) ([]string, string) {
	var path []string
	seen := map[string]bool{}
	cur := name
	for cur != "" {
		if seen[cur] {
			return path, fmt.Sprintf("environment %q has an ancestry cycle (revisits %q)", name, cur)
		}
		seen[cur] = true
		e, ok := envs[cur]
		if !ok {
			return path, fmt.Sprintf("environment %q references unknown parent %q", name, cur)
		}
		path = append([]string{cur}, path...)
		cur = e.Extends
	}
	return path, ""
}

// stringValue unwraps a pxf.StringVal. Local copy of the helper
// rather than importing the workspaceconfig one — chain mustn't
// depend on workspaceconfig (and vice versa) so the two packages
// remain decoupled.
func stringValue(v pxf.Value) (string, bool) {
	if sv, ok := v.(*pxf.StringVal); ok {
		return sv.Value, true
	}
	return "", false
}
