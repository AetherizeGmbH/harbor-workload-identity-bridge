// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRequireTLSUnlessLoopback(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://kubernetes.default.svc/openid/v1/jwks": true,
		"https://10.0.0.5:6443/openid/v1/jwks":          true,
		"http://127.0.0.1:8001/openid/v1/jwks":          true, // make proxy
		"http://127.1.2.3/keys":                         true,
		"http://[::1]:8001/keys":                        true,
		"http://localhost:8001/keys":                    true,
		"http://LOCALHOST/keys":                         true,
		"http://10.0.0.5:8001/openid/v1/jwks":           false,
		"http://kubernetes.default.svc/openid/v1/jwks":  false,
		"http://localhost.attacker.example/keys":        false,
		"http://127.0.0.1.nip.io/keys":                  false,
		"http://[::ffff:10.0.0.5]/keys":                 false,
		"ftp://127.0.0.1/keys":                          false,
		"kubernetes.default.svc/keys":                   false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := requireTLSUnlessLoopback(u); ok != (err == nil) {
			t.Errorf("%s: err = %v, want ok=%v", raw, err, ok)
		}
	}
}

// The signing keys decide every credential. A plain-http fetch from
// anywhere but the bridge's own host would let whoever sits on the path
// substitute them; NewValidator refuses before it fetches anything.
func TestNewValidator_RefusesPlainHTTPKeySources(t *testing.T) {
	var discovery *httptest.Server
	discovery = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": discovery.URL, "jwks_uri": "http://10.0.0.5:8001/openid/v1/jwks"})
	}))
	defer discovery.Close()
	for _, tc := range []struct {
		name, want string
		cfg        Config
	}{
		{"JWKS URL", "JWKS URL", Config{Issuer: "https://kubernetes.default.svc", JWKSURL: "http://10.0.0.5:8001/openid/v1/jwks"}},
		{"issuer used for discovery", "(discovery)", Config{Issuer: "http://10.0.0.5:8001"}},
		{"jwks_uri named by discovery", "names jwks_uri", Config{Issuer: discovery.URL}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.MaxTokenLifetime = time.Hour
			_, err := NewValidator(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), "plain http is allowed only to a loopback host") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want the plain-http refusal for the %s", err, tc.want)
			}
		})
	}
}

// With a JWKS URL the issuer is only the expected iss claim; nothing is
// fetched from it, so its scheme does not matter.
func TestNewValidator_IssuerNotFetchedWithJWKSURL(t *testing.T) {
	fi := newFixtureIssuer(t)
	const declared = "http://issuer.example"
	v, err := NewValidator(context.Background(), Config{Issuer: declared, JWKSURL: fi.URL() + "/keys", MaxTokenLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	claims := fi.standardClaims()
	claims["iss"] = declared
	if _, err := v.Validate(context.Background(), fi.signToken(t, claims)); err != nil {
		t.Fatal(err)
	}
}
