// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ----------------------------------------------------------------------------
// Test fixture: in-memory OIDC issuer
// ----------------------------------------------------------------------------

// fixtureIssuer is a minimal OIDC provider running on an httptest server.
// It serves the discovery document and a JWKS with a single RSA key, and
// exposes helpers to sign tokens against that key. Tests instantiate it
// instead of going to a real Kubernetes apiserver.
type fixtureIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newFixtureIssuer(t *testing.T) *fixtureIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	fi := &fixtureIssuer{key: key, kid: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", fi.handleDiscovery)
	mux.HandleFunc("/keys", fi.handleJWKS)
	fi.server = httptest.NewServer(mux)
	t.Cleanup(fi.server.Close)
	return fi
}

func (fi *fixtureIssuer) URL() string { return fi.server.URL }

func (fi *fixtureIssuer) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                fi.server.URL,
		"jwks_uri":                              fi.server.URL + "/keys",
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"subject_types_supported":               []string{"public"},
		"response_types_supported":              []string{"id_token"},
	})
}

func (fi *fixtureIssuer) handleJWKS(w http.ResponseWriter, r *http.Request) {
	n := base64.RawURLEncoding.EncodeToString(fi.key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(fi.key.E)).Bytes())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": fi.kid,
			"alg": "RS256",
			"use": "sig",
			"n":   n,
			"e":   e,
		}},
	})
}

// signToken builds and signs an RS256 JWT with the fixture's key. claims
// are passed through verbatim so tests can omit/override iss, exp, aud,
// sub, etc. to construct edge cases.
func (fi *fixtureIssuer) signToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = fi.kid
	signed, err := tok.SignedString(fi.key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// signTokenWithOtherKey signs a JWT with a freshly-generated key that is
// NOT advertised in the fixture's JWKS. Used to verify signature
// tampering / unknown-key rejection.
func (fi *fixtureIssuer) signTokenWithOtherKey(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = fi.kid // claim to be the fixture key
	signed, err := tok.SignedString(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// standardClaims returns SA-token-shaped claims with iss bound to the
// fixture. Tests override fields they want to test against.
func (fi *fixtureIssuer) standardClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": fi.URL(),
		"aud": []string{"harbor.example.com"},
		"sub": "system:serviceaccount:flux-system:source-controller",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

// newValidatorFor constructs a Validator pointed at the fixture. The
// fixture's httptest.Server.Client() transport is used so plain-HTTP
// JWKS discovery works in tests without disabling TLS verification.
func newValidatorFor(t *testing.T, fi *fixtureIssuer) Validator {
	t.Helper()
	v, err := NewValidator(context.Background(), Config{
		Issuer:     fi.URL(),
		HTTPClient: fi.server.Client(),
	})
	if err != nil {
		t.Fatalf("construct validator: %v", err)
	}
	return v
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

func TestValidator_ValidToken(t *testing.T) {
	fi := newFixtureIssuer(t)
	v := newValidatorFor(t, fi)

	token := fi.signToken(t, fi.standardClaims())
	claims, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("expected validation success, got: %v", err)
	}
	if claims.Subject != "system:serviceaccount:flux-system:source-controller" {
		t.Errorf("Subject = %q", claims.Subject)
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != "harbor.example.com" {
		t.Errorf("Audience = %v", claims.Audience)
	}
	if claims.Issuer != fi.URL() {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, fi.URL())
	}
	if claims.Expiry.IsZero() {
		t.Errorf("Expiry not populated")
	}
}

func TestValidator_AcceptsAudAsString(t *testing.T) {
	// RFC 7519 allows aud to be a single string rather than an array.
	// Real Kubernetes SA tokens use the string form when projected with a
	// single audience. The validator must normalise both into Claims.Audience.
	fi := newFixtureIssuer(t)
	v := newValidatorFor(t, fi)

	claims := fi.standardClaims()
	claims["aud"] = "harbor.example.com" // string, not []string
	token := fi.signToken(t, claims)

	got, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Audience) != 1 || got.Audience[0] != "harbor.example.com" {
		t.Errorf("Audience = %v (expected single-element slice)", got.Audience)
	}
}

func TestValidator_RejectsExpiredToken(t *testing.T) {
	fi := newFixtureIssuer(t)
	v := newValidatorFor(t, fi)

	claims := fi.standardClaims()
	claims["exp"] = time.Now().Add(-time.Minute).Unix()
	claims["iat"] = time.Now().Add(-time.Hour).Unix()
	token := fi.signToken(t, claims)

	_, err := v.Validate(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected ErrInvalidToken wrapping; got %v", err)
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error should mention expiry: %v", err)
	}
}

func TestValidator_RejectsTamperedSignature(t *testing.T) {
	fi := newFixtureIssuer(t)
	v := newValidatorFor(t, fi)

	// Sign with a key not in the fixture's JWKS — looks legit (RS256, same
	// kid) but the signature won't verify against the published key.
	token := fi.signTokenWithOtherKey(t, fi.standardClaims())

	_, err := v.Validate(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for token signed with unknown key")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected ErrInvalidToken wrapping; got %v", err)
	}
}

func TestValidator_RejectsWrongIssuer(t *testing.T) {
	fi := newFixtureIssuer(t)
	v := newValidatorFor(t, fi)

	claims := fi.standardClaims()
	claims["iss"] = "https://other-cluster.example.com"
	token := fi.signToken(t, claims)

	_, err := v.Validate(context.Background(), token)
	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
	if !errors.Is(err, ErrInvalidToken) {
		t.Errorf("expected ErrInvalidToken wrapping; got %v", err)
	}
}

func TestValidator_RejectsMalformedToken(t *testing.T) {
	fi := newFixtureIssuer(t)
	v := newValidatorFor(t, fi)

	cases := map[string]string{
		"empty":              "",
		"not a JWT":          "this-is-not-a-jwt",
		"two segments":       "header.payload",
		"random base64 only": "Zm9v.YmFy.YmF6",
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Validate(context.Background(), token); err == nil {
				t.Errorf("expected error for malformed token %q", token)
			}
		})
	}
}

