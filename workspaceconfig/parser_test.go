// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package workspaceconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validConfig = `@type protolsp.v1.WorkspaceConfig

registry {
  address    = "registry.example.com:443"
  namespace  = "acme-billing"
  token      = "tok-abc123"
  cache_dir  = "/var/cache/protolsp"
}
`

func TestParse_WellFormedRegistry(t *testing.T) {
	cfg, warnings, err := Parse([]byte(validConfig))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, warnings, "well-formed config has no warnings")
	assert.True(t, cfg.HasContent)

	assert.Equal(t, "registry.example.com:443", cfg.Registry.Address)
	assert.Equal(t, "acme-billing", cfg.Registry.Namespace)
	assert.Equal(t, "tok-abc123", cfg.Registry.Token)
	assert.Equal(t, "/var/cache/protolsp", cfg.Registry.CacheDir)
}

func TestParse_MissingTypeDirective(t *testing.T) {
	src := `registry { address = "x:443" }
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err, "missing @type is a warning, not an error")
	require.NotNil(t, cfg)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "missing @type")
	assert.Equal(t, "x:443", cfg.Registry.Address,
		"parsing continues despite missing @type")
}

func TestParse_WrongTypeDirective(t *testing.T) {
	src := `@type some.other.Thing

registry { address = "x:443" }
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "unexpected @type")
	assert.Contains(t, warnings[0].Message, "some.other.Thing",
		"warning names the wrong type")
	assert.Equal(t, "x:443", cfg.Registry.Address)
}

func TestParse_EnvInterpolation(t *testing.T) {
	t.Setenv("REGISTRY_TOKEN", "secret-from-env")
	t.Setenv("CACHE_HOME", "/home/me/.cache")

	src := `@type protolsp.v1.WorkspaceConfig

registry {
  address   = "registry.example.com:443"
  token     = "${env.REGISTRY_TOKEN}"
  cache_dir = "${env.CACHE_HOME}/protolsp"
}
`
	cfg, _, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, "secret-from-env", cfg.Registry.Token,
		"${env.X} expands to the env var value")
	assert.Equal(t, "/home/me/.cache/protolsp", cfg.Registry.CacheDir,
		"interpolation works inside larger strings")
}

func TestParse_EnvVarUnset(t *testing.T) {
	// REGISTRY_TOKEN_NOT_SET is not set — expansion produces "".
	src := `@type protolsp.v1.WorkspaceConfig

registry {
  token = "${env.REGISTRY_TOKEN_NOT_SET}"
}
`
	cfg, _, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, "", cfg.Registry.Token,
		"unset env vars expand to empty string")
}

func TestParse_UnrecognizedRegistryField(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

registry {
  address = "x:443"
  bogus_field = "nope"
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, "x:443", cfg.Registry.Address)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "bogus_field")
}

func TestParse_UnrecognizedTopBlockWarns(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

something_unknown {
  foo = "bar"
}
`
	_, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "something_unknown")
}

