// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package server

import (
	"runtime/debug"
	"strings"
)

// develVersion is reported when the binary carries no module version:
// Go's own spelling, for a build outside any VCS checkout or a test
// binary.
const develVersion = "(devel)"

// serverVersion is the version initialize reports in serverInfo. The
// release process stamps nothing into the binary, it only tags, so the
// version is the one the Go toolchain records at build time: the tag for
// `go install ...@v0.24.0` or a build of a tagged checkout, a
// pseudo-version for anything between tags.
func serverVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return develVersion
	}
	return moduleVersion(info)
}

// moduleVersion is the main module's version without its leading "v",
// the form clients have always seen in serverInfo.
func moduleVersion(info *debug.BuildInfo) string {
	if info.Main.Version == "" {
		return develVersion
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}
