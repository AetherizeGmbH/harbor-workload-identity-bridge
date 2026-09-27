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

func newFixtureIssuer(t testing.TB) *fixtureIssuer {
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

// standardClaims returns claims shaped like the token kubelet requests
// for the credential provider: one hour, bound to a pod (ADR-0028), iss
// bound to the fixture. Tests override fields they want to test against.
func (fi *fixtureIssuer) standardClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":           fi.URL(),
		"aud":           []string{"harbor.example.com"},
		"sub":           "system:serviceaccount:flux-system:source-controller",
		"iat":           now.Unix(),
		"nbf":           now.Unix(),
		"exp":           now.Add(time.Hour).Unix(),
		"kubernetes.io": podBinding(),
	}
}

// podBinding is the kubernetes.io claim of a token bound to a pod.
func podBinding() map[string]any {
	return map[string]any{
		"namespace":      "flux-system",
		"pod":            map[string]any{"name": "source-controller-7d9f", "uid": "3f2c0a1e"},
		"node":           map[string]any{"name": "node-a", "uid": "9e1b"},
		"serviceaccount": map[string]any{"name": "source-controller", "uid": "5a7d"},
	}
}

// newValidatorFor constructs a Validator pointed at the fixture, with the
// default token policy (one-hour cap, pod binding required). The
// fixture's httptest.Server.Client() transport is used so plain-HTTP
// JWKS discovery works in tests without disabling TLS verification.
func newValidatorFor(t *testing.T, fi *fixtureIssuer) Validator {
	t.Helper()
	return newValidatorWith(t, fi, Config{MaxTokenLifetime: time.Hour})
}

// newValidatorWith is newValidatorFor with the caller's token policy.
func newValidatorWith(t *testing.T, fi *fixtureIssuer, policy Config) Validator {
	t.Helper()
	v, err := NewValidator(context.Background(), Config{
		Issuer:                 fi.URL(),
		HTTPClient:             fi.server.Client(),
		MaxTokenLifetime:       policy.MaxTokenLifetime,
		AllowNonPodBoundTokens: policy.AllowNonPodBoundTokens,
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
	if got := claims.Expiry.Sub(claims.IssuedAt); got != time.Hour {
		t.Errorf("Expiry - IssuedAt = %s, want 1h", got)
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
		Issuer:           "http://127.0.0.1:1/nonexistent",
		HTTPClient:       &http.Client{Timeout: time.Second},
		MaxTokenLifetime: time.Hour,
	})
	if err == nil {
		t.Fatal("expected NewValidator to fail on unreachable issuer")
	}
	if !strings.Contains(err.Error(), "discovery") {
		t.Errorf("error should come from discovery: %v", err)
	}
}

// A JWKS URL, CA or RBAC that does not work must stop the bridge at
// startup, like a failed discovery, not deny every request later.
func TestNewValidator_FailsWhenTheSigningKeysCannotBeFetched(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer failing.Close()
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"unreachable JWKS URL", Config{Issuer: "https://kubernetes.default.svc", JWKSURL: "http://127.0.0.1:1/keys"}},
		{"JWKS URL refuses the bridge", Config{Issuer: "https://kubernetes.default.svc", JWKSURL: failing.URL + "/openid/v1/jwks"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.MaxTokenLifetime = time.Hour
			_, err := NewValidator(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), "token signing keys") {
				t.Fatalf("err = %v, want the signing-key fetch failure", err)
			}
		})
	}
	t.Run("discovered jwks_uri fails", func(t *testing.T) {
		mux := http.NewServeMux()
		var srv *httptest.Server
		mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": srv.URL, "jwks_uri": srv.URL + "/keys"})
		})
		mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
		srv = httptest.NewServer(mux)
		defer srv.Close()
		_, err := NewValidator(context.Background(), Config{Issuer: srv.URL, MaxTokenLifetime: time.Hour})
		if err == nil || !strings.Contains(err.Error(), "token signing keys") {
			t.Fatalf("err = %v, want the signing-key fetch failure", err)
		}
	})
}

