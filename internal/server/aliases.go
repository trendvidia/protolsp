// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Aliases into the public analysis package (#172 rung 3). The engine
// half of this package graduated to
// github.com/trendvidia/protolsp/analysis; these aliases keep the
// LSP/RPC shell and the (large) in-package test suite compiling
// against their historical bare names without a mass rewrite. New
// code should import the analysis package directly.

package server

import (
	"github.com/trendvidia/protolsp/analysis"
)

type (
	Documents = analysis.Documents
	Document  = analysis.Document
	Publisher = analysis.Publisher

	SecretRange   = analysis.SecretRange
	SecretForm    = analysis.SecretForm
	DatasetRegion = analysis.DatasetRegion
	ConfigValue   = analysis.ConfigValue
	ConfigSource  = analysis.ConfigSource

	ChainStatusParams            = analysis.ChainStatusParams
	ConfigAttribute              = analysis.ConfigAttribute
	ConfigGetParams              = analysis.ConfigGetParams
	ConfigGetResult              = analysis.ConfigGetResult
	ConfigListResult             = analysis.ConfigListResult
	ConfigSetParams              = analysis.ConfigSetParams
	ConfigSetResult              = analysis.ConfigSetResult
	DatasetRegionsChangedParams  = analysis.DatasetRegionsChangedParams
	DatasetRegionsParams         = analysis.DatasetRegionsParams
	DatasetRegionsResult         = analysis.DatasetRegionsResult
	EffectiveConfigChangedParams = analysis.EffectiveConfigChangedParams
	GetEffectiveConfigParams     = analysis.GetEffectiveConfigParams
	GetEffectiveConfigResult     = analysis.GetEffectiveConfigResult
	LayerInfoParams              = analysis.LayerInfoParams
	ListMessagesParams           = analysis.ListMessagesParams
	ListMessagesResult           = analysis.ListMessagesResult
	MessageEntry                 = analysis.MessageEntry
	EngineConfigResult           = analysis.EngineConfigResult
	RegistryStatusResult         = analysis.RegistryStatusResult
	RuntimeSourceRef             = analysis.RuntimeSourceRef
	RuntimeViolation             = analysis.RuntimeViolation
	SecretRangeEntry             = analysis.SecretRangeEntry
	SecretRangesChangedParams    = analysis.SecretRangesChangedParams
	SecretRangesParams           = analysis.SecretRangesParams
	SecretRangesResult           = analysis.SecretRangesResult
	SemanticTokensProvider       = analysis.SemanticTokensProvider
	SetOverridesApplied          = analysis.SetOverridesApplied
	SetOverridesParams           = analysis.SetOverridesParams
	SetOverridesResult           = analysis.SetOverridesResult
	SetRuntimeViolationsParams   = analysis.SetRuntimeViolationsParams
	SourceContentParams          = analysis.SourceContentParams
	SourceContentResult          = analysis.SourceContentResult
)

var (
	readClientCapabilities = analysis.ReadClientCapabilities

	ApplyContentChanges         = analysis.ApplyContentChanges
	DecodeRawByKind             = analysis.DecodeRawByKind
	DiffSnapshots               = analysis.DiffSnapshots
	IsFileURI                   = analysis.IsFileURI
	IsProtoURI                  = analysis.IsProtoURI
	IsValidKeyedEntryKey        = analysis.IsValidKeyedEntryKey
	IsValidPxfIdentifier        = analysis.IsValidPxfIdentifier
	KnownSections               = analysis.KnownSections
	ParseRegistryURI            = analysis.ParseRegistryURI
	RedactChangeMap             = analysis.RedactChangeMap
	SectionsOf                  = analysis.SectionsOf
	SemanticTokensLegend        = analysis.SemanticTokensLegend
	ToSecretRangeEntries        = analysis.ToSecretRangeEntries
	UnmarshalIncrementalChanges = analysis.UnmarshalIncrementalChanges
	URIToFilePath               = analysis.URIToFilePath
	ValidateOverrides           = analysis.ValidateOverrides
)

const (
	sectionRegistryAddress       = analysis.SectionRegistryAddress
	sectionRegistryNamespace     = analysis.SectionRegistryNamespace
	sectionRegistryToken         = analysis.SectionRegistryToken
	sectionRegistryCacheDir      = analysis.SectionRegistryCacheDir
	sectionSecurityInlineSecrets = analysis.SectionSecurityInlineSecrets

	MethodChainStatus            = analysis.MethodChainStatus
	MethodConfigGet              = analysis.MethodConfigGet
	MethodConfigList             = analysis.MethodConfigList
	MethodConfigSet              = analysis.MethodConfigSet
	MethodDatasetRegions         = analysis.MethodDatasetRegions
	MethodDatasetRegionsChanged  = analysis.MethodDatasetRegionsChanged
	MethodEffectiveConfigChanged = analysis.MethodEffectiveConfigChanged
	MethodGetEffectiveConfig     = analysis.MethodGetEffectiveConfig
	MethodLayerInfo              = analysis.MethodLayerInfo
	MethodListMessages           = analysis.MethodListMessages
	MethodEngineConfig           = analysis.MethodEngineConfig
	MethodRegistryStatus         = analysis.MethodRegistryStatus
	MethodSecretRanges           = analysis.MethodSecretRanges
	MethodSecretRangesChanged    = analysis.MethodSecretRangesChanged
	MethodSemanticTokensFull     = analysis.MethodSemanticTokensFull
	MethodSetOverrides           = analysis.MethodSetOverrides
	MethodSetRuntimeViolations   = analysis.MethodSetRuntimeViolations
	MethodSourceContent          = analysis.MethodSourceContent

	SecretFormBlock = analysis.SecretFormBlock

	SourceDefault  = analysis.SourceDefault
	SourceFile     = analysis.SourceFile
	SourceOverride = analysis.SourceOverride
	SourceSettings = analysis.SourceSettings
)

// Test-surface aliases: a handful of integration-flavored tests in
// this package still exercise engine surface directly (dataset
// detection, diagnostic-data decoding, config defaults); these keep
// them compiling. The pure-helper unit tests moved into the
// analysis package itself (#179), which let their helpers drop to
// unexported names.

type (
	ChainStatusResult  = analysis.ChainStatusResult
	IncrementalChange  = analysis.IncrementalChange
	LayerInfoResult    = analysis.LayerInfoResult
	LiteralSecretData  = analysis.LiteralSecretData
	PositionShape      = analysis.PositionShape
	ProtocolRangeShape = analysis.ProtocolRangeShape
	UnknownFieldData   = analysis.UnknownFieldData
)

var (
	BuildRegistryURI        = analysis.BuildRegistryURI
	DecodeLiteralSecretData = analysis.DecodeLiteralSecretData
	DecodeUnknownFieldData  = analysis.DecodeUnknownFieldData
	DetectDatasetRegions    = analysis.DetectDatasetRegions
	FindWorkspaceDirFor     = analysis.FindWorkspaceDirFor
	PxfPosToLSP             = analysis.PxfPosToLSP
	SemanticTokenTypes      = analysis.SemanticTokenTypes
)

const (
	DiagDataKindLiteralSecret = analysis.DiagDataKindLiteralSecret
	DiagDataKindUnknownField  = analysis.DiagDataKindUnknownField
	SecretFormScalar          = analysis.SecretFormScalar
)
