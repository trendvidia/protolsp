#!/bin/sh
# Runs protolsp's protocheck end-to-end tests with the GoVM backend.
#
# Those tests are in package server and lean on its test helpers, so
# they stay there, behind the protocheck tag, and import nothing
# private. The one thing they need is a registered backend, and
# protolsp's module can't import this one. So the overlay adds that
# import to the package for this run only. Arguments go to go test.
set -eu
cd "$(dirname "$0")"
root=$(cd .. && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/backend_test.go" <<'GO'
package server

import _ "github.com/trendvidia/protolsp/govm"
GO
cat >"$tmp/overlay.json" <<JSON
{"Replace": {"$root/internal/server/zz_govm_backend_test.go": "$tmp/backend_test.go"}}
JSON
go test -tags protocheck -overlay "$tmp/overlay.json" "$@" github.com/trendvidia/protolsp/internal/server
