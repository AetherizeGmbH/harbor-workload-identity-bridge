// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSignedCert generates an RSA self-signed cert valid for
// localhost and writes the cert + key to t.TempDir(). Returns the two
// paths. Cheap enough to run per test; sidesteps any need to ship a
// pre-baked fixture.
func writeSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

func TestNewServer_RequiresHandler(t *testing.T) {
	cert, key := writeSelfSignedCert(t)
	if _, err := NewServer(ServerConfig{CertFile: cert, KeyFile: key}); err == nil {
		t.Fatal("expected error for missing Handler")
	}
}

func TestNewServer_RequiresCertAndKey(t *testing.T) {
	if _, err := NewServer(ServerConfig{Handler: http.NewServeMux()}); err == nil {
		t.Fatal("expected error for missing CertFile/KeyFile")
	}
}

func TestNewServer_FailsOnUnreadableCert(t *testing.T) {
	if _, err := NewServer(ServerConfig{
		Handler:  http.NewServeMux(),
		CertFile: "/nonexistent/tls.crt",
		KeyFile:  "/nonexistent/tls.key",
	}); err == nil {
		t.Fatal("expected error for missing keypair")
	}
}

func TestServer_ServesAndShutsDownCleanly(t *testing.T) {
	cert, key := writeSelfSignedCert(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv, err := NewServer(ServerConfig{
		ListenAddr:      "127.0.0.1:0",
		CertFile:        cert,
		KeyFile:         key,
		Handler:         mux,
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(ctx) }()

	// Wait until the listener has bound; Start mutates srv.Addr in its
	// goroutine before ServeTLS. Spin briefly rather than sleeping a
	// fixed duration.
	addr := waitForBind(t, srv, 2*time.Second)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 2 * time.Second,
	}
	resp, err := client.Get("https://" + addr + "/ping")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}
}

func TestServer_FailsToListenOnBusyPort(t *testing.T) {
	cert, key := writeSelfSignedCert(t)

	// Hold a port to force a bind conflict.
	hold, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold listener: %v", err)
	}
	defer hold.Close()

	srv, err := NewServer(ServerConfig{
		ListenAddr: hold.Addr().String(),
		CertFile:   cert,
		KeyFile:    key,
		Handler:    http.NewServeMux(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := srv.Start(ctx); err == nil {
		t.Fatal("expected listen error on busy port")
	}
}

func TestServer_MTLS_RejectsConnectionWithoutClientCert(t *testing.T) {
	cert, key := writeSelfSignedCert(t)
	// Re-use the same cert as both server cert and trusted client CA;
	// what we want to verify is the "no client cert presented" rejection.
	srv, err := NewServer(ServerConfig{
		ListenAddr:   "127.0.0.1:0",
		CertFile:     cert,
		KeyFile:      key,
		ClientCAFile: cert,
		Handler:      http.NewServeMux(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(ctx) }()
	addr := waitForBind(t, srv, 2*time.Second)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 2 * time.Second,
	}
	_, err = client.Get("https://" + addr + "/")
	if err == nil {
		t.Fatal("expected TLS handshake to fail without client cert")
	}
}

func TestNewServer_RejectsInvalidClientCAFile(t *testing.T) {
	cert, key := writeSelfSignedCert(t)
	dir := t.TempDir()
	bogus := filepath.Join(dir, "bogus-ca.pem")
	if err := os.WriteFile(bogus, []byte("this is not a PEM file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(ServerConfig{
		ListenAddr:   ":0",
		CertFile:     cert,
		KeyFile:      key,
		ClientCAFile: bogus,
		Handler:      http.NewServeMux(),
	}); err == nil {
		t.Fatal("expected error for CA file with no PEM-encoded certs")
	}
}

// waitForBind polls srv.Addr() until it no longer ends in ":0", or
// timeout. Avoids hardcoding a sleep.
func waitForBind(t *testing.T, srv *Server, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, port, err := net.SplitHostPort(srv.Addr())
		if err == nil && port != "0" {
			// Also confirm the port is reachable; the goroutine may
			// have set Addr but not yet entered ServeTLS.
			conn, dErr := net.DialTimeout("tcp", srv.Addr(), 200*time.Millisecond)
			if dErr == nil {
				_ = conn.Close()
				return srv.Addr()
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server did not bind in time")
	return ""
}
