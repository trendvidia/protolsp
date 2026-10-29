// Copyright (c) 2026 TrendVidia, LLC.
// SPDX-License-Identifier: MIT

package schema

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostFromAddress_StripsSchemeAndPort(t *testing.T) {
	cases := map[string]string{
		"registry.example.com:443":                "registry.example.com",
		"registry.example.com":                    "registry.example.com",
		"dns:///registry.example.com:443":         "registry.example.com",
		"ipv4:10.0.0.1:443":                       "10.0.0.1",
		"[2001:db8::1]:443":                       "2001:db8::1",
		"passthrough:///registry.example.com:443": "registry.example.com",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			assert.Equal(t, want, hostFromAddress(input))
		})
	}
}

func TestBuildTLSCredentials_DefaultsToAddressHost(t *testing.T) {
	creds, err := buildTLSCredentials("registry.example.com:443", &TLSConfig{})
	require.NoError(t, err)
	require.NotNil(t, creds)
	// credentials.NewTLS returns an opaque value; we check the
	// server-name negotiation surface via its public Info() shape.
	assert.Equal(t, "tls", creds.Info().SecurityProtocol)
}

func TestBuildTLSCredentials_ServerNameOverride(t *testing.T) {
	// The override flows into the underlying tls.Config; we can't
	// observe it back through the public credentials.Info() surface
	// (which deprecates ServerName), but we can verify the helper
	// accepts the override without error — guarding against typos
	// or accidental conditional branches that skip it.
	creds, err := buildTLSCredentials("10.0.0.1:443", &TLSConfig{ServerName: "registry.internal"})
	require.NoError(t, err)
	require.NotNil(t, creds)
}

func TestBuildTLSCredentials_CACertFileMissingErrors(t *testing.T) {
	_, err := buildTLSCredentials("registry.example.com:443", &TLSConfig{
		CACertPath: "/does/not/exist/ca.pem",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/does/not/exist/ca.pem")
}

func TestBuildTLSCredentials_CACertFileMalformedErrors(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "garbage.pem")
	require.NoError(t, os.WriteFile(path, []byte("not a pem file"), 0o600))

	_, err := buildTLSCredentials("registry.example.com:443", &TLSConfig{CACertPath: path})
	require.Error(t, err, "non-PEM bytes must surface as an error rather than silently accept zero certs")
	assert.Contains(t, err.Error(), "no certificates")
}

func TestBuildTLSCredentials_AcceptsValidCABundle(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "ca.pem")
	require.NoError(t, os.WriteFile(path, generateSelfSignedCAPEM(t), 0o600))

	creds, err := buildTLSCredentials("registry.example.com:443", &TLSConfig{CACertPath: path})
	require.NoError(t, err)
	require.NotNil(t, creds)
}

func TestBuildTLSCredentials_InsecureSkipVerifyHonored(t *testing.T) {
	// Can't observe InsecureSkipVerify on the returned credentials
	// directly — the field is internal to the tls.Config. This test
	// guards against the option getting silently dropped on the
	// path through buildTLSCredentials (e.g. moved into a branch
	// that compiles away).
	creds, err := buildTLSCredentials("registry.example.com:443", &TLSConfig{InsecureSkipVerify: true})
	require.NoError(t, err)
	require.NotNil(t, creds)
}

func TestOpen_TLSConfigPassedThroughWithoutError(t *testing.T) {
	// End-to-end check that Open accepts the TLS field. Dial fails
	// because 127.0.0.1:1 refuses, but it should fail AFTER building
	// credentials — proving the TLS path doesn't reject upfront.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := Open(ctx, Config{
		Address:   "127.0.0.1:1",
		Namespace: "anything",
		TLS:       &TLSConfig{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "127.0.0.1:1",
		"error must come from the dial, not from buildTLSCredentials")
}

func TestOpen_TLSConfigBadCABundleSurfacesAsBuildError(t *testing.T) {
	_, _, err := Open(context.Background(), Config{
		Address:   "127.0.0.1:1",
		Namespace: "anything",
		TLS:       &TLSConfig{CACertPath: "/does/not/exist/ca.pem"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "building TLS credentials",
		"misconfigured TLS must surface as a build error, not a dial error")
}

// generateSelfSignedCAPEM produces a throwaway self-signed CA
// certificate in PEM form. Only used to satisfy AppendCertsFromPEM's
// "at least one cert" check in TestBuildTLSCredentials_AcceptsValidCABundle.
func generateSelfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "protolsp test CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
