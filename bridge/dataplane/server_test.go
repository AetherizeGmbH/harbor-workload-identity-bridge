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
	resp, err := client.Get("https://" + addr + "/")
	if err == nil {
		_ = resp.Body.Close()
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

// writeServingPair writes a self-signed serving pair with the given serial
// into dir (overwriting), mimicking a cert-manager rotation.
func writeServingPair(t *testing.T, dir string, serial int64) (certPath, keyPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

type testCA struct {
	cert *x509.Certificate
	key  *rsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, cn string) *testCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (ca *testCA) clientCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "plugin"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func startServer(t *testing.T, cfg ServerConfig) (*Server, string) {
	t.Helper()
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return srv, waitForBind(t, srv, 2*time.Second)
}

func servedSerial(t *testing.T, addr string, clientCert *tls.Certificate) (int64, error) {
	t.Helper()
	// Trust is not under test here: the test inspects which cert is served.
	cfg := &tls.Config{InsecureSkipVerify: true}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	// TLS 1.3 reports a client-cert rejection on the first read, not in
	// the handshake; force a round trip.
	if _, err := conn.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		return 0, err
	}
	if _, err := conn.Read(make([]byte, 1)); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64(), nil
}

// TestServer_RunsOnEveryReplica pins the audit H1 fix: without this the
// manager starts the data plane on the leader only.
func TestServer_RunsOnEveryReplica(t *testing.T) {
	cert, key := writeSelfSignedCert(t)
	srv, err := NewServer(ServerConfig{CertFile: cert, KeyFile: key, Handler: http.NewServeMux()})
	if err != nil {
		t.Fatal(err)
	}
	if srv.NeedLeaderElection() {
		t.Fatal("data-plane server must not require leader election")
	}
}

func TestServer_ReadyOnlyWhileListening(t *testing.T) {
	cert, key := writeSelfSignedCert(t)
	srv, err := NewServer(ServerConfig{ListenAddr: "127.0.0.1:0", CertFile: cert, KeyFile: key, Handler: http.NewServeMux()})
	if err != nil {
		t.Fatal(err)
	}
	if srv.ReadyCheck(nil) == nil {
		t.Fatal("ready before the listener is bound")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Start(ctx); close(done) }()
	waitForBind(t, srv, 2*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for srv.ReadyCheck(nil) != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := srv.ReadyCheck(nil); err != nil {
		t.Fatalf("not ready while listening: %v", err)
	}
	cancel()
	<-done
	if srv.ReadyCheck(nil) == nil {
		t.Fatal("still ready after shutdown")
	}
}

// TestServer_PicksUpRotatedCertificate: cert-manager renews the Secret in
// place; the server must serve the new pair without a restart.
func TestServer_PicksUpRotatedCertificate(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeServingPair(t, dir, 1)
	_, addr := startServer(t, ServerConfig{
		ListenAddr: "127.0.0.1:0", CertFile: cert, KeyFile: key,
		Handler: http.NewServeMux(), ReloadInterval: 50 * time.Millisecond,
	})
	if got, err := servedSerial(t, addr, nil); err != nil || got != 1 {
		t.Fatalf("initial serial = %d, %v", got, err)
	}
	writeServingPair(t, dir, 2)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := servedSerial(t, addr, nil); err == nil && got == 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server never served the rotated certificate")
}

// TestServer_MTLS_PicksUpRotatedClientCA: a new issuing CA for plugin
// client certs must be honoured without a restart.
func TestServer_MTLS_PicksUpRotatedClientCA(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeServingPair(t, dir, 7)
	oldCA, newCA := newTestCA(t, "old"), newTestCA(t, "new")
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, oldCA.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	_, addr := startServer(t, ServerConfig{
		ListenAddr: "127.0.0.1:0", CertFile: cert, KeyFile: key, ClientCAFile: caPath,
		Handler: http.NewServeMux(), ReloadInterval: 50 * time.Millisecond,
	})
	newClient := newCA.clientCert(t)
	if _, err := servedSerial(t, addr, &newClient); err == nil {
		t.Fatal("client cert from a CA the server does not trust yet was accepted")
	}
	oldClient := oldCA.clientCert(t)
	if _, err := servedSerial(t, addr, &oldClient); err != nil {
		t.Fatalf("client cert from the configured CA rejected: %v", err)
	}
	if err := os.WriteFile(caPath, newCA.pem, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := servedSerial(t, addr, &newClient); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("server never trusted the rotated client CA")
}
