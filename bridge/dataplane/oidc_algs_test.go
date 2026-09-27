// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
)

// ecIssuer serves discovery and a JWKS with one ECDSA P-256 key, as an
// apiserver with an ECDSA ServiceAccount signing key does.
type ecIssuer struct {
	server *httptest.Server
	key    *ecdsa.PrivateKey
}

func newECIssuer(t *testing.T) *ecIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ei := &ecIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                ei.server.URL,
			"jwks_uri":                              ei.server.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"ES256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &key.PublicKey, KeyID: "ec-1", Algorithm: "ES256", Use: "sig",
		}}})
	})
	ei.server = httptest.NewServer(mux)
	t.Cleanup(ei.server.Close)
	return ei
}

func signES256(t *testing.T, key *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func kubeletClaims(iss string) jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss": iss, "aud": "harbor-bridge", "sub": "system:serviceaccount:flux-system:source-controller",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		"kubernetes.io": podBinding(),
	}
}

// Kubernetes signs ServiceAccount tokens with ES256/384/512 when its
// signing key is ECDSA. Discovery advertises that; with a JWKS URL there
// is no discovery, and the verifier used to fall back to RS256 only.
func TestValidator_ECDSASignedTokens(t *testing.T) {
	ei := newECIssuer(t)
	const declared = "https://kubernetes.default.svc.cluster.local"
	for _, tc := range []struct {
		name string
		cfg  Config
		iss  string
	}{
		{"JWKS URL", Config{Issuer: declared, JWKSURL: ei.server.URL + "/keys"}, declared},
		{"discovery", Config{Issuer: ei.server.URL}, ei.server.URL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.MaxTokenLifetime = time.Hour
			v, err := NewValidator(context.Background(), tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.Validate(context.Background(), signES256(t, ei.key, "ec-1", kubeletClaims(tc.iss))); err != nil {
				t.Fatalf("ES256 token: %v", err)
			}
		})
	}
}

// Accepting every asymmetric algorithm in JWKS-URL mode must not open
// algorithm confusion: the algorithm stays bound to the key's type.
func TestValidator_JWKSURL_AlgorithmBoundToKeyType(t *testing.T) {
	fi := newFixtureIssuer(t) // one RSA key, kid test-key-1
	const declared = "https://kubernetes.default.svc.cluster.local"
	v, err := NewValidator(context.Background(), Config{Issuer: declared, JWKSURL: fi.URL() + "/keys", MaxTokenLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaPub, err := x509.MarshalPKIXPublicKey(&fi.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, kubeletClaims(declared))
	hs.Header["kid"] = fi.kid
	hsToken, err := hs.SignedString(rsaPub) // the RSA public key as an HMAC secret
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{
		"ES256 under the RSA key's kid":         signES256(t, ecKey, fi.kid, kubeletClaims(declared)),
		"HS256 keyed with the RSA public key":   hsToken,
		"ES256 by a key the JWKS does not list": signES256(t, ecKey, "ec-unknown", kubeletClaims(declared)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.Validate(context.Background(), token)
			if !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("err = %v, want ErrInvalidToken", err)
			}
		})
	}
}
