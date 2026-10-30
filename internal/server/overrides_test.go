// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/jsonrpc2"

	"github.com/trendvidia/protolsp/schema"
)

// setOverridesReply drives MethodSetOverrides and returns the
// decoded result or the JSON-RPC error.
func setOverridesReply(t *testing.T, srv *Server, params SetOverridesParams) (*SetOverridesResult, *jsonrpc2.Error) {
	t.Helper()
	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodSetOverrides, params)
	require.NoError(t, err)

	var got *SetOverridesResult
	var gotErr *jsonrpc2.Error
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		if replyErr != nil {
			var jsErr *jsonrpc2.Error
			require.ErrorAs(t, replyErr, &jsErr)
			gotErr = jsErr
			return nil
		}
		b, err := json.Marshal(result)
		require.NoError(t, err)
		var r SetOverridesResult
		require.NoError(t, json.Unmarshal(b, &r))
		got = &r
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	return got, gotErr
}

func TestSetOverrides_AppliesNewValue(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	srv.Config().EditorConfig.Registry = schema.Config{Namespace: "from-settings"}

	got, errResp := setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{
			"protolsp.registry.namespace": "from-override",
		},
	})
	require.Nil(t, errResp)
	require.NotNil(t, got)

	applied := got.Applied["protolsp.registry.namespace"]
	assert.Equal(t, "from-override", applied.NewValue)
	assert.Equal(t, SourceSettings, applied.PreviousSource,
		"previousSource reflects the tier that was supplying the value")

	// getEffectiveConfig now returns the override.
	eff := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{"protolsp.registry.namespace"},
	})
	ns := eff.Values["protolsp.registry.namespace"]
	assert.Equal(t, "from-override", ns.Value)
	assert.Equal(t, SourceOverride, ns.Source)
	assert.Equal(t, "session", ns.Origin)
}

func TestSetOverrides_NullValueClears(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)
	srv.Config().EditorConfig.Registry = schema.Config{Namespace: "from-settings"}

	// Set an override.
	_, _ = setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{"protolsp.registry.namespace": "from-override"},
	})

	// Clear it with null.
	got, errResp := setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{"protolsp.registry.namespace": nil},
	})
	require.Nil(t, errResp)
	require.NotNil(t, got)

	applied := got.Applied["protolsp.registry.namespace"]
	assert.Equal(t, SourceOverride, applied.PreviousSource,
		"previousSource was override; clearing lowers it")
	assert.Equal(t, "from-settings", applied.NewValue,
		"NewValue reflects the settings tier that took over after clear")

	// Verify resolution falls back to settings.
	eff := effectiveConfigReply(t, srv, GetEffectiveConfigParams{
		Sections: []string{"protolsp.registry.namespace"},
	})
	ns := eff.Values["protolsp.registry.namespace"]
	assert.Equal(t, SourceSettings, ns.Source,
		"after clearing override, source is settings again")
}

func TestSetOverrides_RejectsUnknownSection(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)

	got, errResp := setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{"protolsp.unknown.section": "x"},
	})
	assert.Nil(t, got)
	require.NotNil(t, errResp)
	assert.Equal(t, jsonrpc2.InvalidParams, errResp.Code)
	assert.Contains(t, errResp.Message, "unknown section",
		"error message names the offending section")
}

func TestSetOverrides_RejectsWrongType(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)

	// All current sections are strings. Pass an int.
	got, errResp := setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{"protolsp.registry.address": 42},
	})
	assert.Nil(t, got)
	require.NotNil(t, errResp)
	assert.Equal(t, jsonrpc2.InvalidParams, errResp.Code)
	assert.Contains(t, errResp.Message, "string")
}

func TestSetOverrides_EmitsEffectiveConfigChangedNotification(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)
	srv.Config().EditorConfig.Registry = schema.Config{Namespace: "from-settings"}

	_, _ = setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{
			"protolsp.registry.namespace": "from-override",
		},
	})

	notes := fc.configChangedNotifications()
	require.Len(t, notes, 1, "applying an override emits exactly one notification")
	assert.Contains(t, notes[0].Changes, "protolsp.registry.namespace",
		"notification carries the changed section")

	change := notes[0].Changes["protolsp.registry.namespace"]
	assert.Equal(t, "from-override", change.Value)
	assert.Equal(t, SourceOverride, change.Source)
}

func TestSetOverrides_NoNotificationWhenNoEffectiveChange(t *testing.T) {
	// Overriding to the same effective value (with the same source)
	// shouldn't generate notification traffic. We achieve this by
	// re-applying the same override twice.
	srv, fc := newServerWithFakeConn(t)

	// First application — settings → override transition; expect a
	// notification.
	_, _ = setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{"protolsp.registry.address": "x:443"},
	})
	require.Len(t, fc.configChangedNotifications(), 1,
		"first override emits notification")

	// Second application of the same value — no effective change.
	_, _ = setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{"protolsp.registry.address": "x:443"},
	})
	assert.Len(t, fc.configChangedNotifications(), 1,
		"second identical override emits no additional notification")
}

func TestSetOverrides_TokenRedactedInNotification(t *testing.T) {
	srv, fc := newServerWithFakeConn(t)

	_, _ = setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{"protolsp.registry.token": "super-secret"},
	})

	notes := fc.configChangedNotifications()
	require.Len(t, notes, 1)
	tok := notes[0].Changes["protolsp.registry.token"]
	assert.Equal(t, "<redacted>", tok.Value,
		"sensitive sections redact in the notification too")
	assert.Equal(t, SourceOverride, tok.Source,
		"source preserved through redaction")
}

func TestSetOverrides_NilParamsTolerated(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)

	call, err := newTestCall(jsonrpc2.NewNumberID(1), MethodSetOverrides, nil)
	require.NoError(t, err)
	var got *SetOverridesResult
	reply := func(_ context.Context, result interface{}, replyErr error) error {
		require.NoError(t, replyErr)
		b, _ := json.Marshal(result)
		var r SetOverridesResult
		_ = json.Unmarshal(b, &r)
		got = &r
		return nil
	}
	require.NoError(t, testHandle(srv, context.Background(), reply, call))
	require.NotNil(t, got)
	assert.Empty(t, got.Applied, "null params → empty applied map, no error")
}

func TestSetOverrides_MultipleSectionsInOneCall(t *testing.T) {
	srv, _ := newServerWithFakeConn(t)

	got, errResp := setOverridesReply(t, srv, SetOverridesParams{
		Overrides: map[string]any{
			"protolsp.registry.address":   "addr:443",
			"protolsp.registry.namespace": "ns",
		},
	})
	require.Nil(t, errResp)
	require.NotNil(t, got)

	assert.Len(t, got.Applied, 2)
	assert.Equal(t, "addr:443", got.Applied["protolsp.registry.address"].NewValue)
	assert.Equal(t, "ns", got.Applied["protolsp.registry.namespace"].NewValue)
}
