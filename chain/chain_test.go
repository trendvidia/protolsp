// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package chain

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const simpleChainSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [
  { name = "prod" }
  { name = "sandbox" extends = "prod" }
  { name = "dev"     extends = "sandbox" }
]

layers = [
  { id = "base" path = "base.pxf"        for_each = ONCE    required = true }
  { id = "env"  path = "env/${env}.pxf"  for_each = PER_ENV }
  { id = "sec"  path = "${env}.sops.pxf" for_each = ONCE    encrypted = true source = "secrets" }
]
`

func TestLoad_SimpleChain(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(simpleChainSpec), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)
	require.NotNil(t, ac)
	assert.Empty(t, ac.Errors, "well-formed chain spec yields no diagnostics")
	assert.Equal(t, specPath, ac.Path)
	require.NotNil(t, ac.Spec)
	assert.Equal(t, "1.0.0", ac.Spec.Version)
	assert.Equal(t, "dev", ac.Spec.DefaultEnvironment)
}

func TestLoad_AncestryIsRootFirst(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(simpleChainSpec), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)

	prod := ac.Environments["prod"]
	assert.Equal(t, []string{"prod"}, prod.Ancestors,
		"root environment has only itself in the ancestry")

	sandbox := ac.Environments["sandbox"]
	assert.Equal(t, []string{"prod", "sandbox"}, sandbox.Ancestors,
		"one-level extension lists parent then self, root-first")

	dev := ac.Environments["dev"]
	assert.Equal(t, []string{"prod", "sandbox", "dev"}, dev.Ancestors,
		"deeper extension preserves root-first ordering")
}

func TestLoad_PerEnvLayerExpandsAcrossAllEnvironments(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(simpleChainSpec), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)

	var envLayers []LayerInstance
	for _, l := range ac.Layers {
		if l.ID == "env" {
			envLayers = append(envLayers, l)
		}
	}
	require.Len(t, envLayers, 3,
		"PER_ENV layer materializes once per declared environment")
	gotEnvs := map[string]string{}
	for _, l := range envLayers {
		assert.Equal(t, "PER_ENV", l.Strategy)
		assert.Equal(t, "env/${env}.pxf", l.PathTemplate)
		gotEnvs[l.Environment] = l.ResolvedPath
	}
	assert.Equal(t, "env/prod.pxf", gotEnvs["prod"])
	assert.Equal(t, "env/sandbox.pxf", gotEnvs["sandbox"])
	assert.Equal(t, "env/dev.pxf", gotEnvs["dev"])
}

func TestLoad_OnceLayerIsSingleInstance(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(simpleChainSpec), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)

	var baseLayers []LayerInstance
	for _, l := range ac.Layers {
		if l.ID == "base" {
			baseLayers = append(baseLayers, l)
		}
	}
	require.Len(t, baseLayers, 1, "ONCE layer materializes as a single instance")
	assert.Equal(t, "base.pxf", baseLayers[0].ResolvedPath)
	assert.Empty(t, baseLayers[0].Environment,
		"ONCE layer has no environment binding")
	assert.Equal(t, "ONCE", baseLayers[0].Strategy)
}

// TestExpand_FsSourceRootPrependedToPath covers the customer
// workspace shape that broke go-to-def for layers under a non-default
// fs source: chain declares `sources.secrets = { kind="fs", options
// = { root = "./layers/secrets" } }` and a layer that references it.
// ResolvedPath must include the source root so the file matcher in
// computeLayerInfo can hit the on-disk file's workspace-relative
// path.
func TestExpand_FsSourceRootPrependedToPath(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	src := `@type chameleon.v1.ChainSpec
version = "1.0.0"
default_environment = "dev"
environments = [ { name = "prod" } { name = "dev" extends = "prod" } ]
sources = {
  "secrets": { kind = "fs"  options = { "root": "./layers/secrets" } }
}
layers = [
  { id = "secrets-leaf" path = "${env}.pxf" for_each = ONCE source = "secrets" }
]
`
	require.NoError(t, os.WriteFile(specPath, []byte(src), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)

	// Expand emits one instance per env for a ONCE+${env} layer so
	// the LSP can match files for any env without knowing which env
	// the user is currently editing.
	var resolved []string
	for _, l := range ac.Layers {
		if l.ID == "secrets-leaf" {
			resolved = append(resolved, l.ResolvedPath)
		}
	}
	assert.ElementsMatch(t,
		[]string{"layers/secrets/prod.pxf", "layers/secrets/dev.pxf"},
		resolved,
		"fs source root must be prepended so the file matcher hits the right relative path")

	// ResolveOrder still emits one instance per call, substituting
	// the leaf env. The prepended root must apply here too so file
	// reads in chain_required.go etc. find the right files.
	order := ac.ResolveOrder("dev")
	require.Len(t, order, 1)
	assert.Equal(t, "layers/secrets/dev.pxf", order[0].ResolvedPath)
}

func TestLoad_EncryptedFlagPreserved(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(simpleChainSpec), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)

	var found bool
	for _, l := range ac.Layers {
		if l.ID == "sec" {
			assert.True(t, l.Encrypted, "encrypted=true flag is preserved")
			assert.Equal(t, "secrets", l.Source)
			found = true
		}
	}
	assert.True(t, found, "encrypted layer is present in expansion")
}

func TestLoad_MissingFileReturnsError(t *testing.T) {
	_, err := Load("/nonexistent/path/chain.pxf")
	require.Error(t, err)
}

func TestLoad_UnparseablePxfReturnsError(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte("@type x { unclosed"), 0o600))

	_, err := Load(specPath)
	require.Error(t, err)
}