func TestNewValidator_FailsOnUnreachableIssuer(t *testing.T) {
	// Constructor must fail fast on a bad issuer URL so misconfiguration
	// blocks the bridge from starting.
	_, err := NewValidator(context.Background(), Config{
		Issuer:     "http://127.0.0.1:1/nonexistent",
		HTTPClient: &http.Client{Timeout: time.Second},
	})
	if err == nil {
		t.Fatal("expected NewValidator to fail on unreachable issuer")
	}
}

func TestNewValidator_RequiresIssuer(t *testing.T) {
	_, err := NewValidator(context.Background(), Config{})
	if err == nil {
		t.Fatal("expected error for empty issuer")
	}
	if !strings.Contains(err.Error(), "issuer is required") {
		t.Errorf("unhelpful error for missing issuer: %v", err)
	}
}

// TestJsonAudience_Unmarshal exercises the OIDC aud-as-string-or-array
// normalisation in isolation, so a regression in the normalisation is
// flagged without needing the full validator round-trip.
func TestJsonAudience_Unmarshal(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"string", `"single"`, []string{"single"}},
		{"slice", `["a","b"]`, []string{"a", "b"}},
		{"empty slice", `[]`, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var a jsonAudience
			if err := json.Unmarshal([]byte(c.raw), &a); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint([]string(a)) != fmt.Sprint(c.want) {
				t.Errorf("got %v, want %v", []string(a), c.want)
			}
		})
	}

	t.Run("bad input", func(t *testing.T) {
		var a jsonAudience
		if err := json.Unmarshal([]byte(`123`), &a); err == nil {
			t.Errorf("expected error for non-string-or-array input")
		}
	})
}
