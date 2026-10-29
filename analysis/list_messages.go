// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package analysis

import (
	"context"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// MethodListMessages is the custom LSP request that enumerates every
// message type visible to the bound resolver. Wraps
// [schema.Lookup.RangeMessages] (and pairs each result with its
// contributing namespace via FindFileByPathWithOrigin).
//
// Pxfed's schema browser, the future hosted Monaco IDE, and any
// editor that wants a "browse all types" affordance all consume
// this — the alternative would be each editor opening its own
// protoregistry gRPC connection, duplicating auth + namespace-chain
// state. Going through protolsp keeps that state in one place.
//
// Request:  { "prefix": "<optional FQN prefix>" }
// Response: { "messages": [{ "fullName", "file", "namespace" }, ...] }
const MethodListMessages = "protolsp/listMessages"

// ListMessagesParams is the request shape for [MethodListMessages].
// Prefix is an optional filter — when non-empty, only message types
// whose FQN starts with it are returned. Useful for client-side
// search-as-you-type without re-fetching the full list each
// keystroke.
type ListMessagesParams struct {
	Prefix string `json:"prefix,omitempty"`
}

// MessageEntry describes one message type in a ListMessages response.
// Namespace is the registry namespace that contributed the type
// (per FindFileByPathWithOrigin) — empty when the type came from an
// ad-hoc parent whose identity isn't tracked.
type MessageEntry struct {
	FullName  string `json:"fullName"`
	File      string `json:"file"`
	Namespace string `json:"namespace,omitempty"`
}

// ListMessagesResult is the response shape for [MethodListMessages].
// Entries are sorted by FullName for deterministic UI rendering and
// deduped — RangeMessages can yield the same FQN twice when a child
// namespace re-exports a parent's type, but clients only want it
// once.
type ListMessagesResult struct {
	Messages []MessageEntry `json:"messages"`
}

// ListMessages enumerates every message type visible to the bound
// resolver, optionally filtered to FQNs starting with prefix:
// deduped, origin-attributed (namespace per
// FindFileByPathWithOrigin, memoized per file), and sorted by
// FullName. When no lookup is configured the result is an empty
// list, not an error — schema-aware browsing simply isn't
// available. The composed recipe behind [MethodListMessages] and
// the RPC sidecar's ListMessages; exported so in-process embedders
// share it (#203).
//
// Long namespace chains can hold tens of thousands of types, so the
// walk respects ctx: on cancellation the partial result is dropped
// and ctx's error is returned.
func (s *Workspace) ListMessages(ctx context.Context, prefix string) (ListMessagesResult, error) {
	out := make([]MessageEntry, 0, 64)
	lk := s.lookupMgr.Snapshot()
	if lk == nil {
		return ListMessagesResult{Messages: out}, nil
	}

	seen := make(map[protoreflect.FullName]bool)
	originByFile := make(map[string]string) // memoize FindFileByPathWithOrigin
	lk.RangeMessages(func(mt protoreflect.MessageType) bool {
		// Returning false stops the iterator.
		if ctx.Err() != nil {
			return false
		}
		fn := mt.Descriptor().FullName()
		if seen[fn] {
			return true
		}
		if prefix != "" && !strings.HasPrefix(string(fn), prefix) {
			return true
		}
		seen[fn] = true

		filePath := mt.Descriptor().ParentFile().Path()
		origin, cached := originByFile[filePath]
		if !cached {
			if _, ns, err := lk.FindFileByPathWithOrigin(filePath); err == nil {
				origin = ns
			}
			originByFile[filePath] = origin
		}
		out = append(out, MessageEntry{
			FullName:  string(fn),
			File:      filePath,
			Namespace: origin,
		})
		return true
	})
	if err := ctx.Err(); err != nil {
		return ListMessagesResult{}, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	return ListMessagesResult{Messages: out}, nil
}
