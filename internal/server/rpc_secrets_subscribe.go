// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Server-side state for SecretsService.Subscribe — the streaming
// counterpart to the existing one-shot GetRanges + LSP-notification
// flow. Subscribers register a channel per (server, document URI);
// notifySecretRangesChanged fans events out to every channel
// alongside the existing LSP send. Slow consumers drop events
// rather than blocking the analyze pipeline.

package server

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect"
	"go.lsp.dev/uri"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
)

// secretsSubscriber is one outstanding Subscribe stream. Each call
// to SecretsService.Subscribe allocates one and registers it under
// the document URI it cares about.
type secretsSubscriber struct {
	docURI uri.URI
	// Buffered channel — the publisher uses a non-blocking send so
	// a slow stream consumer can't stall the analyze pipeline. When
	// the buffer's full, the event is dropped. For secret-range
	// updates the lossy semantics are fine: the next published
	// event always carries the latest full table, not a delta.
	ch chan *rpcpb.SecretsServiceSubscribeResponse
}

// secretsSubscribers tracks every active Subscribe stream. Lookup
// is by document URI — a single document can have multiple streams
// (e.g. two editor windows pointed at the same file).
type secretsSubscribers struct {
	mu  sync.Mutex
	all map[uri.URI]map[*secretsSubscriber]struct{}
}

func newSecretsSubscribers() *secretsSubscribers {
	return &secretsSubscribers{all: map[uri.URI]map[*secretsSubscriber]struct{}{}}
}

func (s *secretsSubscribers) add(sub *secretsSubscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.all[sub.docURI]
	if !ok {
		bucket = map[*secretsSubscriber]struct{}{}
		s.all[sub.docURI] = bucket
	}
	bucket[sub] = struct{}{}
}

func (s *secretsSubscribers) remove(sub *secretsSubscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.all[sub.docURI]
	if !ok {
		return
	}
	delete(bucket, sub)
	if len(bucket) == 0 {
		delete(s.all, sub.docURI)
	}
}

// publish fans an event out to every active subscriber for docURI.
// Non-blocking on each channel; a slow consumer drops the event
// rather than stalling the publisher.
func (s *secretsSubscribers) publish(docURI uri.URI, ev *rpcpb.SecretsServiceSubscribeResponse) {
	s.mu.Lock()
	subs := s.all[docURI]
	// Snapshot under the lock so publish-side iteration can't race
	// with concurrent add/remove. The snapshot is a fixed slice of
	// pointers; the channels themselves are concurrency-safe.
	out := make([]*secretsSubscriber, 0, len(subs))
	for sub := range subs {
		out = append(out, sub)
	}
	s.mu.Unlock()
	for _, sub := range out {
		select {
		case sub.ch <- ev:
		default:
			// Buffer full — drop. The latest event always carries
			// the full snapshot, so the next successful send
			// catches the consumer up.
		}
	}
}

// Subscribe implements rpcpbconnect.SecretsServiceHandler. The
// stream's first message is the current snapshot (so callers don't
// need a separate GetRanges before opening the stream); subsequent
// messages are the delta-driven events fired from
// notifySecretRangesChanged via publishSecretRanges.
func (h *rpcSecretsService) Subscribe(
	ctx context.Context,
	req *connect.Request[rpcpb.SecretsServiceSubscribeRequest],
	stream *connect.ServerStream[rpcpb.SecretsServiceSubscribeResponse],
) error {
	docURI := uri.URI(req.Msg.TextDocument.GetUri())
	if docURI == "" {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("text_document.uri is required"))
	}

	sub := &secretsSubscriber{
		docURI: docURI,
		ch:     make(chan *rpcpb.SecretsServiceSubscribeResponse, 16),
	}
	h.srv.secretsSubs().add(sub)
	defer h.srv.secretsSubs().remove(sub)

	// Send the initial snapshot synchronously. Callers that just
	// want the current state can read once and Cancel — same
	// semantics as a GetRanges call, no extra round trip.
	if err := stream.Send(currentSecretsEvent(h.srv, docURI)); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			// Client cancelled or disconnected. Connect handles the
			// cleanup; our defer pulls us out of the subscriber set.
			return nil
		case ev := <-sub.ch:
			if err := stream.Send(ev); err != nil {
				// Stream broken — exit. The defer cleans us up.
				return err
			}
		}
	}
}

// currentSecretsEvent builds a SubscribeResponse from the current
// secret-range snapshot for docURI. Used as the stream's initial
// message; mirrors what GetRanges returns. Empty ranges + version=0
// for documents that aren't open.
func currentSecretsEvent(s *Server, docURI uri.URI) *rpcpb.SecretsServiceSubscribeResponse {
	doc := s.Documents().Get(docURI)
	if doc == nil {
		return &rpcpb.SecretsServiceSubscribeResponse{}
	}
	doc.RLock()
	defer doc.RUnlock()
	return &rpcpb.SecretsServiceSubscribeResponse{
		Version: doc.Version,
		Ranges:  secretRangesToProto(doc.SecretRanges),
	}
}

// publishSecretRanges fans the latest snapshot out to every stream
// subscribed to docURI. Called from notifySecretRangesChanged on
// every edit-driven analyze pass where the secret table changed.
func (s *Server) publishSecretRanges(docURI uri.URI, version int32, current []SecretRange) {
	if s.rpc == nil {
		return // no sidecar / no subscribers possible
	}
	ev := &rpcpb.SecretsServiceSubscribeResponse{
		Version: version,
		Ranges:  secretRangesToProto(current),
	}
	s.secretsSubs().publish(docURI, ev)
}

// secretsSubs lazily allocates the per-Server subscriber registry.
// Keeping it lazy means Server New doesn't pay for it on tests
// that never exercise the sidecar.
func (s *Server) secretsSubs() *secretsSubscribers {
	s.secretsSubsOnce.Do(func() {
		s.secretsSubsInstance = newSecretsSubscribers()
	})
	return s.secretsSubsInstance
}
