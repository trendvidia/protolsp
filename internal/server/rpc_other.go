// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Connect-RPC handlers for the read-only / non-streaming services
// (Registry, Schema, Chameleon, Secrets.GetRanges,
// Datasets.GetRegions, EffectiveConfig.{GetEffectiveConfig,
// SetOverrides}). Streaming Subscribe endpoints on Secrets / Datasets
// / EffectiveConfig are deferred — pxfed still polls / re-requests
// via the LSP notification channel for those, and the migration to
// streaming RPCs is a follow-up that doesn't gate Phase 5's win.
//
// Each handler is a thin shim over the existing dispatch*-flavored
// logic. Wherever possible the body calls back into helpers we
// already use from the JSON-RPC path so there's a single source of
// truth for behavior.

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"
	"go.lsp.dev/uri"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
	"github.com/trendvidia/protolsp/proto/protolsp/v1/rpc/rpcpbconnect"

	"github.com/trendvidia/protolsp/analysis"
	"github.com/trendvidia/protolsp/chain"
	"github.com/trendvidia/protolsp/redact"
)

// ----------------------------------------------------------------
// RegistryService
// ----------------------------------------------------------------

type rpcRegistryService struct{ srv *Server }

func (s *Server) registerRegistryRPC(mux *http.ServeMux) {
	path, h := rpcpbconnect.NewRegistryServiceHandler(
		&rpcRegistryService{srv: s},
		s.rpcServiceInterceptors(),
	)
	mux.Handle(path, h)
}

func (h *rpcRegistryService) GetStatus(
	_ context.Context,
	_ *connect.Request[rpcpb.RegistryServiceGetStatusRequest],
) (*connect.Response[rpcpb.RegistryServiceGetStatusResponse], error) {
	res := h.srv.RegistryStatus()
	return connect.NewResponse(&rpcpb.RegistryServiceGetStatusResponse{
		Configured: res.Configured,
		Stale:      res.Stale,
	}), nil
}

// ----------------------------------------------------------------
// SchemaService — ListMessages + GetSourceContent
// ----------------------------------------------------------------

type rpcSchemaService struct{ srv *Server }

func (s *Server) registerSchemaRPC(mux *http.ServeMux) {
	path, h := rpcpbconnect.NewSchemaServiceHandler(
		&rpcSchemaService{srv: s},
		s.rpcServiceInterceptors(),
	)
	mux.Handle(path, h)
}

func (h *rpcSchemaService) ListMessages(
	ctx context.Context,
	req *connect.Request[rpcpb.ListMessagesRequest],
) (*connect.Response[rpcpb.ListMessagesResponse], error) {
	// Thin shim over the engine recipe; the no-lookup case
	// returns an empty list rather than an error — the editor UI
	// should render "no schema connected", not pop an RPC error.
	res, err := h.srv.ListMessages(ctx, req.Msg.Prefix)
	if err != nil {
		return nil, connect.NewError(connect.CodeCanceled, err)
	}
	out := make([]*rpcpb.MessageEntry, 0, len(res.Messages))
	for _, m := range res.Messages {
		out = append(out, &rpcpb.MessageEntry{
			FullName:  m.FullName,
			File:      m.File,
			Namespace: m.Namespace,
		})
	}
	return connect.NewResponse(&rpcpb.ListMessagesResponse{Messages: out}), nil
}

func (h *rpcSchemaService) GetSourceContent(
	ctx context.Context,
	req *connect.Request[rpcpb.GetSourceContentRequest],
) (*connect.Response[rpcpb.GetSourceContentResponse], error) {
	// Thin shim over the engine recipe, which owns the
	// `_internal_` namespace routing. WorkspaceDir scopes the fetch
	// to a sub-workspace's own registry when set. Error
	// classes map to Connect codes here.
	res, err := h.srv.SourceContentForWorkspace(ctx, uri.URI(req.Msg.Uri), req.Msg.WorkspaceDir)
	if err != nil {
		code := connect.CodeInternal
		switch {
		case errors.Is(err, analysis.ErrNotRegistryURI):
			code = connect.CodeInvalidArgument
		case errors.Is(err, analysis.ErrNoSchemaLookup):
			code = connect.CodeFailedPrecondition
		}
		return nil, connect.NewError(code, err)
	}
	return connect.NewResponse(&rpcpb.GetSourceContentResponse{Content: res.Content}), nil
}

// ----------------------------------------------------------------
// ChameleonService — GetChainStatus + GetLayerInfo
// ----------------------------------------------------------------

type rpcChameleonService struct{ srv *Server }

func (s *Server) registerChameleonRPC(mux *http.ServeMux) {
	path, h := rpcpbconnect.NewChameleonServiceHandler(
		&rpcChameleonService{srv: s},
		s.rpcServiceInterceptors(),
	)
	mux.Handle(path, h)
}

