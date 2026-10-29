// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

// Connect-RPC sidecar that exposes protolsp's custom-method surface as
// typed proto services over HTTP/1.1+2. Lives alongside the LSP stdio
// handler — the two transports share state via the *Server pointer
// passed into each handler.
//
// Why a sidecar instead of replacing LSP: the LSP surface (open/close,
// hover, diagnostics, semantic tokens, etc.) is standardised and an
// editor extension that points at protolsp expects to speak it. The
// custom methods (configList, secretRanges, …) on the other hand are
// our own invention; moving them to typed Connect-RPC gives clients
// strong typing and proper schema validation without breaking the
// vanilla LSP path.
//
// Endpoint advertisement: the bound address and per-session bearer
// token are returned in InitializeResult.serverInfo.rpc so clients can
// discover the sidecar without out-of-band coordination. The token is
// 32 random bytes encoded as hex — sufficient against drive-by
// connections from other local processes; no replay protection, since
// the connection is over localhost-loopback only.

package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// rpcEndpoint is the discovery payload returned alongside the LSP
// InitializeResult.serverInfo. Clients connect to Address and present
// Token in the Authorization header.
type rpcEndpoint struct {
	// Address is the base URL of the Connect-RPC server, e.g.
	// "http://127.0.0.1:54321". Always loopback.
	Address string `json:"address"`
	// Token is the bearer token clients must send as
	// "Authorization: Bearer <token>". One token per protolsp
	// session — regenerated on every server start.
	Token string `json:"token"`
}

// rpcServer wraps the HTTP listener + handler stack. Owned by *Server.
// Start is idempotent (subsequent calls no-op); Stop is safe at any
// point.
type rpcServer struct {
	mu       sync.Mutex
	server   *http.Server
	listener net.Listener
	address  string
	token    string
	started  bool

	// listenAddr is the bind address startRPC uses. Empty means
	// "127.0.0.1:0" — the production default that picks any
	// free loopback port. Tests pre-populate s.rpc with a
	// non-empty value to force EADDRINUSE so the bind-failure
	// branch becomes observable.
	listenAddr string
}

// startRPC binds a fresh localhost listener, generates an auth token,
// installs every Connect-RPC handler defined in this package, and
// starts serving on a goroutine. Returns the endpoint info for
// inclusion in InitializeResult; safe to call once per Server.
//
// Any error (port exhaustion, token generation failure) is fatal for
// the sidecar but non-fatal for protolsp overall — the caller logs and
// continues with LSP-only mode. That keeps a broken sidecar from
// taking down the whole editor experience.
func (s *Server) startRPC() (*rpcEndpoint, error) {
	if s.rpc == nil {
		s.rpc = &rpcServer{}
	}
	s.rpc.mu.Lock()
	defer s.rpc.mu.Unlock()
	if s.rpc.started {
		return &rpcEndpoint{Address: s.rpc.address, Token: s.rpc.token}, nil
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("generating rpc token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)

	bindAddr := s.rpc.listenAddr
	if bindAddr == "" {
		bindAddr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("binding rpc listener: %w", err)
	}

	mux := http.NewServeMux()
	s.registerRPCServices(mux)

	// Handler chain (outermost first):
	//   1. h2c           — opportunistic HTTP/2 negotiation over plaintext
	//                       loopback (needed for the streaming Subscribe RPCs).
	//   2. corsMiddleware — Electron renderers fetch from a different origin
	//                       (file:// or http://localhost:5173 in dev) into
	//                       this loopback listener; preflights must succeed
	//                       BEFORE bearer-auth so the OPTIONS never gets a
	//                       401 it can't recover from.
	//   3. bearerAuth     — per-session token gate on the actual POSTs.
	//   4. mux            — Connect-RPC service handlers.
	handler := h2c.NewHandler(
		corsMiddleware(bearerAuth(token, mux)),
		&http2.Server{},
	)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	s.rpc.listener = listener
	s.rpc.server = srv
	s.rpc.token = token
	s.rpc.address = "http://" + listener.Addr().String()
	s.rpc.started = true

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.Logger().Warn("rpc server exited unexpectedly", "err", err.Error())
		}
	}()
	s.Logger().Info("rpc server listening",
		"address", s.rpc.address,
	)

	return &rpcEndpoint{Address: s.rpc.address, Token: token}, nil
}

