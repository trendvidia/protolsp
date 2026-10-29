// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package workspaceconfig

import "fmt"

// AttributeKind distinguishes the value types a workspace
// attribute can carry. Drives the Get/Set type checks and the
// canonical type-tag string returned by the configList RPC.
type AttributeKind int

const (
	KindString AttributeKind = iota
	KindStringList
	KindBool
)

// String returns the canonical type-tag for LSP responses. Stable
// across releases — pxfed / the hosted UI render this directly
// (e.g. into a settings-form input type).
func (k AttributeKind) String() string {
	switch k {
	case KindString:
		return "string"
	case KindStringList:
		return "string[]"
	case KindBool:
		return "bool"
	default:
		return "unknown"
	}
}

// AttributeSpec describes one settable attribute of the workspace
// config: its dotted name, a human-readable display label, a one-
// line description, the value type Set expects, and an optional
// semantic validator (e.g. enum membership) that runs after the
// type check.
type AttributeSpec struct {
	// Name is the dotted attribute identifier — stable across
	// releases and used as the wire key for Get/Set. UI shouldn't
	// render this directly; render DisplayName.
	Name string

	// DisplayName is the label editors should show next to the
	// input widget. Short (1–3 words), title-case-ish, no dots
	// or underscores. Owned by protolsp so every UI consumer
	// (pxfed, the VS Code extension's planned settings panel, the
	// hosted IDE) shows the same wording.
	DisplayName string

	Description string
	Kind        AttributeKind

	// Validate optionally rejects values that pass the Kind check
	// but violate a semantic constraint. Nil means "any value of
	// the declared Kind is acceptable." Called by SetAttribute
	// before mutating the AST.
	Validate func(any) error
}

// AttributeRegistry is the canonical, hand-maintained list of
// every attribute the configList / configGet / configSet RPCs
// expose. Adding a field to WorkspaceConfig requires adding an
// entry here — the registry, not the proto descriptor, is the
// API surface.
//
// Why a static table and not descriptor walk: protoc-gen-go drops
// source-info by default, so proto comments aren't available at
// runtime for descriptions. Maintaining ~10 entries by hand is
// cheaper than building the source-info plumbing.
//
// Order matters: configList returns entries in declaration order,
// which UIs use to render the settings form top-to-bottom.
var AttributeRegistry = []AttributeSpec{
	{
		Name:        "registry.address",
		DisplayName: "Address",
		Description: `gRPC address of the registry, e.g. "registry.example.com:443".`,
		Kind:        KindString,
	},
	{
		Name:        "registry.namespace",
		DisplayName: "Namespace",
		Description: "Namespace ID to bind to within the registry.",
		Kind:        KindString,
	},
	{
		Name:        "registry.token",
		DisplayName: "Token",
		Description: "Bearer token for authenticated registries. Supports ${env.VAR}.",
		Kind:        KindString,
	},
	{
		Name:        "registry.cache_dir",
		DisplayName: "Cache directory",
		Description: "On-disk cache directory for fetched descriptors. Supports ${env.VAR}.",
		Kind:        KindString,
	},
	{
		Name:        "proto_files.paths",
		DisplayName: "Search paths",
		Description: "Paths to .proto files or directories, relative to the workspace root.",
		Kind:        KindStringList,
	},
	{
		Name:        `security.inline_secrets`,
		DisplayName: "Inline secrets policy",
		Description: `Literal-secret policy: "allow" | "warn" | "refuse".`,
		Kind:        KindString,
		Validate:    validateInlineSecrets,
	},
	{
		Name:        "chameleon.chain_spec",
		DisplayName: "Chain spec path",
		Description: "Path to the chain.pxf, relative to the .protolsp.pxf directory.",
		Kind:        KindString,
	},
	{
		Name:        "chameleon.schema_fqn",
		DisplayName: "Default schema type",
		Description: "Default @type for chain layer files that don't carry one.",
		Kind:        KindString,
	},
	{
		Name:        "chameleon.enabled",
		DisplayName: "Chain-aware behaviors",
		Description: "Toggle chain-aware behaviors. Defaults to true when chain_spec is set.",
		Kind:        KindBool,
	},
}

// LookupAttribute returns the AttributeSpec for name, or ok=false
// when no such attribute is registered. Callers that surface
// unknown-attribute errors to the user should prefer the explicit
// error from GetAttribute / SetAttribute over this lookup so the
// message is uniform.
func LookupAttribute(name string) (AttributeSpec, bool) {
	for _, a := range AttributeRegistry {
		if a.Name == name {
			return a, true
		}
	}
	return AttributeSpec{}, false
}

// validateInlineSecrets mirrors the parser's allowed-policy set
// (parser.go isValidInlineSecretsPolicy). Kept distinct so Set
// can reject a bad value at write-time rather than letting it
// land in the file and surface as a parse-time warning later.
func validateInlineSecrets(v any) error {
	s, _ := v.(string)
	switch s {
	case "allow", "warn", "refuse":
		return nil
	}
	return fmt.Errorf("must be one of allow|warn|refuse; got %q", s)
}
