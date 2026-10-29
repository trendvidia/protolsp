//go:build !protocheck

// Community (OSS) build's no-op stubs for the protocheck
// integration. newProtocheckValidator returns (nil, nil) so the
// Workspace.protocheck field stays nil and the data-validation pass
// short-circuits in protocheckValidate; rebuildProtocheck makes the
// lifecycle triggers (initialize, config edits, lookup swaps)
// compile tag-free. The real implementations live in
// protocheck_runner_real.go (//go:build protocheck), the only path
// that imports the protocheck/v2 runtime dependencies.

package analysis

import (
	"context"
	"log/slog"
)

func newProtocheckValidator(_ *slog.Logger) (protocheckValidator, func()) { return nil, nil }

func (s *Workspace) rebuildProtocheck(context.Context) {}