func TestLoad_MissingTypeIsDiagnosticNotError(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	src := `version = "1.0.0"
default_environment = "dev"
environments = [ { name = "dev" } ]
layers = []
`
	require.NoError(t, os.WriteFile(specPath, []byte(src), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err, "missing @type is a soft warning, not an error")
	require.NotNil(t, ac)
	require.NotEmpty(t, ac.Errors)
	found := false
	for _, d := range ac.Errors {
		if contains(d.Message, "missing @type") {
			found = true
		}
	}
	assert.True(t, found, "diagnostic names the missing directive")
	// Spec still parses.
	assert.Equal(t, "1.0.0", ac.Spec.Version)
}

func TestExpand_MissingParentIsDiagnostic(t *testing.T) {
	spec := &Spec{
		Environments: []Environment{
			{Name: "dev", Extends: "nope"},
		},
	}
	envs, _, diags := Expand(spec)
	require.NotEmpty(t, diags, "missing parent surfaces as a diagnostic")
	found := false
	for _, d := range diags {
		if contains(d.Message, "unknown parent") && contains(d.Message, "nope") {
			found = true
		}
	}
	assert.True(t, found)
	// Partial ancestry is still recorded — "dev" alone, since
	// the walk stopped at "nope".
	assert.Equal(t, []string{"dev"}, envs["dev"].Ancestors,
		"partial ancestry collected up to the missing parent")
}

func TestExpand_CycleIsDiagnostic(t *testing.T) {
	spec := &Spec{
		Environments: []Environment{
			{Name: "a", Extends: "b"},
			{Name: "b", Extends: "a"},
		},
	}
	_, _, diags := Expand(spec)
	require.NotEmpty(t, diags)
	found := false
	for _, d := range diags {
		if contains(d.Message, "cycle") {
			found = true
		}
	}
	assert.True(t, found, "cycle is reported with a 'cycle' diagnostic")
}

func TestResolveOrder_PerEnvExpandsByEnvAncestry(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(simpleChainSpec), 0o600))
	ac, err := Load(specPath)
	require.NoError(t, err)

	order := ac.ResolveOrder("dev")
	// Spec has 3 entries: base (ONCE), env (PER_ENV), sec (ONCE
	// using ${env}). For env=dev with ancestors [prod, sandbox, dev]:
	//   base.pxf,
	//   env/prod.pxf, env/sandbox.pxf, env/dev.pxf,
	//   dev.sops.pxf
	require.Len(t, order, 5, "1 base + 3 per-env + 1 sec(once with substitution) = 5")
	assert.Equal(t, "base.pxf", order[0].ResolvedPath)
	assert.Equal(t, "env/prod.pxf", order[1].ResolvedPath)
	assert.Equal(t, "prod", order[1].Environment)
	assert.Equal(t, "env/sandbox.pxf", order[2].ResolvedPath)
	assert.Equal(t, "env/dev.pxf", order[3].ResolvedPath)
	assert.Equal(t, "dev.sops.pxf", order[4].ResolvedPath,
		"ONCE with ${env} substitutes the leaf env at resolve time")
}

func TestResolveOrder_RootEnvHasOnlySelfAncestor(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(simpleChainSpec), 0o600))
	ac, err := Load(specPath)
	require.NoError(t, err)

	order := ac.ResolveOrder("prod")
	// prod has no ancestors beyond itself; env expansion → only env/prod.pxf.
	require.Len(t, order, 3, "1 base + 1 per-env + 1 sec")
	assert.Equal(t, "base.pxf", order[0].ResolvedPath)
	assert.Equal(t, "env/prod.pxf", order[1].ResolvedPath)
	assert.Equal(t, "prod.sops.pxf", order[2].ResolvedPath)
}

