// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package schema

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/trendvidia/protoregistry/client"
)

// DialTimeout caps how long schema.Open will wait for client.Dial
// to return. Without it, a registry that accepts TCP but stalls
// (or a wrong-port address that the TCP stack quietly buffers)
// leaves the rebuild goroutine alive indefinitely; repeated
// config edits then leak goroutines. 30s is long enough to cover
// slow links + ServerChain handshake + initial namespace pull,
// short enough that a stuck dial doesn't outlive the user's
// patience.
const DialTimeout = 30 * time.Second

// Lookup is the minimal interface the server uses to resolve message
// types by fully-qualified name, enumerate known types for completion
// suggestions, and report which namespace contributed each
// descriptor. Real implementations wrap a protoregistry/client.Resolver;
// tests pass a fake.
type Lookup interface {
	FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error)

	// RangeMessages iterates every message type currently visible to
	// the resolver — bound namespace plus any chained parents. The
	// callback returns false to stop the walk. Used by protolsp to
	// populate completion lists for the `@type` directive.
	RangeMessages(f func(protoreflect.MessageType) bool)

	// FindFileByPathWithOrigin looks up a file by its proto path and
	// returns the namespace ID that contributed it. Used by hover to
	// render "defined in namespace <X>" provenance. Origin is "" when
	// the file came from an ad-hoc parent (WithParent / WithFallback)
	// whose namespace identity isn't tracked.
	FindFileByPathWithOrigin(path string) (protoreflect.FileDescriptor, string, error)

	// GetSource fetches the original .proto source bytes for a file
	// known to the resolver. Used by the protolsp/sourceContent
	// custom request — the editor's TextDocumentContentProvider for
	// `registry://` URIs reads back through this to render the
	// virtual document. Returns protoregistry.NotFound for paths
	// reachable only through ad-hoc parents (no registry connection).
	GetSource(ctx context.Context, filePath string) ([]byte, error)

	// IsStale reports whether the resolver is serving from a disk
	// cache rather than a live registry connection. The server uses
	// this to (a) warn the user via window/showMessage at startup
	// and (b) answer the protolsp/registryStatus custom request so
	// editor UIs can render a "stale schema" indicator.
	IsStale() bool
}

// Config mirrors the workspace's registry configuration block, supplied
// via the LSP initialize call's initializationOptions. Empty Address
// disables schema validation; the server then publishes parse-only
// diagnostics.
//
// CacheDir, when set, enables the protoregistry client's on-disk
// cache. The resolver persists every successful refresh under
// <CacheDir>/<namespace>/ and falls back to that snapshot when the
// registry is unreachable at startup. Editors should default this
// to a per-user cache location (e.g. $XDG_CACHE_HOME/protolsp/) so
// schema-aware features survive offline workdays.
type Config struct {
	Address   string     `json:"address"`
	Namespace string     `json:"namespace"`
	Token     string     `json:"token,omitempty"`
	CacheDir  string     `json:"cacheDir,omitempty"`
	TLS       *TLSConfig `json:"tls,omitempty"`
}

