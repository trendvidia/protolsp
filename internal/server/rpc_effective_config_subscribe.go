// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Server-side state for EffectiveConfigService.Subscribe. Streams
// per-section changes whenever the resolved value of any config
// section shifts — file edits, settings pulls, override applies.
// Unlike Secrets/Datasets, this one is WORKSPACE-scoped: a single
// workspaceDir produces one stream of events, not per-document.

package server

import (
	"context"
	"sync"

	"connectrpc.com/connect"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
)

type effectiveConfigSubscriber struct {
	workspaceDir string
	ch           chan *rpcpb.EffectiveConfigServiceSubscribeResponse
}

type effectiveConfigSubscribers struct {
	mu  sync.Mutex
	all map[string]map[*effectiveConfigSubscriber]struct{}
}

func newEffectiveConfigSubscribers() *effectiveConfigSubscribers {
	return &effectiveConfigSubscribers{
		all: map[string]map[*effectiveConfigSubscriber]struct{}{},
	}
}

func (s *effectiveConfigSubscribers) add(sub *effectiveConfigSubscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.all[sub.workspaceDir]
	if !ok {
		bucket = map[*effectiveConfigSubscriber]struct{}{}
		s.all[sub.workspaceDir] = bucket
	}
	bucket[sub] = struct{}{}
}

func (s *effectiveConfigSubscribers) remove(sub *effectiveConfigSubscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.all[sub.workspaceDir]
	if !ok {
		return
	}
	delete(bucket, sub)
	if len(bucket) == 0 {
		delete(s.all, sub.workspaceDir)
	}
}

func (s *effectiveConfigSubscribers) publish(
	workspaceDir string,
	ev *rpcpb.EffectiveConfigServiceSubscribeResponse,
) {
	s.mu.Lock()
	subs := s.all[workspaceDir]
	out := make([]*effectiveConfigSubscriber, 0, len(subs))
	for sub := range subs {
		out = append(out, sub)
	}
	s.mu.Unlock()
	for _, sub := range out {
		select {
		case sub.ch <- ev:
		default:
			// Buffer full → drop. Unlike the doc-scoped streams,
			// this one's events are DELTAS (only sections that
			// shifted), not full snapshots. A dropped event means
			// the client misses one update — they'd need to call
			// GetEffectiveConfig to resync. For a debug-panel
			// consumer that's acceptable; high-throughput consumers
			// would want a larger buffer or a re-snapshot signal.
		}
	}
}

// Subscribe implements rpcpbconnect.EffectiveConfigServiceHandler.
//
// Stream contract: first message is INTENTIONALLY EMPTY (no
// changes). Callers do a separate GetEffectiveConfig once for the
// snapshot, then keep the Subscribe stream open to react to deltas.
// This differs from Secrets/Datasets — there are way more sections
// here, the snapshot is much bigger, and the natural UI consumer
// (debug panel) already calls GetEffectiveConfig at mount for the
// initial render. Folding the snapshot into the first event would
// double the work for that common path.
func (h *rpcEffectiveConfigService) Subscribe(
	ctx context.Context,
	req *connect.Request[rpcpb.EffectiveConfigServiceSubscribeRequest],
	stream *connect.ServerStream[rpcpb.EffectiveConfigServiceSubscribeResponse],
) error {
	workspaceDir := req.Msg.WorkspaceDir
	sub := &effectiveConfigSubscriber{
		workspaceDir: workspaceDir,
		ch:           make(chan *rpcpb.EffectiveConfigServiceSubscribeResponse, 16),
	}
	h.srv.effectiveConfigSubs().add(sub)
	defer h.srv.effectiveConfigSubs().remove(sub)

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-sub.ch:
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
}

// publishEffectiveConfig fans a change event out to every active
// stream for workspaceDir. Called from NotifyEffectiveConfigChanged
// — the LSP notification path. Mirrors that path's redaction: any
// secret-sensitive section's value is masked before publish.
func (s *Server) publishEffectiveConfig(
	workspaceDir string,
	changes map[string]ConfigValue,
) {
	if s.rpc == nil || len(changes) == 0 {
		return
	}
	// RedactChangeMap is shared with the LSP notification path so
	// the on-the-wire shape matches across both transports.
	redacted := RedactChangeMap(changes)
	protoChanges := make(map[string]*rpcpb.ConfigValue, len(redacted))
	for section, v := range redacted {
		protoChanges[section] = configValueToProto(v)
	}
	ev := &rpcpb.EffectiveConfigServiceSubscribeResponse{
		WorkspaceDir: workspaceDir,
		Changes:      protoChanges,
	}
	s.effectiveConfigSubs().publish(workspaceDir, ev)
}

func (s *Server) effectiveConfigSubs() *effectiveConfigSubscribers {
	s.effectiveConfigSubsOnce.Do(func() {
		s.effectiveConfigSubsInstance = newEffectiveConfigSubscribers()
	})
	return s.effectiveConfigSubsInstance
}