func (h *rpcChameleonService) GetChainStatus(
	_ context.Context,
	_ *connect.Request[rpcpb.GetChainStatusRequest],
) (*connect.Response[rpcpb.GetChainStatusResponse], error) {
	res := h.srv.ChainStatusSnapshot()
	envs := make([]*rpcpb.ChainEnvironment, 0, len(res.Environments))
	for _, e := range res.Environments {
		envs = append(envs, &rpcpb.ChainEnvironment{
			Name:      e.Name,
			Ancestors: e.Ancestors,
			KeyName:   e.KeyName,
		})
	}
	errs := make([]*rpcpb.ChainError, 0, len(res.Errors))
	for _, e := range res.Errors {
		errs = append(errs, &rpcpb.ChainError{
			Message: e.Message,
			Line:    int32(e.Line),
			Column:  int32(e.Column),
		})
	}
	return connect.NewResponse(&rpcpb.GetChainStatusResponse{
		Configured:         res.Configured,
		ChainSpecPath:      res.ChainSpecPath,
		Environments:       envs,
		SchemaFqn:          res.SchemaFQN,
		Errors:             errs,
		Version:            res.Version,
		DefaultEnvironment: res.DefaultEnvironment,
		ActiveEnvironment:  res.ActiveEnvironment,
	}), nil
}

func (h *rpcChameleonService) SetActiveEnv(
	_ context.Context,
	req *connect.Request[rpcpb.SetActiveEnvRequest],
) (*connect.Response[rpcpb.SetActiveEnvResponse], error) {
	prev, err := h.srv.SetActiveEnvironment(req.Msg.GetEnvironment())
	if err != nil {
		// InvalidArgument matches the semantics: client sent an env
		// that doesn't exist in the chain spec (or asked to set one
		// when no chain is configured). The validation layer is
		// authoritative — the client should refetch ChainStatus to
		// see the legal env set.
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&rpcpb.SetActiveEnvResponse{
		PreviousEnvironment: prev,
	}), nil
}

