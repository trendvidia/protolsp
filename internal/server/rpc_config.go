// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Connect-RPC implementation of ConfigService — the typed Connect
// surface for `.protolsp.pxf` flat-attribute access. Reuses the same
// load/edit/write helpers that back the LSP-side dispatchConfig*
// methods so behavior is identical regardless of which transport the
// editor speaks.

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"connectrpc.com/connect"

	"github.com/trendvidia/protolsp/analysis"
	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
	"github.com/trendvidia/protolsp/proto/protolsp/v1/rpc/rpcpbconnect"

	"github.com/trendvidia/protolsp/workspaceconfig"
)

// rpcConfigService satisfies rpcpbconnect.ConfigServiceHandler. The
// embedded *Server pointer lets handlers reach the same workspace
// state the LSP dispatchers use.
type rpcConfigService struct {
	srv *Server
}

func (s *Server) registerConfigRPC(mux *http.ServeMux) {
	path, handler := rpcpbconnect.NewConfigServiceHandler(
		&rpcConfigService{srv: s},
		s.rpcServiceInterceptors(),
	)
	mux.Handle(path, handler)
}

func (h *rpcConfigService) ListAttributes(
	_ context.Context,
	_ *connect.Request[rpcpb.ListAttributesRequest],
) (*connect.Response[rpcpb.ListAttributesResponse], error) {
	out := make([]*rpcpb.AttributeSpec, 0, len(workspaceconfig.AttributeRegistry))
	for _, a := range workspaceconfig.AttributeRegistry {
		out = append(out, &rpcpb.AttributeSpec{
			Name:        a.Name,
			DisplayName: a.DisplayName,
			Description: a.Description,
			Kind:        kindToProto(a.Kind),
		})
	}
	return connect.NewResponse(&rpcpb.ListAttributesResponse{Attributes: out}), nil
}

func (h *rpcConfigService) GetAttribute(
	_ context.Context,
	req *connect.Request[rpcpb.GetAttributeRequest],
) (*connect.Response[rpcpb.GetAttributeResponse], error) {
	params := req.Msg
	spec, ok := workspaceconfig.LookupAttribute(params.Name)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unknown attribute %q", params.Name))
	}
	cfgPath, cerr := h.srv.configPathForWorkspaceRPC(params.WorkspaceDir)
	if cerr != nil {
		return nil, cerr
	}
	doc, err := workspaceconfig.LoadDocument(cfgPath)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("loading workspace config: %w", err))
	}
	val, present, err := workspaceconfig.GetAttribute(doc, params.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("reading attribute: %w", err))
	}
	resp := &rpcpb.GetAttributeResponse{
		Name:    params.Name,
		Kind:    kindToProto(spec.Kind),
		Present: present,
	}
	if present {
		value, encErr := goValueToProto(val, spec.Kind)
		if encErr != nil {
			return nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("encoding attribute value: %w", encErr))
		}
		resp.Value = value
	}
	return connect.NewResponse(resp), nil
}

func (h *rpcConfigService) SetAttribute(
	ctx context.Context,
	req *connect.Request[rpcpb.SetAttributeRequest],
) (*connect.Response[rpcpb.SetAttributeResponse], error) {
	params := req.Msg
	spec, ok := workspaceconfig.LookupAttribute(params.Name)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unknown attribute %q", params.Name))
	}
	value, err := protoValueToGo(params.Value, spec.Kind)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("attribute %q: %w", params.Name, err))
	}
	cfgPath, cerr := h.srv.configPathForWorkspaceRPC(params.WorkspaceDir)
	if cerr != nil {
		return nil, cerr
	}
	doc, err := workspaceconfig.LoadDocument(cfgPath)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("loading workspace config: %w", err))
	}
	if err := workspaceconfig.SetAttribute(doc, params.Name, value); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := workspaceconfig.WriteDocument(cfgPath, doc); err != nil {
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("writing workspace config: %w", err))
	}
	h.srv.ReloadAfterConfigEdit(ctx, filepath.Dir(cfgPath))
	return connect.NewResponse(&rpcpb.SetAttributeResponse{Ok: true}), nil
}

