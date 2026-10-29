// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// CacheDir wiring tests for schema.Open. The full staleness
// transitions live in protoregistry/client (own tests own that
// surface); the schema package's responsibility is the wiring
// glue: forwarding cfg.CacheDir to client.WithDiskCache without
// changing the error semantics callers depend on when the cache
// is empty and the registry is unreachable.

package schema

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpen_CacheDirWithUnreachableAddress_ReturnsClearError
// confirms the cache-dir wiring doesn't silently downgrade a
// fatal dial error into a stale-mode success when the cache
// directory contains no usable snapshot. This is the
// "first-ever-open in offline mode" case — there's nothing to
// fall back to, so Open must error rather than hand back an
// empty-but-non-nil resolver.
func TestOpen_CacheDirWithUnreachableAddress_ReturnsClearError(t *testing.T) {
	cacheDir := t.TempDir()
	// Sanity: cacheDir is empty.
	entries, err := os.ReadDir(cacheDir)
	require.NoError(t, err)
	require.Empty(t, entries, "test precondition: cache dir starts empty")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	lookup, closer, err := Open(ctx, Config{
		Address:   "127.0.0.1:1", // refuses immediately
		Namespace: "anything",
		CacheDir:  cacheDir,
	})
	require.Error(t, err,
		"empty CacheDir + unreachable address must surface an error, not a stale resolver")
	assert.Nil(t, lookup)
	assert.Nil(t, closer)
	assert.True(t, strings.Contains(err.Error(), "127.0.0.1:1"),
		"error must still name the failing address even with CacheDir set: %v", err)
}

// TestOpen_CacheDirTriggersFallbackAttempt confirms the
// cache-fallback path actually runs on a dial failure when
// CacheDir is configured. We can't easily produce a populated
// cache without a real registry, but we CAN observe the
// fallback attempt: the error message includes a "cache load
// failed" segment that's absent when CacheDir is empty. That's
// the wiring signal — if a future refactor accidentally drops
// the WithDiskCache option, the cache-load-failed branch never
// runs and the error message changes shape.
func TestOpen_CacheDirTriggersFallbackAttempt(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "lazy-subdir")
	// Don't pre-create — confirm Open tolerates a path that
	// protoregistry/client will create lazily on first persist.

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, errWithCache := Open(ctx, Config{
		Address:   "127.0.0.1:1",
		Namespace: "anything",
		CacheDir:  cacheDir,
	})
	require.Error(t, errWithCache)

	_, _, errNoCache := Open(ctx, Config{
		Address:   "127.0.0.1:1",
		Namespace: "anything",
	})
	require.Error(t, errNoCache)

	// With CacheDir set, the error chain includes the
	// cache-load failure (proves WithDiskCache was wired in).
	// Without CacheDir, the chain has no cache-load segment.
	assert.True(t, strings.Contains(errWithCache.Error(), "cache load failed"),
		"CacheDir must trigger an upstream cache-fallback attempt; got: %v", errWithCache)
	assert.False(t, strings.Contains(errNoCache.Error(), "cache load failed"),
		"no CacheDir → no cache-fallback attempt expected; got: %v", errNoCache)
}
