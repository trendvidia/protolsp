// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package workspaceconfig

// ConfigFileName is the on-disk name of the workspace configuration
// file (see workspace-config.md §"Location and naming").
const ConfigFileName = ".protolsp.pxf"

// ExpectedType is the @type directive value every .protolsp.pxf
// must carry. Parsers warn loudly when this is missing or
// mismatched — the directive disambiguates the file from any
// other PXF document a user might accidentally name
// `.protolsp.pxf`.
const ExpectedType = "protolsp.v1.WorkspaceConfig"

// Config is the parsed, interpolation-resolved view of a
// .protolsp.pxf file. Fields are the union of everything any
// feature might consume.
//
// Unset fields stay at their zero value. Callers consume fields
// they recognize and ignore the rest — this keeps adding new
// sections additive.
type Config struct {
	// Registry is the workspace's registry schema source. Sourced
	// from a top-level `registry { ... }` block (or the legacy
	// `schema_sources { registry { ... } }` shape, with a
	// deprecation warning). W3 composes it with ProtoFiles via
	// schema.WithFallback.
	Registry RegistrySource

	// ProtoFiles is the workspace's local .proto-files source.
	// Sourced from a top-level `proto_files { ... }` block (or
	// the legacy `schema_sources { proto_files { ... } }` shape,
	// with a deprecation warning). W3 composes it with Registry
	// via schema.WithFallback.
	ProtoFiles ProtoFilesSource

	// Security mirrors the `security { ... }` block from the
	// secret-handling design (S4). Today the only field is
	// InlineSecrets — allow / warn / refuse policy governing the
	// literal-secret diagnostic emit path.
	Security SecurityConfig

	// Chameleon mirrors the `chameleon { ... }` block from the
	// chameleon-integration design (C1). When ChainSpec is set the
	// LSP loads the named chain.pxf at initialize time and exposes
	// it via the protolsp/chainStatus request.
	Chameleon ChameleonConfig

	// HasContent reports whether the parser found ANY known
	// configuration in the file. Empty/unrecognized files set
	// this to false so callers can distinguish "no config" from
	// "config that didn't apply."
	HasContent bool
}

// ChameleonConfig mirrors the `chameleon { ... }` block from
// .protolsp.pxf. Empty when the workspace has no chain.
type ChameleonConfig struct {
	// ChainSpec is the path to the chain.pxf, relative to the
	// .protolsp.pxf directory. Empty disables chain-aware mode.
	ChainSpec string

	// SchemaFQN is the assumed message type for layer files that
	// don't carry their own @type directive. Optional.
	SchemaFQN string

	// Enabled is the toggle for chain-aware behaviors. Defaults to
	// true when ChainSpec is set; the parser flips this to false
	// only when the file says so explicitly. Consumers should check
	// `Enabled && ChainSpec != ""` to decide whether to activate.
	Enabled bool
}

// IsEmpty reports whether ChameleonConfig is the zero value — used
// to flag HasContent for chameleon-only files.
func (c ChameleonConfig) IsEmpty() bool {
	return c.ChainSpec == "" && c.SchemaFQN == "" && !c.Enabled
}

// SecurityConfig mirrors the `security { ... }` block. Fields are
// strings rather than enums so unrecognized values flow through to
// the consumer (which can decide whether to log + fall back to a
// default vs reject) — keeps the parser additive as new policies
// land.
type SecurityConfig struct {
	// InlineSecrets governs the literal-secret diagnostic from
	// secret-handling S3. Recognized values:
	//   - "allow"  — no diagnostic emitted
	//   - "warn"   — Warning severity (the design's default)
	//   - "refuse" — Error severity
	// Empty means "not set in this file" — lower tiers (defaults)
	// supply the policy.
	InlineSecrets string
}

// RegistrySource mirrors the proto's RegistrySource shape but is
// hand-written so W1 doesn't need a proto-compile step.
type RegistrySource struct {
	Address   string
	Namespace string
	Token     string
	CacheDir  string
}

// IsEmpty reports whether the RegistrySource is the zero value.
// Callers use this to decide whether to merge the file's Tier 2
// values into the resolver chain at all.
func (r RegistrySource) IsEmpty() bool {
	return r.Address == "" && r.Namespace == "" && r.Token == "" && r.CacheDir == ""
}

// ProtoFilesSource mirrors the proto's ProtoFilesSource shape:
// directories scanned for `*.proto` files, compiled by W2 into a
// local descriptor set that backs a schema.Lookup. Paths are
// interpreted relative to the directory containing .protolsp.pxf.
//
// Line / Column anchor the block opening so compile errors can be
// surfaced as diagnostics on the right region of .protolsp.pxf
// (W2's "compile failure becomes editor diagnostic" requirement).
type ProtoFilesSource struct {
	Paths []string

	Line   int
	Column int
}

// IsEmpty reports whether the ProtoFilesSource carries no
// directories — used by the parser to decide whether to keep the
// block in the config slice at all.
func (p ProtoFilesSource) IsEmpty() bool {
	return len(p.Paths) == 0
}

// IsEmpty reports whether the SecurityConfig is the zero value.
// Used to decide whether HasContent should flip true on a
// security-only file.
func (s SecurityConfig) IsEmpty() bool {
	return s.InlineSecrets == ""
}