func TestParse_ProtoFilesSourceCollectsPaths(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

proto_files {
  paths = ["proto/", "third_party/proto/"]
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.False(t, cfg.ProtoFiles.IsEmpty())
	assert.Equal(t, []string{"proto/", "third_party/proto/"}, cfg.ProtoFiles.Paths)
	assert.True(t, cfg.HasContent, "proto_files-only config still has content")
	assert.NotZero(t, cfg.ProtoFiles.Line, "block position recorded for diagnostics")
}

func TestParse_ProtoFilesPathsMustBeStrings(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

proto_files {
  paths = ["ok/", 42]
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.False(t, cfg.ProtoFiles.IsEmpty())
	assert.Equal(t, []string{"ok/"}, cfg.ProtoFiles.Paths,
		"non-string elements are dropped, strings retained")
	require.NotEmpty(t, warnings)
	assert.Contains(t, warnings[0].Message, "must be strings")
}

func TestParse_ProtoFilesUnknownFieldWarns(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

proto_files {
  paths   = ["proto/"]
  exclude = ["**/*_test.proto"]
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.False(t, cfg.ProtoFiles.IsEmpty())
	assert.Equal(t, []string{"proto/"}, cfg.ProtoFiles.Paths)
	require.NotEmpty(t, warnings)
	assert.Contains(t, warnings[0].Message, "exclude")
}

func TestParse_DuplicateRegistryWarnsSecondIgnored(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

registry { address = "first:443" }
registry { address = "second:443" }
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, "first:443", cfg.Registry.Address,
		"first registry wins; subsequent ones ignored")
	require.NotEmpty(t, warnings)
	found := false
	for _, w := range warnings {
		if assertContains(w.Message, "duplicate registry") {
			found = true
			break
		}
	}
	assert.True(t, found, "warns about the duplicate registry")
}

func TestParse_DuplicateProtoFilesWarnsSecondIgnored(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

proto_files { paths = ["first/"] }
proto_files { paths = ["second/"] }
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, []string{"first/"}, cfg.ProtoFiles.Paths,
		"first proto_files wins; subsequent ones ignored")
	require.NotEmpty(t, warnings)
	found := false
	for _, w := range warnings {
		if assertContains(w.Message, "duplicate proto_files") {
			found = true
			break
		}
	}
	assert.True(t, found, "warns about the duplicate proto_files")
}

// ---------------------------------------------------------------------------
// Legacy schema_sources { ... } wrapper (back-compat shim, removed
// after the Set RPC has rewritten all files in the wild)
// ---------------------------------------------------------------------------

func TestParse_LegacySchemaSourcesRegistryStillWorks(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

schema_sources {
  registry {
    address   = "legacy:443"
    namespace = "ns"
  }
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, "legacy:443", cfg.Registry.Address,
		"legacy nested registry still loads")
	assert.Equal(t, "ns", cfg.Registry.Namespace)
	require.NotEmpty(t, warnings)
	found := false
	for _, w := range warnings {
		if assertContains(w.Message, "schema_sources") && assertContains(w.Message, "deprecated") {
			found = true
			break
		}
	}
	assert.True(t, found, "legacy schema_sources wrapper emits a deprecation warning")
}

func TestParse_LegacySchemaSourcesProtoFilesStillWorks(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

schema_sources {
  proto_files {
    paths = ["proto/"]
  }
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, []string{"proto/"}, cfg.ProtoFiles.Paths,
		"legacy nested proto_files still loads")
	require.NotEmpty(t, warnings)
	deprecation := false
	for _, w := range warnings {
		if assertContains(w.Message, "deprecated") {
			deprecation = true
		}
	}
	assert.True(t, deprecation, "deprecation warning emitted for legacy wrapper")
}

func TestParse_LegacyMixedWithCanonicalDuplicateWarns(t *testing.T) {
	// A file mid-migration may have a legacy schema_sources wrapper
	// AND a top-level registry block. The first one wins; the second
	// gets the duplicate warning, regardless of source shape.
	src := `@type protolsp.v1.WorkspaceConfig

schema_sources {
  registry { address = "from-legacy:443" }
}

registry { address = "from-flat:443" }
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, "from-legacy:443", cfg.Registry.Address,
		"first occurrence wins regardless of which shape it used")
	deprecation, duplicate := false, false
	for _, w := range warnings {
		if assertContains(w.Message, "deprecated") {
			deprecation = true
		}
		if assertContains(w.Message, "duplicate registry") {
			duplicate = true
		}
	}
	assert.True(t, deprecation, "legacy wrapper warns")
	assert.True(t, duplicate, "second registry occurrence warns")
}

func TestParse_EmptyFile(t *testing.T) {
	cfg, _, err := Parse([]byte(""))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.False(t, cfg.HasContent)
	assert.True(t, cfg.Registry.IsEmpty())
}

func TestParse_MalformedReturnsError(t *testing.T) {
	// Unmatched `{` — pxf.Parse rejects this.
	src := `@type protolsp.v1.WorkspaceConfig

registry {
  address = "x:443"
`
	_, _, err := Parse([]byte(src))
	assert.Error(t, err,
		"unparseable PXF returns an error (not just warnings)")
}

// assertContains is a small helper that mirrors strings.Contains
// without pulling in the assert package's nested-call semantics.
func assertContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// security block (S4)
// ---------------------------------------------------------------------------

func TestParse_SecurityInlineSecretsValid(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

security {
  inline_secrets = "refuse"
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, warnings)
	assert.Equal(t, "refuse", cfg.Security.InlineSecrets)
	assert.True(t, cfg.HasContent,
		"security-only config still has content")
}

func TestParse_SecurityInlineSecretsAllValues(t *testing.T) {
	for _, want := range []string{"allow", "warn", "refuse"} {
		t.Run(want, func(t *testing.T) {
			src := `@type protolsp.v1.WorkspaceConfig

security { inline_secrets = "` + want + `" }
`
			cfg, warnings, err := Parse([]byte(src))
			require.NoError(t, err)
			assert.Empty(t, warnings)
			assert.Equal(t, want, cfg.Security.InlineSecrets)
		})
	}
}

func TestParse_SecurityInlineSecretsInvalidValueWarns(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

security { inline_secrets = "maybe" }
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotEmpty(t, warnings)
	assert.Empty(t, cfg.Security.InlineSecrets,
		"bad enum value rejected → field stays unset (lower tiers supply policy)")
	found := false
	for _, w := range warnings {
		if assertContains(w.Message, "allow|warn|refuse") {
			found = true
		}
	}
	assert.True(t, found, "warning names the allowed values")
}

// ---------------------------------------------------------------------------
// chameleon block (C1 of chameleon-integration)
// ---------------------------------------------------------------------------

func TestParse_ChameleonFullBlock(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "config/chain.pxf"
  schema_fqn = "myapp.v1.AppConfig"
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, warnings)
	assert.Equal(t, "config/chain.pxf", cfg.Chameleon.ChainSpec)
	assert.Equal(t, "myapp.v1.AppConfig", cfg.Chameleon.SchemaFQN)
	assert.True(t, cfg.Chameleon.Enabled,
		"enabled defaults to true when chain_spec is set")
	assert.True(t, cfg.HasContent,
		"chameleon-only config still has content")
}

func TestParse_ChameleonExplicitDisable(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "config/chain.pxf"
  enabled = false
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.Equal(t, "config/chain.pxf", cfg.Chameleon.ChainSpec)
	assert.False(t, cfg.Chameleon.Enabled,
		"explicit `enabled = false` overrides the chain_spec default")
}

func TestParse_ChameleonChainSpecOnly(t *testing.T) {
	// schema_fqn is optional.
	src := `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "config/chain.pxf"
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.Equal(t, "config/chain.pxf", cfg.Chameleon.ChainSpec)
	assert.Empty(t, cfg.Chameleon.SchemaFQN)
	assert.True(t, cfg.Chameleon.Enabled)
}

func TestParse_ChameleonEnvInterpolation(t *testing.T) {
	t.Setenv("MYAPP_CHAIN_PATH", "envs/dev-chain.pxf")
	src := `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "${env.MYAPP_CHAIN_PATH}"
}
`
	cfg, _, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, "envs/dev-chain.pxf", cfg.Chameleon.ChainSpec,
		"chain_spec resolves ${env.X} like other string fields")
}

func TestParse_ChameleonUnknownFieldWarns(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = "config/chain.pxf"
  mystery_field = "x"
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, "config/chain.pxf", cfg.Chameleon.ChainSpec)
	found := false
	for _, w := range warnings {
		if assertContains(w.Message, "mystery_field") {
			found = true
		}
	}
	assert.True(t, found, "unknown chameleon field surfaces as a warning")
}

func TestParse_ChameleonBadTypeWarns(t *testing.T) {
	// chain_spec must be a string; enabled must be a bool.
	src := `@type protolsp.v1.WorkspaceConfig

chameleon {
  chain_spec = 42
  enabled = "yes"
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Empty(t, cfg.Chameleon.ChainSpec)
	require.GreaterOrEqual(t, len(warnings), 2)
	chainSpecWarned, enabledWarned := false, false
	for _, w := range warnings {
		if assertContains(w.Message, "chain_spec") {
			chainSpecWarned = true
		}
		if assertContains(w.Message, "enabled") {
			enabledWarned = true
		}
	}
	assert.True(t, chainSpecWarned, "bad chain_spec type warns")
	assert.True(t, enabledWarned, "bad enabled type warns")
}

func TestParse_SecurityUnknownFieldWarns(t *testing.T) {
	src := `@type protolsp.v1.WorkspaceConfig

security {
  inline_secrets = "warn"
  unknown_knob = "x"
}
`
	cfg, warnings, err := Parse([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, "warn", cfg.Security.InlineSecrets,
		"known field still applied alongside unknown one")
	found := false
	for _, w := range warnings {
		if assertContains(w.Message, "unknown_knob") {
			found = true
		}
	}
	assert.True(t, found, "unknown security field surfaces as a warning")
}