// configPathForWorkspaceRPC is the Connect-RPC twin of the existing
// ConfigPathForWorkspace — same logic, but returns a *connect.Error
// (codes instead of JSON-RPC numbers) so handlers can return it
// directly.
func (s *Server) configPathForWorkspaceRPC(workspaceDir string) (string, *connect.Error) {
	cfgPath, err := s.ConfigPathForWorkspace(workspaceDir)
	if err == nil {
		return cfgPath, nil
	}
	if errors.Is(err, analysis.ErrUnknownWorkspaceDir) {
		return "", connect.NewError(connect.CodeInvalidArgument, err)
	}
	return "", connect.NewError(connect.CodeFailedPrecondition, err)
}

// kindToProto maps the workspaceconfig.AttributeKind enum to the wire
// rpcpb.AttributeKind. Centralised so future kinds (int, duration)
// only need one update point.
func kindToProto(k workspaceconfig.AttributeKind) rpcpb.AttributeKind {
	switch k {
	case workspaceconfig.KindString:
		return rpcpb.AttributeKind_ATTRIBUTE_KIND_STRING
	case workspaceconfig.KindStringList:
		return rpcpb.AttributeKind_ATTRIBUTE_KIND_STRING_LIST
	case workspaceconfig.KindBool:
		return rpcpb.AttributeKind_ATTRIBUTE_KIND_BOOL
	}
	return rpcpb.AttributeKind_ATTRIBUTE_KIND_UNSPECIFIED
}

// goValueToProto encodes the in-memory Go value returned by
// workspaceconfig.GetAttribute into the typed AttributeValue oneof
// the wire format expects.
func goValueToProto(v any, k workspaceconfig.AttributeKind) (*rpcpb.AttributeValue, error) {
	switch k {
	case workspaceconfig.KindString:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", v)
		}
		return &rpcpb.AttributeValue{Value: &rpcpb.AttributeValue_StringValue{StringValue: s}}, nil
	case workspaceconfig.KindStringList:
		ss, ok := v.([]string)
		if !ok {
			return nil, fmt.Errorf("expected []string, got %T", v)
		}
		return &rpcpb.AttributeValue{Value: &rpcpb.AttributeValue_StringListValue{StringListValue: &rpcpb.StringList{Values: ss}}}, nil
	case workspaceconfig.KindBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("expected bool, got %T", v)
		}
		return &rpcpb.AttributeValue{Value: &rpcpb.AttributeValue_BoolValue{BoolValue: b}}, nil
	}
	return nil, fmt.Errorf("unsupported kind %v", k)
}

// protoValueToGo decodes the typed AttributeValue oneof into the Go
// shape workspaceconfig.SetAttribute consumes. Mismatched
// oneof case vs. expected kind is a client error — we map it to
// CodeInvalidArgument upstream.
func protoValueToGo(v *rpcpb.AttributeValue, k workspaceconfig.AttributeKind) (any, error) {
	if v == nil {
		return nil, fmt.Errorf("value is required")
	}
	switch k {
	case workspaceconfig.KindString:
		sv, ok := v.GetValue().(*rpcpb.AttributeValue_StringValue)
		if !ok {
			return nil, fmt.Errorf("expected string value")
		}
		return sv.StringValue, nil
	case workspaceconfig.KindStringList:
		lv, ok := v.GetValue().(*rpcpb.AttributeValue_StringListValue)
		if !ok {
			return nil, fmt.Errorf("expected string-list value")
		}
		if lv.StringListValue == nil {
			return []string{}, nil
		}
		return lv.StringListValue.Values, nil
	case workspaceconfig.KindBool:
		bv, ok := v.GetValue().(*rpcpb.AttributeValue_BoolValue)
		if !ok {
			return nil, fmt.Errorf("expected bool value")
		}
		return bv.BoolValue, nil
	}
	return nil, fmt.Errorf("unsupported kind %v", k)
}