// stopRPC shuts the sidecar down. Called from Server.Close.
func (s *Server) stopRPC() {
	if s.rpc == nil {
		return
	}
	s.rpc.mu.Lock()
	srv := s.rpc.server
	started := s.rpc.started
	s.rpc.started = false
	s.rpc.mu.Unlock()
	if !started || srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		s.Logger().Warn("rpc server shutdown", "err", err.Error())
	}
}

// rpcAddress returns the bound base URL once startRPC has succeeded.
// Empty when the sidecar isn't running — exposed for tests.
func (s *Server) rpcAddress() string {
	if s.rpc == nil {
		return ""
	}
	s.rpc.mu.Lock()
	defer s.rpc.mu.Unlock()
	return s.rpc.address
}

// corsMiddleware answers preflight OPTIONS and stamps CORS headers on
// every response so the Electron renderer (origin `file://` in the
// packaged build, `http://localhost:5173` in `pnpm dev`) can reach
// this loopback listener.
//
// A Connect-RPC POST carrying `Authorization: Bearer …` and
// `Content-Type: application/proto` is not a "simple" CORS request,
// so the browser fires an OPTIONS preflight first. Without these
// headers, Connect-Web fetch() fails with the generic "Failed to
// fetch" before the actual POST ever leaves the renderer.
//
// Allow-Origin is permissive (`*`) because the listener is loopback-
// only and bearer-auth still gates real requests. A hosted IDE
// deployment would tighten this to the known IDE origin and pair
// with `Access-Control-Allow-Credentials: true` if cookies replace
// the bearer token.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		// The header set must cover everything connect-web sends:
		//   Authorization                  — our bearer token
		//   Content-Type                   — application/proto or
		//                                    application/json
		//   Connect-Protocol-Version       — protocol negotiation
		//   Connect-Timeout-Ms             — deadline propagation
		//   X-User-Agent / X-Grpc-Web      — grpc-web compat
		h.Set("Access-Control-Allow-Headers",
			"Authorization, Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, X-User-Agent, X-Grpc-Web")
		// Trailer-style metadata Connect surfaces back to clients
		// — without expose, fetch() can't see them in the response.
		h.Set("Access-Control-Expose-Headers",
			"Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerAuth gates every RPC behind an Authorization: Bearer header
// matching the per-session token. Constant-time compare avoids timing
// channels; failures return 401 with no body so a probe can't tell
// "wrong token" from "endpoint exists."
func bearerAuth(expectedToken string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(got, prefix) || !secureCompare(got[len(prefix):], expectedToken) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// secureCompare is constant-time string comparison sufficient for the
// session-token check above. Wraps subtle.ConstantTimeCompare via the
// connect package's helper-style inline approach so we don't pull
// crypto/subtle for one use.
func secureCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// rpcServiceInterceptors returns the connect.Interceptor stack every
// RPC handler shares. Today: a single slog-based request logger
// covering unary + streaming RPCs. See rpc_interceptor.go for the
// implementation.
func (s *Server) rpcServiceInterceptors() connect.Option {
	return connect.WithInterceptors(&rpcLoggingInterceptor{log: s.Logger()})
}

// registerRPCServices wires every Connect-RPC service handler into
// the mux. Each rpc_*.go file in this package adds its own
// registerXxxRPC method; this is the one call site that runs them so
// new services don't require touching rpc_server.go.
func (s *Server) registerRPCServices(mux *http.ServeMux) {
	s.registerConfigRPC(mux)
	s.registerRegistryRPC(mux)
	s.registerSchemaRPC(mux)
	s.registerChameleonRPC(mux)
	s.registerSecretsRPC(mux)
	s.registerDatasetsRPC(mux)
	s.registerEffectiveConfigRPC(mux)
}
