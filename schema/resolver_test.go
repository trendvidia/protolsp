// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package schema

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_EmptyAddressNoOps(t *testing.T) {
	lookup, closer, err := Open(context.Background(), Config{})
	require.NoError(t, err)
	assert.Nil(t, lookup, "no address → nil lookup (schema validation disabled)")
	assert.Nil(t, closer)
}

// TestValidate_PartialConfigRejected is TC1 from the audit
// roadmap: the two Validate error branches (namespace without
// address, address without namespace) were 0% covered.
func TestValidate_PartialConfigRejected(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		errSub string
	}{
		{"empty is valid", Config{}, ""},
		{"both set is valid", Config{Address: "x:443", Namespace: "ns"}, ""},
		{
			"namespace without address",
			Config{Namespace: "ns"},
			"namespace set but address is empty",
		},
		{
			"address without namespace",
			Config{Address: "x:443"},
			"address set but namespace is empty",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if c.errSub == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.errSub,
				"error message must name the missing field so users can fix it")
		})
	}
}

func TestOpen_DialErrorWrapsAddress(t *testing.T) {
	// 127.0.0.1:1 should refuse immediately on all reasonable
	// hosts; if not, the DialTimeout (30s) is the upper bound.
	// Use a shorter deadline on the context so this test never
	// blocks the suite for that long.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lookup, closer, err := Open(ctx, Config{
		Address:   "127.0.0.1:1",
		Namespace: "anything",
	})
	require.Error(t, err)
	assert.Nil(t, lookup)
	assert.Nil(t, closer)
	assert.True(t, strings.Contains(err.Error(), "127.0.0.1:1"),
		"error must include the address so users see what failed: %v", err)
}

func TestOpen_RespectsCallerCancellation(t *testing.T) {
	// A caller-cancelled context surfaces as the dial error;
	// confirms the timeout wrapper doesn't swallow the outer
	// deadline.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately
	_, _, err := Open(ctx, Config{
		Address:   "127.0.0.1:1",
		Namespace: "anything",
	})
	require.Error(t, err)
}
