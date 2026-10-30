// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import "go.lsp.dev/protocol"

func optText(o protocol.Optional[string]) string {
	v, _ := o.Get()
	return v
}