func TestResolveOrder_UnknownEnvIsEmpty(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(simpleChainSpec), 0o600))
	ac, err := Load(specPath)
	require.NoError(t, err)

	assert.Empty(t, ac.ResolveOrder("nonexistent"),
		"querying an undeclared env returns an empty slice")
}

func TestResolveOrder_NilReceiverIsSafe(t *testing.T) {
	var ac *ActiveChain
	assert.Nil(t, ac.ResolveOrder("dev"),
		"nil ActiveChain receiver doesn't panic")
}

func TestExpand_NilSpecIsSafe(t *testing.T) {
	envs, layers, diags := Expand(nil)
	assert.Nil(t, envs)
	assert.Nil(t, layers)
	assert.Nil(t, diags)
}

// ---------------------------------------------------------------------------
// Sources block parsing + env-var name encoding (C7 part 2)
// ---------------------------------------------------------------------------

const sourcesChainSpec = `@type chameleon.v1.ChainSpec

version = "1.0.0"
default_environment = "dev"

environments = [ { name = "dev" } ]

sources = {
  "fs_default": { kind = "fs"  options = { "root": "./layers" } }
  "env_runtime": { kind = "env" options = { "prefix": "MYAPP_" } }
  "env_bare":    { kind = "env" }
}

layers = [
  { id = "base" path = "base.pxf" for_each = ONCE }
  { id = "env"  path = "(env)"     source = "env_runtime" for_each = ONCE }
]
`

func TestLoad_ParsesSourcesBlock(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(sourcesChainSpec), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)
	require.NotEmpty(t, ac.Sources, "sources map populated when block is present")
	assert.Equal(t, "fs", ac.Sources["fs_default"].Kind)
	assert.Equal(t, "./layers", ac.Sources["fs_default"].Options["root"])
	assert.Equal(t, "env", ac.Sources["env_runtime"].Kind)
	assert.Equal(t, "MYAPP_", ac.Sources["env_runtime"].Options["prefix"])
	assert.Equal(t, "env", ac.Sources["env_bare"].Kind)
	assert.Empty(t, ac.Sources["env_bare"].Options["prefix"],
		"bare env source has no prefix option")
}

func TestLoad_EmptySourcesYieldsEmptyMap(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(simpleChainSpec), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)
	assert.Empty(t, ac.Sources,
		"spec with no sources block yields an empty (non-nil) map")
}

func TestLayerIsEnvSourced_MatchesByName(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(sourcesChainSpec), 0o600))
	ac, err := Load(specPath)
	require.NoError(t, err)

	var baseLayer, envLayer LayerInstance
	for _, l := range ac.Layers {
		if l.ID == "base" {
			baseLayer = l
		}
		if l.ID == "env" {
			envLayer = l
		}
	}
	assert.False(t, ac.LayerIsEnvSourced(baseLayer),
		"layer without source field is not env-sourced")
	assert.True(t, ac.LayerIsEnvSourced(envLayer),
		"layer referencing an env-kind source is env-sourced")
}

func TestLayerIsEnvSourced_UnknownSourceNameIsFalse(t *testing.T) {
	ac := &ActiveChain{
		Sources: map[string]SourceRef{"a": {Name: "a", Kind: "env"}},
	}
	assert.False(t, ac.LayerIsEnvSourced(LayerInstance{Source: "nonexistent"}))
	assert.False(t, ac.LayerIsEnvSourced(LayerInstance{Source: ""}))
	// nil receiver is safe.
	var nilAC *ActiveChain
	assert.False(t, nilAC.LayerIsEnvSourced(LayerInstance{Source: "a"}))
}

func TestEnvVarNameForPath_UpperSnakeWithOptionalPrefix(t *testing.T) {
	cases := []struct {
		prefix, path, want string
	}{
		{"", "db.host", "DB_HOST"},
		{"MYAPP_", "db.host", "MYAPP_DB_HOST"},
		{"MYAPP_", "api_url", "MYAPP_API_URL"},
		{"", "deep.nested.field_path", "DEEP_NESTED_FIELD_PATH"},
		{"PFX_", "", "PFX_"},
		{"", "", ""},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			assert.Equal(t, c.want, EnvVarNameForPath(c.prefix, c.path))
		})
	}
}

