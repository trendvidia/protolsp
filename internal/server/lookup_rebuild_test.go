// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.lsp.dev/uri"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/trendvidia/protolsp/schema"
)

// instrumentedLookup is a schema.Lookup test double the rebuild
// tests use to track close + observe IsStale toggling. The
// underlying responses are stubbed; real schema.Open dial
// behavior is exercised via the registry client elsewhere.
type instrumentedLookup struct {
	id       string
	stale    bool
	closed   atomic.Bool
	closeErr error
}

func (l *instrumentedLookup) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	return nil, protoregistry.NotFound
}
func (l *instrumentedLookup) RangeMessages(_ func(protoreflect.MessageType) bool) {}
func (l *instrumentedLookup) FindFileByPathWithOrigin(_ string) (protoreflect.FileDescriptor, string, error) {
	return nil, "", protoregistry.NotFound
}
func (l *instrumentedLookup) GetSource(_ context.Context, _ string) ([]byte, error) {
	return nil, protoregistry.NotFound
}
func (l *instrumentedLookup) IsStale() bool { return l.stale }
func (l *instrumentedLookup) Close() error {
	l.closed.Store(true)
	return l.closeErr
}

var _ schema.Lookup = (*instrumentedLookup)(nil)
var _ io.Closer = (*instrumentedLookup)(nil)

func TestSwapLookup_ReturnsOldCloserForCleanup(t *testing.T) {
	srv := New(discardLogger())
	first := &instrumentedLookup{id: "first"}
	second := &instrumentedLookup{id: "second"}

	// Initial swap-in.
	oldOnFirstInstall := srv.Lookups().Swap(first, first)
	assert.Nil(t, oldOnFirstInstall,
		"no prior state → no closer returned")

	// Second swap returns the first as the old closer to clean up.
	oldOnSecondInstall := srv.Lookups().Swap(second, second)
	require.NotNil(t, oldOnSecondInstall)
	assert.Same(t, first, oldOnSecondInstall,
		"swapLookup returns the prior closer so the caller can close it")
	// Server now reports the second lookup.
	assert.Same(t, schema.Lookup(second), srv.Lookups().Snapshot())
}

func TestRebuildLookupAsync_DiffSkipsWhenConfigUnchanged(t *testing.T) {
	// Two consecutive rebuilds with the same effective config.
	// The second one should be a no-op — no dial, no state churn.
	srv := New(discardLogger())
	first := &instrumentedLookup{id: "v1"}
	srv.Lookups().Swap(first, first)
	srv.Lookups().RecordBaseline(schema.Config{Address: "registry.example:443"})
	// Set the same effective config so the diff trips.
	srv.Config().EditorConfig.Registry = schema.Config{Address: "registry.example:443"}

	// Trigger a "rebuild" — diff catches it, no goroutine, no swap.
	srv.RebuildLookupAsync(context.Background())

	// Lookup is still the first instance; not closed.
	assert.Same(t, schema.Lookup(first), srv.Lookups().Snapshot())
	assert.False(t, first.closed.Load(),
		"diff-skip → previous lookup not closed")
}

func TestRebuildLookupAsync_EmptyAddressTearsDownSynchronously(t *testing.T) {
	srv := New(discardLogger())
	prior := &instrumentedLookup{id: "prior"}
	srv.Lookups().Swap(prior, prior)
	srv.Lookups().RecordBaseline(schema.Config{Address: "old:443"})

	// Empty Address → effective config switches to "no registry"
	// → teardown happens synchronously.
	srv.Config().EditorConfig.Registry = schema.Config{}

	srv.RebuildLookupAsync(context.Background())

	assert.Nil(t, srv.Lookups().Snapshot(),
		"empty Address tears down the prior lookup")
	assert.True(t, prior.closed.Load(),
		"prior lookup's closer is invoked during teardown")
}

