//go:build !protocheck

// Community (OSS) build's no-op stub for the Phase 1 runtime
// protocheck validation. Server.protocheck is always nil here
// (see protocheck_runner_stub.go), so runProtocheck would
// short-circuit anyway; the stub exists so diagnostics.go's
// validate() call site compiles in this build.

package analysis

import "google.golang.org/protobuf/proto"

func (s *Workspace) runProtocheck(_ *Document, _ proto.Message) []protocheckFinding {
	return nil
}