// A token whose key the bridge does not hold, while the JWKS cannot be
// fetched, is unavailable (503), not invalid: it may be signed by a key
// the issuer just rotated in. A key the bridge holds that does not verify
// the signature stays a bad signature.
func TestValidator_UnavailableSigningKeysAreNotInvalidTokens(t *testing.T) {
	fi := newFixtureIssuer(t)
	srv := newJWKSServer(t, serveBody(jwksBody(t, fi.key, fi.kid)))
	clock := newFakeClock()
	v, err := NewValidator(context.Background(), Config{
		Issuer: fi.URL(), JWKSURL: srv.URL, MaxTokenLifetime: time.Hour,
		HTTPClient: &http.Client{Timeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	gv, ok := v.(*goOIDCValidator)
	if !ok {
		t.Fatalf("validator is %T", v)
	}
	gv.keys.now = clock.now
	rotated := forgedToken(t, fi, "rotated-in")
	unavailable := func(t *testing.T, ctx context.Context, token string) {
		t.Helper()
		_, err := v.Validate(ctx, token)
		if !errors.Is(err, ErrSigningKeysUnavailable) || errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrSigningKeysUnavailable and not ErrInvalidToken", err)
		}
		if c := classifyOIDCError(err); c != OIDCReasonKeysUnavailable {
			t.Fatalf("category = %q, want %q", c, OIDCReasonKeysUnavailable)
		}
	}
	invalid := func(t *testing.T, token string) {
		t.Helper()
		_, err := v.Validate(context.Background(), token)
		if !errors.Is(err, ErrInvalidToken) || errors.Is(err, ErrSigningKeysUnavailable) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
		if c := classifyOIDCError(err); c != OIDCReasonBadSignature {
			t.Fatalf("category = %q, want %q", c, OIDCReasonBadSignature)
		}
	}

	srv.set(serveStatus(http.StatusForbidden))
	clock.advance(jwksMinRefresh + time.Second)
	t.Run("unknown key, the refresh fails", func(t *testing.T) { unavailable(t, context.Background(), rotated) })
	t.Run("unknown key, rate-limited after the failure", func(t *testing.T) { unavailable(t, context.Background(), rotated) })
	t.Run("known key, wrong signature", func(t *testing.T) {
		invalid(t, fi.signTokenWithOtherKey(t, fi.standardClaims()))
	})
	t.Run("known key still verifies", func(t *testing.T) {
		if _, err := v.Validate(context.Background(), fi.signToken(t, fi.standardClaims())); err != nil {
			t.Fatal(err)
		}
	})

	srv.set(srv.hang)
	clock.advance(jwksMinRefresh + time.Second)
	t.Run("unknown key, the request ends during the refresh", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		unavailable(t, ctx, rotated)
	})

	srv.set(serveBody(jwksBody(t, fi.key, fi.kid)))
	eventually(t, "the hanging fetch to end", func() bool {
		gv.keys.mu.Lock()
		defer gv.keys.mu.Unlock()
		return gv.keys.inflight == nil
	})
	clock.advance(jwksMinRefresh + time.Second)
	t.Run("unknown key against a current key set", func(t *testing.T) { invalid(t, rotated) })
}

func TestNewValidator_RequiresPositiveMaxTokenLifetime(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Hour} {
		_, err := NewValidator(context.Background(), Config{Issuer: "https://kubernetes.default.svc", MaxTokenLifetime: d})
		if err == nil || !strings.Contains(err.Error(), "max token lifetime must be positive") {
			t.Errorf("MaxTokenLifetime=%s: err = %v, want the positive-lifetime refusal", d, err)
		}
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

// TestNewValidator_JWKSURL_SkipsDiscoveryAndValidatesCustomIssuer simulates
// the off-cluster topology (local dev via `kubectl proxy`, prod with the
// bridge behind an internal LB) where the issuer string tokens advertise
// is not reachable but a separate JWKS URL is. The constructor must skip
// discovery, fetch JWKS directly, and still verify tokens against the
// configured Issuer string.
func TestNewValidator_JWKSURL_SkipsDiscoveryAndValidatesCustomIssuer(t *testing.T) {
	fi := newFixtureIssuer(t)
	const declaredIssuer = "https://kubernetes.default.svc.cluster.local"

	// Sanity-check: discovery is NOT reached. Wire a fail-loud transport
	// to expose any accidental discovery fetch.
	disallowDiscovery := func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "openid-configuration") {
			t.Fatalf("discovery was hit at %s when JWKSURL is set", req.URL)
		}
		return http.DefaultTransport.RoundTrip(req)
	}
	client := &http.Client{Transport: roundTripperFunc(disallowDiscovery)}

	v, err := NewValidator(context.Background(), Config{
		Issuer:           declaredIssuer,
		JWKSURL:          fi.URL() + "/keys",
		HTTPClient:       client,
		MaxTokenLifetime: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	tok := fi.signToken(t, jwt.MapClaims{
		"iss":           declaredIssuer, // matches Config.Issuer, NOT the fixture's URL
		"sub":           "system:serviceaccount:flux-system:source-controller",
		"aud":           "harbor-bridge",
		"exp":           time.Now().Add(time.Hour).Unix(),
		"iat":           time.Now().Unix(),
		"kubernetes.io": podBinding(),
	})

	claims, err := v.Validate(context.Background(), tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.Issuer != declaredIssuer {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, declaredIssuer)
	}
}

// TestNewValidator_JWKSURL_RejectsTokensWithWrongIssuer confirms the
// discovery-skip path does NOT weaken issuer verification: a token whose
// iss claim does not match Config.Issuer must still be rejected even
// when JWKS is fetched from elsewhere.
func TestNewValidator_JWKSURL_RejectsTokensWithWrongIssuer(t *testing.T) {
	fi := newFixtureIssuer(t)
	const declaredIssuer = "https://kubernetes.default.svc.cluster.local"

	v, err := NewValidator(context.Background(), Config{
		Issuer:           declaredIssuer,
		JWKSURL:          fi.URL() + "/keys",
		MaxTokenLifetime: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	tok := fi.signToken(t, jwt.MapClaims{
		"iss":           "https://somewhere-else.example.com", // does not match declaredIssuer
		"sub":           "system:serviceaccount:x:y",
		"exp":           time.Now().Add(time.Hour).Unix(),
		"iat":           time.Now().Unix(),
		"kubernetes.io": podBinding(),
	})

	_, err = v.Validate(context.Background(), tok)
	if err == nil {
		t.Fatal("expected Validate to reject token with mismatched issuer")
	}
	if got := classifyOIDCError(err); got != OIDCReasonWrongIssuer {
		t.Errorf("category = %q, want %q (%v)", got, OIDCReasonWrongIssuer, err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