func TestRebuildLookupAsync_NonEmptyAddressReturnsImmediately(t *testing.T) {
	// The async path schedules schema.Open in a goroutine. Even when
	// Open would block forever, RebuildLookupAsync returns at once.
	// We can't easily intercept schema.Open from outside the
	// package, so this test asserts the timing: RebuildLookupAsync
	// completes much faster than a realistic dial would.
	srv := New(discardLogger())
	srv.Config().EditorConfig.Registry = schema.Config{
		// Bogus address — dialing will fail fast, but the failure
		// happens in the goroutine, not in our call.
		Address:   "unreachable.example.invalid:443",
		Namespace: "test",
	}

	start := time.Now()
	srv.RebuildLookupAsync(context.Background())
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 50*time.Millisecond,
		"RebuildLookupAsync must return promptly — the actual dial runs in a goroutine")

	// Cancel anything in flight + clean up.
	srv.Lookups().CancelInFlight()
}

func TestRebuildLookupAsync_NewRequestCancelsInFlight(t *testing.T) {
	// Kick off two rebuilds back-to-back with DIFFERENT configs.
	// The second cancels the first. Internal rebuildCancel state
	// isn't observable from outside the manager, so we test the
	// externally-visible contract: both calls return promptly,
	// the manager's last-baseline reflects the SECOND cfg, and
	// CancelInFlight runs cleanly afterward.
	srv := New(discardLogger())
	srv.Config().EditorConfig.Registry = schema.Config{
		Address: "first.invalid:443", Namespace: "a",
	}
	srv.RebuildLookupAsync(context.Background())

	// Second rebuild with a different config. BeginRebuild
	// internally cancels the first and installs a fresh cancel.
	srv.Config().EditorConfig.Registry = schema.Config{
		Address: "second.invalid:443", Namespace: "b",
	}
	srv.RebuildLookupAsync(context.Background())

	// A THIRD rebuild with the SECOND cfg must short-circuit at
	// the diff check — proves the manager recorded the second
	// cfg as the new baseline. If it hadn't (the cancel-vs-record
	// ordering inside BeginRebuild was wrong), this would fire a
	// fresh dial and the cancel chain would be inconsistent.
	cfg := srv.EffectiveRegistryConfig()
	_, proceed := srv.Lookups().BeginRebuild(cfg)
	assert.False(t, proceed,
		"after two rebuilds, a third with the same cfg must diff-skip")

	srv.Lookups().CancelInFlight()
}

func TestRebuildLookupAsync_ConcurrentReadsDuringSwapAreSafe(t *testing.T) {
	// Race-detector test: spawn many readers and a swapper, run
	// briefly, no race report. Sanity-checks that snapshotLookup +
	// swapLookup are correctly synchronized.
	srv := New(discardLogger())
	initial := &instrumentedLookup{id: "init"}
	srv.Lookups().Swap(initial, initial)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = srv.Lookups().Snapshot()
				}
			}
		}()
	}

	// Swapper.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				next := &instrumentedLookup{id: "x"}
				if old := srv.Lookups().Swap(next, next); old != nil {
					_ = old.Close()
				}
				if i > 100 {
					return
				}
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestRebuildLookupAsync_LiveReloadOnConfigEditDoesntBlockHandler(t *testing.T) {
	// End-to-end: open .protolsp.pxf, edit the chameleon block via
	// didChange. The registry address didn't change → no dial
	// should fire. The handler returns promptly.
	srv, _ := initializeWithWorkspace(t, t.TempDir(), nil)

	configURI := uri.URI("file:///work/.protolsp.pxf")
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didOpenCall(t, string(configURI), 1, `@type protolsp.v1.WorkspaceConfig
registry { address = "stable.example:443" }
chameleon { chain_spec = "v1.pxf" }
`)))

	// Edit ONLY the chameleon block. Registry config unchanged.
	const edited = `@type protolsp.v1.WorkspaceConfig
registry { address = "stable.example:443" }
chameleon { chain_spec = "v2.pxf" }
`
	start := time.Now()
	require.NoError(t, testHandle(srv, context.Background(), noopReply,
		didChangeCall(t, string(configURI), 2, edited)))
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 100*time.Millisecond,
		"editing the chameleon block (registry unchanged) doesn't trigger a dial; handler returns immediately")
}