func (h *rpcChameleonService) SetEnvironmentKeyName(
	_ context.Context,
	req *connect.Request[rpcpb.SetEnvironmentKeyNameRequest],
) (*connect.Response[rpcpb.SetEnvironmentKeyNameResponse], error) {
	envName := req.Msg.GetEnvironment()
	if envName == "" {
		return nil, connect.NewError(
			connect.CodeInvalidArgument,
			fmt.Errorf("environment is required"),
		)
	}
	specPath := h.srv.ResolvedChainSpecPath()
	if specPath == "" {
		return nil, connect.NewError(
			connect.CodeFailedPrecondition,
			fmt.Errorf("no chameleon chain configured for this workspace"),
		)
	}
	doc, err := chain.LoadDocument(specPath)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := chain.SetEnvironmentKeyName(doc, envName, req.Msg.GetKeyName()); err != nil {
		// Environment-not-found maps to InvalidArgument so the
		// client can prompt the user to add the env first;
		// anything else surfaces as Internal — these are
		// document-shape / disk-write failures the user can't
		// directly fix.
		if errors.Is(err, chain.ErrEnvironmentNotFound) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := chain.WriteDocument(specPath, doc); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// LoadWorkspaceChain rebuilds Server.chain from disk so the
	// next GetChainStatus reflects the edit without waiting on the
	// watched-files debouncer. Synchronous because the caller
	// expects to be able to refresh status immediately and see the
	// new value.
	h.srv.LoadWorkspaceChain()
	return connect.NewResponse(&rpcpb.SetEnvironmentKeyNameResponse{
		KeyName: req.Msg.GetKeyName(),
	}), nil
}

func (h *rpcChameleonService) GetLayerInfo(
	_ context.Context,
	req *connect.Request[rpcpb.GetLayerInfoRequest],
) (*connect.Response[rpcpb.GetLayerInfoResponse], error) {
	// protocol.DocumentURI is an alias for uri.URI; the cast tags
	// the string with the right type for ComputeLayerInfo.
	res := h.srv.ComputeLayerInfo(uri.URI(req.Msg.TextDocument.GetUri()))
	if res == nil {
		return connect.NewResponse(&rpcpb.GetLayerInfoResponse{}), nil
	}
	return connect.NewResponse(&rpcpb.GetLayerInfoResponse{
		IsLayer:     res.IsLayer,
		Environment: res.Environment,
		Ancestors:   res.Ancestors,
	}), nil
}

// ----------------------------------------------------------------
// SecretsService.GetRanges
// ----------------------------------------------------------------

type rpcSecretsService struct{ srv *Server }

func (s *Server) registerSecretsRPC(mux *http.ServeMux) {
	path, h := rpcpbconnect.NewSecretsServiceHandler(
		&rpcSecretsService{srv: s},
		s.rpcServiceInterceptors(),
	)
	mux.Handle(path, h)
}

func (h *rpcSecretsService) GetRanges(
	_ context.Context,
	req *connect.Request[rpcpb.SecretsServiceGetRangesRequest],
) (*connect.Response[rpcpb.SecretsServiceGetRangesResponse], error) {
	docURI := uri.URI(req.Msg.TextDocument.GetUri())
	doc := h.srv.Documents().Get(docURI)
	if doc == nil {
		return connect.NewResponse(&rpcpb.SecretsServiceGetRangesResponse{}), nil
	}
	doc.RLock()
	entries := secretRangesToProto(doc.SecretRanges)
	doc.RUnlock()
	return connect.NewResponse(&rpcpb.SecretsServiceGetRangesResponse{Ranges: entries}), nil
}

// Secrets.Subscribe is implemented in rpc_secrets_subscribe.go —
// keeping the streaming-state plumbing in its own file so the
// per-Server registry + publish helpers stay grouped together.

// ----------------------------------------------------------------
// DatasetsService.GetRegions
// ----------------------------------------------------------------

type rpcDatasetsService struct{ srv *Server }

func (s *Server) registerDatasetsRPC(mux *http.ServeMux) {
	path, h := rpcpbconnect.NewDatasetsServiceHandler(
		&rpcDatasetsService{srv: s},
		s.rpcServiceInterceptors(),
	)
	mux.Handle(path, h)
}

func (h *rpcDatasetsService) GetRegions(
	_ context.Context,
	req *connect.Request[rpcpb.DatasetsServiceGetRegionsRequest],
) (*connect.Response[rpcpb.DatasetsServiceGetRegionsResponse], error) {
	docURI := uri.URI(req.Msg.TextDocument.GetUri())
	doc := h.srv.Documents().Get(docURI)
	if doc == nil {
		return connect.NewResponse(&rpcpb.DatasetsServiceGetRegionsResponse{}), nil
	}
	doc.RLock()
	regions := datasetRegionsToProto(doc.DatasetRegions)
	doc.RUnlock()
	return connect.NewResponse(&rpcpb.DatasetsServiceGetRegionsResponse{Regions: regions}), nil
}

// Datasets.Subscribe is implemented in rpc_datasets_subscribe.go.

// ----------------------------------------------------------------
// EffectiveConfigService
// ----------------------------------------------------------------

type rpcEffectiveConfigService struct{ srv *Server }

func (s *Server) registerEffectiveConfigRPC(mux *http.ServeMux) {
	path, h := rpcpbconnect.NewEffectiveConfigServiceHandler(
		&rpcEffectiveConfigService{srv: s},
		s.rpcServiceInterceptors(),
	)
	mux.Handle(path, h)
}

func (h *rpcEffectiveConfigService) GetEffectiveConfig(
	_ context.Context,
	req *connect.Request[rpcpb.GetEffectiveConfigRequest],
) (*connect.Response[rpcpb.GetEffectiveConfigResponse], error) {
	sections := req.Msg.Sections
	if len(sections) == 0 {
		sections = KnownSections
	}
	values := make(map[string]*rpcpb.ConfigValue, len(sections))
	for _, section := range sections {
		v := h.srv.ResolveSection(section)
		if !req.Msg.RevealSecrets && redact.IsSensitiveSection(section) && v.Value != "" && v.Value != nil {
			v.Value = redact.Placeholder
		}
		values[section] = configValueToProto(v)
	}
	return connect.NewResponse(&rpcpb.GetEffectiveConfigResponse{
		WorkspaceDir: req.Msg.WorkspaceDir,
		Values:       values,
	}), nil
}

func (h *rpcEffectiveConfigService) SetOverrides(
	ctx context.Context,
	req *connect.Request[rpcpb.SetOverridesRequest],
) (*connect.Response[rpcpb.SetOverridesResponse], error) {
	// Convert the typed proto overrides map into the loose
	// map[string]any the existing applyOverrides path expects.
	// nil case (empty oneof) → clear; populated case → set value.
	overrides := map[string]any{}
	for section, v := range req.Msg.Overrides {
		if v == nil || v.GetValue() == nil {
			overrides[section] = nil
			continue
		}
		switch tv := v.GetValue().(type) {
		case *rpcpb.ConfigSectionValue_StringValue:
			overrides[section] = tv.StringValue
		case *rpcpb.ConfigSectionValue_BoolValue:
			overrides[section] = tv.BoolValue
		case *rpcpb.ConfigSectionValue_StringListValue:
			if tv.StringListValue == nil {
				overrides[section] = []string{}
			} else {
				overrides[section] = tv.StringListValue.Values
			}
		}
	}
	if err := ValidateOverrides(overrides); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	sections := SectionsOf(overrides)
	h.srv.Config().LockOverrides()
	before := h.srv.SnapshotSectionsLocked(sections)
	h.srv.ApplyOverridesLocked(overrides)
	after := h.srv.SnapshotSectionsLocked(sections)
	h.srv.Config().UnlockOverrides()
	applied := make(map[string]*rpcpb.AppliedOverride, len(overrides))
	for section := range overrides {
		applied[section] = &rpcpb.AppliedOverride{
			PreviousSource: configSourceToProto(before[section].Source),
			NewValue:       configValueOnlyToProto(after[section]),
		}
	}
	changes := DiffSnapshots(before, after)
	if len(changes) > 0 {
		h.srv.NotifyEffectiveConfigChanged(ctx, req.Msg.WorkspaceDir, changes)
	}
	return connect.NewResponse(&rpcpb.SetOverridesResponse{Applied: applied}), nil
}

// EffectiveConfig.Subscribe is implemented in
// rpc_effective_config_subscribe.go.

// ----------------------------------------------------------------
// helpers
// ----------------------------------------------------------------

func secretRangesToProto(in []SecretRange) []*rpcpb.SecretRangeEntry {
	out := make([]*rpcpb.SecretRangeEntry, 0, len(in))
	for _, r := range in {
		out = append(out, &rpcpb.SecretRangeEntry{
			Range: &rpcpb.Range{
				Start: &rpcpb.Position{Line: uint32(r.Range.Start.Line), Character: uint32(r.Range.Start.Character)},
				End:   &rpcpb.Position{Line: uint32(r.Range.End.Line), Character: uint32(r.Range.End.Character)},
			},
			FieldName: r.Path,
			Form:      secretFormToProto(r.Form),
		})
	}
	return out
}

func secretFormToProto(f SecretForm) rpcpb.SecretForm {
	if f == SecretFormBlock {
		return rpcpb.SecretForm_SECRET_FORM_BLOCK
	}
	return rpcpb.SecretForm_SECRET_FORM_SCALAR
}

func datasetRegionsToProto(in []DatasetRegion) []*rpcpb.DatasetRegion {
	out := make([]*rpcpb.DatasetRegion, 0, len(in))
	for _, r := range in {
		out = append(out, &rpcpb.DatasetRegion{
			Range: &rpcpb.Range{
				Start: &rpcpb.Position{Line: uint32(r.Range.Start.Line), Character: uint32(r.Range.Start.Character)},
				End:   &rpcpb.Position{Line: uint32(r.Range.End.Line), Character: uint32(r.Range.End.Character)},
			},
			TypeFqn:    r.TypeFQN,
			Resolvable: r.Resolvable,
			RowCount:   int32(r.RowCount),
			ByteSize:   int32(r.ByteSize),
			Huge:       r.Huge,
		})
	}
	return out
}

func configValueToProto(v ConfigValue) *rpcpb.ConfigValue {
	return &rpcpb.ConfigValue{
		Value:  configValueInnerToProto(v.Value),
		Source: configSourceToProto(v.Source),
		Origin: v.Origin,
	}
}

func configValueOnlyToProto(v ConfigValue) *rpcpb.ConfigSectionValue {
	return configValueInnerToProto(v.Value)
}

func configValueInnerToProto(v any) *rpcpb.ConfigSectionValue {
	switch tv := v.(type) {
	case string:
		return &rpcpb.ConfigSectionValue{Value: &rpcpb.ConfigSectionValue_StringValue{StringValue: tv}}
	case bool:
		return &rpcpb.ConfigSectionValue{Value: &rpcpb.ConfigSectionValue_BoolValue{BoolValue: tv}}
	case []string:
		return &rpcpb.ConfigSectionValue{Value: &rpcpb.ConfigSectionValue_StringListValue{StringListValue: &rpcpb.StringList{Values: tv}}}
	case nil:
		return &rpcpb.ConfigSectionValue{}
	}
	// Defensive fallback: stringify unknown shapes so we never panic.
	return &rpcpb.ConfigSectionValue{Value: &rpcpb.ConfigSectionValue_StringValue{StringValue: fmt.Sprintf("%v", v)}}
}

func configSourceToProto(s ConfigSource) rpcpb.ConfigSource {
	switch s {
	case SourceDefault:
		return rpcpb.ConfigSource_CONFIG_SOURCE_DEFAULT
	case SourceFile:
		return rpcpb.ConfigSource_CONFIG_SOURCE_FILE
	case SourceSettings:
		return rpcpb.ConfigSource_CONFIG_SOURCE_SETTINGS
	case SourceOverride:
		return rpcpb.ConfigSource_CONFIG_SOURCE_OVERRIDE
	}
	return rpcpb.ConfigSource_CONFIG_SOURCE_UNSPECIFIED
}
