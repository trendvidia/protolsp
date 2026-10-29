// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Server-side state for DatasetsService.Subscribe. Same template as
// rpc_secrets_subscribe.go — see comments there for the rationale on
// snapshot-first events, lossy backpressure semantics, and the per-
// Server lazy registry.

package server

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect"
	"go.lsp.dev/uri"

	rpcpb "github.com/trendvidia/protolsp/proto/protolsp/v1/rpc"
)

type datasetsSubscriber struct {
	docURI uri.URI
	ch     chan *rpcpb.DatasetsServiceSubscribeResponse
}

type datasetsSubscribers struct {
	mu  sync.Mutex
	all map[uri.URI]map[*datasetsSubscriber]struct{}
}

func newDatasetsSubscribers() *datasetsSubscribers {
	return &datasetsSubscribers{all: map[uri.URI]map[*datasetsSubscriber]struct{}{}}
}

func (s *datasetsSubscribers) add(sub *datasetsSubscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.all[sub.docURI]
	if !ok {
		bucket = map[*datasetsSubscriber]struct{}{}
		s.all[sub.docURI] = bucket
	}
	bucket[sub] = struct{}{}
}

func (s *datasetsSubscribers) remove(sub *datasetsSubscriber) {
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

func (s *datasetsSubscribers) publish(docURI uri.URI, ev *rpcpb.DatasetsServiceSubscribeResponse) {
	s.mu.Lock()
	subs := s.all[docURI]
	out := make([]*datasetsSubscriber, 0, len(subs))
	for sub := range subs {
		out = append(out, sub)
	}
	s.mu.Unlock()
	for _, sub := range out {
		select {
		case sub.ch <- ev:
		default:
			// Buffer full → drop. Next event carries the full
			// region list, so the consumer catches up.
		}
	}
}

// Subscribe implements rpcpbconnect.DatasetsServiceHandler. First
// stream message is the current snapshot; subsequent messages fire
// from publishDatasetRegions whenever the analyze pipeline detects
// a structural change in the @dataset regions.
func (h *rpcDatasetsService) Subscribe(
	ctx context.Context,
	req *connect.Request[rpcpb.DatasetsServiceSubscribeRequest],
	stream *connect.ServerStream[rpcpb.DatasetsServiceSubscribeResponse],
) error {
	docURI := uri.URI(req.Msg.TextDocument.GetUri())
	if docURI == "" {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("text_document.uri is required"))
	}

	sub := &datasetsSubscriber{
		docURI: docURI,
		ch:     make(chan *rpcpb.DatasetsServiceSubscribeResponse, 16),
	}
	h.srv.datasetsSubs().add(sub)
	defer h.srv.datasetsSubs().remove(sub)

	if err := stream.Send(currentDatasetsEvent(h.srv, docURI)); err != nil {
		return err
	}

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

func currentDatasetsEvent(s *Server, docURI uri.URI) *rpcpb.DatasetsServiceSubscribeResponse {
	doc := s.Documents().Get(docURI)
	if doc == nil {
		return &rpcpb.DatasetsServiceSubscribeResponse{}
	}
	doc.RLock()
	defer doc.RUnlock()
	return &rpcpb.DatasetsServiceSubscribeResponse{
		Version: doc.Version,
		Regions: datasetRegionsToProto(doc.DatasetRegions),
	}
}

// publishDatasetRegions fans the latest snapshot out to every active
// stream for docURI. Called from notifyDatasetRegionsChanged.
func (s *Server) publishDatasetRegions(docURI uri.URI, version int32, current []DatasetRegion) {
	if s.rpc == nil {
		return
	}
	ev := &rpcpb.DatasetsServiceSubscribeResponse{
		Version: version,
		Regions: datasetRegionsToProto(current),
	}
	s.datasetsSubs().publish(docURI, ev)
}

func (s *Server) datasetsSubs() *datasetsSubscribers {
	s.datasetsSubsOnce.Do(func() {
		s.datasetsSubsInstance = newDatasetsSubscribers()
	})
	return s.datasetsSubsInstance
}