// TLSConfig configures TLS transport for the registry dial. Absent
// (nil) means insecure transport, the historical default — fine for
// loopback / docker-compose dev, never appropriate when the registry
// lives anywhere a network operator can sniff. Set TLS to a non-nil
// value (even the zero value) to enable TLS; the empty struct means
// "standard TLS with the system trust store and the address's host
// component as the expected server name."
type TLSConfig struct {
	// ServerName overrides the SNI / certificate-verification name.
	// Leave empty to use the host portion of Config.Address. Useful
	// when dialing via an IP or a load-balancer whose advertised
	// certificate names a backing service.
	ServerName string `json:"serverName,omitempty"`

	// CACertPath, when set, points at a PEM file with additional
	// trust roots appended to the system pool. Required when the
	// registry is fronted by a private CA the editor's host doesn't
	// trust by default. Empty means system roots only.
	CACertPath string `json:"caCertPath,omitempty"`

	// InsecureSkipVerify disables certificate verification entirely.
	// Reserved for local-only testing; setting this true on a
	// network-reachable registry is equivalent to no TLS — strip it
	// when shipping. Open.buildTLSCredentials logs a warning when
	// it sees this set so the choice is at least surfaced.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// Validate returns nil when the config is sufficient to construct a
// resolver. An empty Config is valid and means "schema validation
// disabled" — Open returns (nil, nil, nil) in that case.
func (c Config) Validate() error {
	if c.Address == "" && c.Namespace == "" {
		return nil
	}
	if c.Address == "" {
		return errors.New("schema config: namespace set but address is empty")
	}
	if c.Namespace == "" {
		return errors.New("schema config: address set but namespace is empty")
	}
	return nil
}

// OpenOption tweaks the dial path in Open. The current option is
// WithFallback (W3 of workspace-config); future options can layer
// in without changing the Open signature.
type OpenOption func(*openOptions)

type openOptions struct {
	fallbackFiles *protoregistry.NamespacedFiles
	fallbackTypes *protoregistry.NamespacedTypes
}

// WithFallback wires the registry Resolver to fall through to the
// given NamespacedFiles + NamespacedTypes when a lookup misses
// locally (W3 of workspace-config). Both arguments must be non-nil;
// nil disables the fallback. Used to compose a proto_files schema
// source behind a registry source on the same workspace.
//
// The caller owns the lifecycle of the fallback registries —
// schema.Open neither closes them on the resulting Resolver's
// Close nor copies them.
func WithFallback(files *protoregistry.NamespacedFiles, types *protoregistry.NamespacedTypes) OpenOption {
	return func(o *openOptions) {
		o.fallbackFiles = files
		o.fallbackTypes = types
	}
}

// Open constructs a Lookup from the given config, or returns
// (nil, nil, nil) when the config disables schema validation. The
// returned io.Closer should be closed on server shutdown to stop the
// resolver's background refresh goroutine.
//
// When CacheDir is set, the resolver persists each successful refresh
// to disk and — critically — falls back to that cached snapshot when
// the registry is unreachable at dial time. Lookup.IsStale() reports
// whether the resolver is serving from cache; callers (the LSP
// server) surface that to the user via window/showMessage at startup
// and via the protolsp/registryStatus custom request.
func Open(ctx context.Context, cfg Config, opts ...OpenOption) (Lookup, io.Closer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	if cfg.Address == "" {
		return nil, nil, nil
	}
	var oo openOptions
	for _, opt := range opts {
		opt(&oo)
	}
	clientOpts := []client.Option{
		client.WithServerChain(),
	}
	if cfg.Token != "" {
		clientOpts = append(clientOpts, client.WithToken(cfg.Token))
	}
	if cfg.CacheDir != "" {
		clientOpts = append(clientOpts, client.WithDiskCache(cfg.CacheDir))
	}
	if cfg.TLS != nil {
		creds, err := buildTLSCredentials(cfg.Address, cfg.TLS)
		if err != nil {
			return nil, nil, fmt.Errorf("building TLS credentials: %w", err)
		}
		clientOpts = append(clientOpts, client.WithTransportCredentials(creds))
	}
	// W3: layer in fallback after the server-chain default so the
	// last-writer-wins semantic on client.config.parentFiles puts
	// the local proto_files set in place. WithServerChain still
	// applies for namespace ancestors served by the registry — the
	// two coexist because WithFallback also fires the ServerChain
	// machinery (the client.config.useServerChain flag persists
	// regardless of when WithFallback runs).
	if oo.fallbackFiles != nil && oo.fallbackTypes != nil {
		clientOpts = append(clientOpts, client.WithFallback(oo.fallbackFiles, oo.fallbackTypes))
	}
	// Bound the dial. A caller-supplied earlier deadline still
	// wins; this only adds an upper bound when the caller passed
	// context.Background (the async-rebuild path does that
	// today since the request context is gone by the time the
	// goroutine fires).
	dialCtx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()
	r, err := client.Dial(dialCtx, cfg.Address, cfg.Namespace, clientOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing protoregistry %s: %w", cfg.Address, err)
	}
	return r, r, nil
}

// buildTLSCredentials translates a TLSConfig into gRPC transport
// credentials. Errors flow back to the caller via schema.Open so the
// LSP surface can warn the user that schema validation is disabled
// (typical cause: a caCertPath that doesn't exist or fails to parse).
//
// ServerName defaults to the host portion of address — gRPC needs a
// SNI / verification name and our caller never explicitly sets it
// unless the cert names something other than the dial host.
func buildTLSCredentials(address string, cfg *TLSConfig) (credentials.TransportCredentials, error) {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if cfg.ServerName != "" {
		tlsCfg.ServerName = cfg.ServerName
	} else {
		tlsCfg.ServerName = hostFromAddress(address)
	}
	if cfg.CACertPath != "" {
		pool, err := loadCertPool(cfg.CACertPath)
		if err != nil {
			return nil, fmt.Errorf("loading CA bundle %s: %w", cfg.CACertPath, err)
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.InsecureSkipVerify {
		// gosec G402: the operator opted in via explicit config. The
		// callsite in dispatchInitialize / configureSchemaLookup
		// emits a window/showMessage warning so the user sees the
		// choice in the editor too.
		tlsCfg.InsecureSkipVerify = true //#nosec G402 -- opt-in dev-only flag
	}
	return credentials.NewTLS(tlsCfg), nil
}

// loadCertPool reads a PEM file containing one or more CA
// certificates and returns a pool that includes the system roots
// plus those CAs. A single unparseable PEM block fails the load —
// silent partial trust would be worse than no TLS at all.
func loadCertPool(path string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(path) //#nosec G304 -- path is operator-supplied config
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// SystemCertPool returns an error on some platforms (notably
		// Windows in older Go versions). Fall through to an empty
		// pool so the supplied bundle still works in isolation.
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("no certificates found in PEM bundle")
	}
	return pool, nil
}

// hostFromAddress returns the host portion of a gRPC dial address.
// gRPC accepts host:port, scheme://host:port, and bare hosts. We
// strip the scheme prefix if present and drop the port. This is
// best-effort — gRPC's resolver handles weirder shapes (DNS lookups,
// passthrough URIs) but those don't need a populated ServerName
// since they imply a different SNI handshake path.
func hostFromAddress(addr string) string {
	for _, prefix := range []string{"dns:///", "dns://", "ipv4:", "ipv6:", "passthrough:///", "unix:"} {
		if len(addr) >= len(prefix) && addr[:len(prefix)] == prefix {
			addr = addr[len(prefix):]
			break
		}
	}
	// Strip trailing :port, but keep IPv6 brackets intact ([::1]:443).
	if len(addr) > 0 && addr[0] == '[' {
		if end := indexByte(addr, ']'); end >= 0 {
			return addr[1:end]
		}
		return addr
	}
	if i := indexByte(addr, ':'); i >= 0 {
		return addr[:i]
	}
	return addr
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
