// Tag-free tests for the protocheck validator lifecycle: swap
// semantics (old validator closed exactly once, after the swap),
// Workspace.Close teardown, and the protocheckValidate accessor
// under concurrent swaps (run with -race). The fake validator keeps
// these independent of the real engine, so they run in both
// the OSS and the protocheck-tagged suites.

package analysis

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type fakeProtocheckValidator struct {
	err error
}

func (f *fakeProtocheckValidator) Validate(proto.Message) error { return f.err }

func TestSwapProtocheck_ClosesDisplacedValidatorOnce(t *testing.T) {
	w := New(slog.Default())
	defer func() { require.NoError(t, w.Close()) }()

	var closed1, closed2 atomic.Int64
	sentinel := errors.New("v1")
	w.swapProtocheck(&fakeProtocheckValidator{err: sentinel}, func() { closed1.Add(1) }, nil, nil)

	err, ok := w.protocheckValidate(nil)
	require.True(t, ok)
	require.ErrorIs(t, err, sentinel, "reads route to the installed validator")
	require.Zero(t, closed1.Load(), "installing must not close the new validator")

	w.swapProtocheck(&fakeProtocheckValidator{}, func() { closed2.Add(1) }, nil, nil)
	require.Equal(t, int64(1), closed1.Load(), "displaced validator closed exactly once")
	require.Zero(t, closed2.Load())

	err, ok = w.protocheckValidate(nil)
	require.True(t, ok)
	require.NoError(t, err, "reads route to the replacement")
}

func TestWorkspaceClose_ClosesCurrentValidator(t *testing.T) {
	w := New(slog.Default())
	var closed atomic.Int64
	w.swapProtocheck(&fakeProtocheckValidator{}, func() { closed.Add(1) }, nil, nil)

	require.NoError(t, w.Close())
	require.Equal(t, int64(1), closed.Load(), "Close releases the current validator")

	_, ok := w.protocheckValidate(nil)
	require.False(t, ok, "no validator installed after Close")
}

func TestProtocheckValidate_NilValidatorShortCircuits(t *testing.T) {
	w := New(slog.Default())
	defer func() { require.NoError(t, w.Close()) }()
	// OSS build: New installs nil. Tagged build: force the OSS
	// shape so the assertion holds under both suites.
	w.swapProtocheck(nil, nil, nil, nil)
	err, ok := w.protocheckValidate(nil)
	require.False(t, ok)
	require.NoError(t, err)
}

// TestProtocheckSwapRace drives concurrent validations against
// concurrent swaps. Run under -race: the RLock-for-the-whole-call
// discipline in protocheckValidate is what makes closing the
// displaced validator after the swap safe.
func TestProtocheckSwapRace(t *testing.T) {
	w := New(slog.Default())
	defer func() { require.NoError(t, w.Close()) }()
	// The tagged build's New installs a real validator, which
	// dereferences the nil message below. Start from a fake so the
	// validate goroutine never sees it, whichever goroutine runs first.
	w.swapProtocheck(&fakeProtocheckValidator{}, nil, nil, nil)

	const iterations = 500
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			w.protocheckValidate(nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			v := &fakeProtocheckValidator{}
			var self atomic.Int64
			w.swapProtocheck(v, func() { self.Add(1) }, nil, nil)
		}
	}()
	wg.Wait()
}

type fakeCatalog struct{ entries map[string]string }

func (f *fakeCatalog) Render(code string, _ map[string]any) (string, bool) {
	m, ok := f.entries[code]
	return m, ok
}

func TestMatchLocaleCatalog(t *testing.T) {
	de := &fakeCatalog{}
	deAT := &fakeCatalog{}
	deCH := &fakeCatalog{}
	pt := &fakeCatalog{}
	cases := []struct {
		name     string
		catalogs map[string]protocheckCatalog
		locale   string
		want     protocheckCatalog
	}{
		{"exact", map[string]protocheckCatalog{"de": de, "pt": pt}, "de", de},
		{"exact case-insensitive", map[string]protocheckCatalog{"de-AT": deAT}, "de-at", deAT},
		{"request base tag wins", map[string]protocheckCatalog{"de": de, "de-AT": deAT}, "de-DE", de},
		{"shared primary subtag", map[string]protocheckCatalog{"de-AT": deAT, "pt": pt}, "de-DE", deAT},
		{"shared primary picks smallest tag", map[string]protocheckCatalog{"de-CH": deCH, "de-AT": deAT}, "de", deAT},
		{"no match", map[string]protocheckCatalog{"pt": pt}, "de", nil},
		{"empty locale", map[string]protocheckCatalog{"de": de}, "", nil},
		{"no catalogs", nil, "de", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchLocaleCatalog(tc.catalogs, tc.locale)
			if tc.want == nil {
				require.False(t, ok)
				return
			}
			require.True(t, ok)
			require.Same(t, tc.want, got)
		})
	}
}

func TestSwapProtocheck_CarriesCatalogs(t *testing.T) {
	w := New(slog.Default())
	defer func() { require.NoError(t, w.Close()) }()
	w.SetClientLocale("de-DE")

	cat := &fakeCatalog{entries: map[string]string{"x": "y"}}
	w.swapProtocheck(&fakeProtocheckValidator{}, nil,
		map[string]protocheckCatalog{"de": cat}, []string{"/cfg/i18n/de.textproto"})

	got, ok := w.protocheckRenderCatalog()
	require.True(t, ok)
	require.Same(t, protocheckCatalog(cat), got)
	require.Equal(t, []string{"/cfg/i18n/de.textproto"}, w.protocheckCatalogPathsSnapshot())

	// A swap without catalogs clears both — no stale rendering
	// against a validator that wasn't built with them.
	w.swapProtocheck(&fakeProtocheckValidator{}, nil, nil, nil)
	_, ok = w.protocheckRenderCatalog()
	require.False(t, ok)
	require.Empty(t, w.protocheckCatalogPathsSnapshot())
}
