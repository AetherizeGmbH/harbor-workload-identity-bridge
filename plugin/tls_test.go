// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pki is a throwaway CA with helpers to issue leaf certs.
type pki struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newPKI(t *testing.T) *pki {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &pki{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns PEM cert + key for a leaf with the given usage.
func (p *pki) issue(t *testing.T, usage x509.ExtKeyUsage) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.cert, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// startBridge serves a canned credential response over TLS signed by ca,
// optionally requiring a client cert from clientCA.
func startBridge(t *testing.T, ca *pki, clientCA *pki) string {
	t.Helper()
	certPEM, keyPEM := ca.issue(t, x509.ExtKeyUsageServerAuth)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(bridgeResponse{Username: "u", Password: "p", ExpiresInSecs: 60, CacheKeyType: "Registry"})
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	if clientCA != nil {
		pool := x509.NewCertPool()
		pool.AddCert(clientCA.cert)
		srv.TLS.ClientCAs = pool
		srv.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestNewBridgeClient_TrustsCABundleFromFile(t *testing.T) {
	ca := newPKI(t)
	url := startBridge(t, ca, nil)
	bc, err := newBridgeClient(&config{Endpoint: url, CABundle: writeFile(t, "ca.crt", ca.pem)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bc.fetch("h/x:1", "tok"); err != nil {
		t.Fatalf("fetch with the right CA: %v", err)
	}
}

func TestNewBridgeClient_TrustsInlineCABundle(t *testing.T) {
	ca := newPKI(t)
	url := startBridge(t, ca, nil)
	bc, err := newBridgeClient(&config{Endpoint: url, CABundle: string(ca.pem)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bc.fetch("h/x:1", "tok"); err != nil {
		t.Fatalf("fetch with an inline CA: %v", err)
	}
}

func TestNewBridgeClient_RejectsUntrustedServer(t *testing.T) {
	url := startBridge(t, newPKI(t), nil)
	bc, err := newBridgeClient(&config{Endpoint: url, CABundle: string(newPKI(t).pem)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bc.fetch("h/x:1", "tok"); err == nil {
		t.Fatal("a server signed by another CA was trusted — the SA token would be sent to it")
	}
}

func TestNewBridgeClient_PresentsClientCertForMTLS(t *testing.T) {
	ca := newPKI(t)
	url := startBridge(t, ca, ca)
	certPEM, keyPEM := ca.issue(t, x509.ExtKeyUsageClientAuth)

	without, err := newBridgeClient(&config{Endpoint: url, CABundle: string(ca.pem)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := without.fetch("h/x:1", "tok"); err == nil {
		t.Fatal("mTLS bridge accepted a client without a certificate")
	}

	with, err := newBridgeClient(&config{
		Endpoint: url, CABundle: string(ca.pem),
		ClientCert: writeFile(t, "tls.crt", certPEM), ClientKey: writeFile(t, "tls.key", keyPEM),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := with.fetch("h/x:1", "tok"); err != nil {
		t.Fatalf("fetch with a client cert: %v", err)
	}
}

func TestNewBridgeClient_ConfigErrors(t *testing.T) {
	if _, err := newBridgeClient(&config{Endpoint: "https://x", CABundle: "/does/not/exist"}); err == nil {
		t.Error("missing CA file accepted")
	}
	if _, err := newBridgeClient(&config{Endpoint: "https://x", CABundle: "-----BEGIN nothing"}); err == nil || !strings.Contains(err.Error(), "no PEM") {
		t.Errorf("garbage inline CA: %v", err)
	}
	if _, err := newBridgeClient(&config{Endpoint: "https://x", ClientCert: "/nope.crt", ClientKey: "/nope.key"}); err == nil {
		t.Error("missing client cert accepted")
	}
}