func TestLoad_WrongAtTypeWarns(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	src := `@type some.other.Thing
version = "1.0.0"
environments = [ { name = "x" } ]
layers = []
`
	require.NoError(t, os.WriteFile(specPath, []byte(src), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)
	found := false
	for _, d := range ac.Errors {
		if contains(d.Message, "unexpected @type") && contains(d.Message, "some.other.Thing") {
			found = true
		}
	}
	assert.True(t, found, "wrong @type surfaces as a diagnostic naming the bad value")
}

// contains is a tiny strings.Contains stand-in to keep test imports
// minimal (mirrors the helper used in the workspaceconfig parser
// tests).
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// chameleonTemplateSpec mirrors pxfed's chameleon template
// (packages/pxfed-shell/resources/templates/chameleon/chain.pxf):
// a single "default" fs source and layer entries that OMIT
// `source =`. The runtime binds those layers to the default source
// implicitly; Expand used to miss that rule, leaving
// ResolvedPath without the `layers/` root and downgrading every
// chain-aware feature to the permissive fs-root fallback.
const chameleonTemplateSpec = `@type chameleon.v1.ChainSpec
version = "1.0.0"
default_environment = "dev"
environments = [ { name = "prod" } { name = "dev" extends = "prod" } ]
sources = {
  "default": { kind = "fs" options = { "root": "./layers" } }
}
layers = [
  { id = "base" path = "base.pxf"       for_each = ONCE }
  { id = "env"  path = "env/${env}.pxf" for_each = PER_ENV }
]
`

func TestExpand_ImplicitDefaultSourceAppliesRoot(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(chameleonTemplateSpec), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)
	assert.Empty(t, ac.Errors)

	byPath := map[string]LayerInstance{}
	for _, l := range ac.Layers {
		byPath[l.ResolvedPath] = l
	}

	base, ok := byPath["layers/base.pxf"]
	require.True(t, ok,
		"ONCE layer without source= must resolve under the default source root; got %v", ac.Layers)
	assert.Equal(t, "default", base.Source,
		"implicit binding surfaces on LayerInstance.Source so LayerInfo reports it")

	dev, ok := byPath["layers/env/dev.pxf"]
	require.True(t, ok,
		"PER_ENV layer without source= must resolve under the default source root; got %v", ac.Layers)
	assert.Equal(t, "dev", dev.Environment)
	assert.Equal(t, "default", dev.Source)
	_, ok = byPath["layers/env/prod.pxf"]
	assert.True(t, ok, "PER_ENV expansion covers every declared environment")
}

func TestResolveOrder_ImplicitDefaultSourceAppliesRoot(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "chain.pxf")
	require.NoError(t, os.WriteFile(specPath, []byte(chameleonTemplateSpec), 0o600))

	ac, err := Load(specPath)
	require.NoError(t, err)

	order := ac.ResolveOrder("dev")
	var resolved []string
	for _, l := range order {
		resolved = append(resolved, l.ResolvedPath)
		assert.Equal(t, "default", l.Source)
	}
	assert.Equal(t,
		[]string{"layers/base.pxf", "layers/env/prod.pxf", "layers/env/dev.pxf"},
		resolved,
		"resolve order reads the same rooted paths the runtime loads")
}

func TestExpand_NoDeclaredDefaultKeepsBarePaths(t *testing.T) {
	// Sources exist but none is named "default": a sourceless layer
	// must keep its bare path and empty Source — the earlier
	// behavior for chains that genuinely have no default source.
	spec := &Spec{
		Environments: []Environment{{Name: "dev"}},
		Sources: []SourceRef{
			{Name: "secrets", Kind: "fs", Options: map[string]string{"root": "./layers/secrets"}},
		},
		Layers: []LayerEntry{{ID: "base", Path: "base.pxf", ForEach: "ONCE"}},
	}
	_, layers, diags := Expand(spec)
	require.Empty(t, diags)
	require.Len(t, layers, 1)
	assert.Equal(t, "base.pxf", layers[0].ResolvedPath)
	assert.Empty(t, layers[0].Source)
}

func TestLayerIsEnvSourced_ImplicitDefaultEnvSource(t *testing.T) {
	// A "default" source declared as kind=env: layers omitting
	// source= bind to it, so env-source affordances (required-field
	// suppression, hover hints) apply — same as the runtime, which
	// would consult env vars for those layers.
	spec := &Spec{
		Environments: []Environment{{Name: "dev"}},
		Sources:      []SourceRef{{Name: "default", Kind: "env"}},
		Layers:       []LayerEntry{{ID: "env", Path: "(env)", ForEach: "ONCE"}},
	}
	_, layers, _ := Expand(spec)
	require.Len(t, layers, 1)
	ac := &ActiveChain{Sources: map[string]SourceRef{"default": spec.Sources[0]}}
	assert.True(t, ac.LayerIsEnvSourced(layers[0]),
		"implicit default binding makes env-kind default sources visible to LayerIsEnvSourced")
}
