// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func signWith(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// Every forged token with an unknown key ID used to trigger one JWKS
// fetch from the apiserver. The key set now fetches at most once per
// jwksMinRefresh.
func TestCachedKeySet_ForgedTokensDoNotFetchPerRequest(t *testing.T) {
	fi := newFixtureIssuer(t)
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		fi.handleJWKS(w, r)
	}))
	defer srv.Close()
	ks := newCachedKeySet(srv.URL, srv.Client())
	ctx := context.Background()

	if _, err := ks.VerifySignature(ctx, fi.signToken(t, fi.standardClaims())); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	forger, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := ks.VerifySignature(ctx, signWith(t, forger, fmt.Sprintf("forged-%d", i), fi.standardClaims())); err == nil {
			t.Fatal("forged token verified")
		}
	}
	if n := fetches.Load(); n > 2 {
		t.Fatalf("%d JWKS fetches for 1 valid and 50 forged tokens, want at most 2", n)
	}
	// Known keys keep working while refreshes are rate-limited.
	if _, err := ks.VerifySignature(ctx, fi.signToken(t, fi.standardClaims())); err != nil {
		t.Fatalf("valid token after the forged ones: %v", err)
	}
}

func TestCachedKeySet_PicksUpRotatedKeyAfterMinRefresh(t *testing.T) {
	fi := newFixtureIssuer(t)
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		fi.handleJWKS(w, r)
	}))
	defer srv.Close()
	now := time.Now()
	ks := newCachedKeySet(srv.URL, srv.Client())
	ks.now = func() time.Time { return now }
	ctx := context.Background()

	if _, err := ks.VerifySignature(ctx, fi.signToken(t, fi.standardClaims())); err != nil {
		t.Fatal(err)
	}
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	fi.key, fi.kid = newKey, "test-key-2"
	mu.Unlock()
	rotated := signWith(t, newKey, "test-key-2", fi.standardClaims())

	// Within jwksMinRefresh of the last fetch the unknown key is refused
	// without a fetch; after it, the first such token refreshes the set.
	now = now.Add(time.Second)
	if _, err := ks.VerifySignature(ctx, rotated); err == nil {
		t.Fatal("new key accepted before jwksMinRefresh elapsed; the refresh was not rate-limited")
	}
	now = now.Add(jwksMinRefresh)
	if _, err := ks.VerifySignature(ctx, rotated); err != nil {
		t.Fatalf("new key after jwksMinRefresh: %v", err)
	}
}

// writeCAFile writes the httptest TLS certificate as a PEM bundle.
func writeCAFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("BRIDGE-SA-TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The bridge's ServiceAccount token authenticates to the apiserver. It
// must not reach other hosts, plain http, or redirect targets.
func TestOIDCHTTPClient_TokenOnlyToAllowedHostsAndNoRedirects(t *testing.T) {
	var externalAuth atomic.Value
	externalAuth.Store("")
	external := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalAuth.Store(r.Header.Get("Authorization"))
	}))
	defer external.Close()
	var apiAuth atomic.Value
	apiAuth.Store("")
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiAuth.Store(r.Header.Get("Authorization"))
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, external.URL+"/steal", http.StatusFound)
		}
	}))
	defer api.Close()

	client, err := NewOIDCHTTPClient(writeCAFile(t, api), writeTokenFile(t))
	if err != nil {
		t.Fatal(err)
	}
	tt, ok := client.Transport.(*tokenTransport)
	if !ok {
		t.Fatalf("transport is %T", client.Transport)
	}
	apiURL, _ := url.Parse(api.URL)
	tt.allow(apiURL.Host)

	get := func(u string) int {
		t.Helper()
		resp, err := client.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	get(api.URL + "/keys")
	if got := apiAuth.Load(); got != "Bearer BRIDGE-SA-TOKEN" {
		t.Errorf("allowed host got Authorization %q", got)
	}
	get(external.URL + "/keys")
	if got := externalAuth.Load(); got != "" {
		t.Errorf("other host got Authorization %q", got)
	}
	externalAuth.Store("unvisited")
	if code := get(api.URL + "/redirect"); code != http.StatusFound {
		t.Errorf("redirect status %d, want the 302 itself (not followed)", code)
	}
	if got := externalAuth.Load(); got != "unvisited" {
		t.Errorf("redirect target was requested (Authorization %q)", got)
	}
	if tt.isAllowed(&url.URL{Scheme: "http", Host: apiURL.Host}) {
		t.Error("token allowed over plain http")
	}
}

// tlsIssuer is an OIDC issuer on an httptest TLS server that records the
// Authorization header of every request.
func tlsIssuer(t *testing.T) (*httptest.Server, *fixtureIssuer, *sync.Map) {
	t.Helper()
	fi := newFixtureIssuer(t)
	var seen sync.Map // path -> Authorization
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.URL.Path, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                                srv.URL,
				"jwks_uri":                              srv.URL + "/keys",
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/keys":
			fi.handleJWKS(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, fi, &seen
}

func TestNewValidator_SendsTokenOnlyToInClusterAPIServer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inCluster bool
		want      string
	}{
		{"in-cluster apiserver issuer", true, "Bearer BRIDGE-SA-TOKEN"},
		{"external issuer", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fi, seen := tlsIssuer(t)
			if tc.inCluster {
				t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
			} else {
				t.Setenv("KUBERNETES_SERVICE_HOST", "")
			}
			client, err := NewOIDCHTTPClient(writeCAFile(t, srv), writeTokenFile(t))
			if err != nil {
				t.Fatal(err)
			}
			v, err := NewValidator(context.Background(), Config{Issuer: srv.URL, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			claims := fi.standardClaims()
			claims["iss"] = srv.URL
			if _, err := v.Validate(context.Background(), fi.signToken(t, claims)); err != nil {
				t.Fatalf("validate: %v", err)
			}
			for _, path := range []string{"/.well-known/openid-configuration", "/keys"} {
				got, _ := seen.Load(path)
				if got != tc.want {
					t.Errorf("%s: Authorization %q, want %q", path, got, tc.want)
				}
			}
		})
	}
}
