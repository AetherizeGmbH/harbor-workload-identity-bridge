// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// tokenFakeHarbor records calls to /service/token and lets each test
// dictate the response.
type tokenFakeHarbor struct {
	server *httptest.Server

	// inspected request fields
	gotMethod     string
	gotPath       string
	gotAuthHeader string
	gotQuery      url.Values

	// dictated response
	respStatus int
	respBody   string
}

func newTokenFakeHarbor(t *testing.T) *tokenFakeHarbor {
	t.Helper()
	fh := &tokenFakeHarbor{respStatus: http.StatusOK}
	fh.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fh.gotMethod = r.Method
		fh.gotPath = r.URL.Path
		fh.gotAuthHeader = r.Header.Get("Authorization")
		fh.gotQuery = r.URL.Query()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fh.respStatus)
		_, _ = w.Write([]byte(fh.respBody))
	}))
	t.Cleanup(fh.server.Close)
	return fh
}

func (fh *tokenFakeHarbor) URL() *url.URL {
	u, _ := url.Parse(fh.server.URL)
	return u
}

func (fh *tokenFakeHarbor) client(t *testing.T) HarborTokenClient {
	t.Helper()
	c, err := NewHarborTokenClient(fh.URL(), fh.server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// expectBasicAuth decodes the Authorization header value back to user:pass.
func expectBasicAuth(t *testing.T, header, wantUser, wantPass string) {
	t.Helper()
	if !strings.HasPrefix(header, "Basic ") {
		t.Errorf("Authorization header missing Basic prefix: %q", header)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
	if err != nil {
		t.Errorf("Authorization header not base64: %v", err)
		return
	}
	got := string(raw)
	want := wantUser + ":" + wantPass
	if got != want {
		t.Errorf("Authorization decoded to %q, want %q", got, want)
	}
}

// ----------------------------------------------------------------------------

func TestMint_HappyPath(t *testing.T) {
	fh := newTokenFakeHarbor(t)
	body, _ := json.Marshal(map[string]any{
		"token":      "fake.jwt.value",
		"expires_in": 1800,
		"issued_at":  "2026-05-11T12:00:00Z",
	})
	fh.respBody = string(body)

	tok, err := fh.client(t).Mint(
		context.Background(),
		"robot$bridge-prod-flux", "secret-pw", "harbor-registry",
		[]Scope{{Type: "repository", Resource: "production/myimg", Actions: []string{"pull"}}},
	)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok.Token != "fake.jwt.value" {
		t.Errorf("Token = %q", tok.Token)
	}
	if tok.ExpiresIn != 30*time.Minute {
		t.Errorf("ExpiresIn = %v, want 30m", tok.ExpiresIn)
	}
	if fh.gotMethod != http.MethodGet {
		t.Errorf("HTTP method = %q, want GET", fh.gotMethod)
	}
	if fh.gotPath != "/service/token" {
		t.Errorf("Path = %q, want /service/token", fh.gotPath)
	}
}

func TestMint_SendsBasicAuth(t *testing.T) {
	fh := newTokenFakeHarbor(t)
	fh.respBody = `{"token":"x","expires_in":60}`

	_, err := fh.client(t).Mint(context.Background(),
		"robot$bridge-prod-flux", "p@ss:word!", "harbor-registry",
		[]Scope{{Type: "repository", Resource: "production/img", Actions: []string{"pull"}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	expectBasicAuth(t, fh.gotAuthHeader, "robot$bridge-prod-flux", "p@ss:word!")
}

func TestMint_EncodesScopes(t *testing.T) {
	fh := newTokenFakeHarbor(t)
	fh.respBody = `{"token":"x","expires_in":60}`

	scopes := []Scope{
		{Type: "repository", Resource: "production/img", Actions: []string{"pull"}},
		{Type: "repository", Resource: "shared/base", Actions: []string{"pull", "push"}},
	}
	_, err := fh.client(t).Mint(context.Background(), "u", "p", "harbor-registry", scopes)
	if err != nil {
		t.Fatal(err)
	}

	if got := fh.gotQuery.Get("service"); got != "harbor-registry" {
		t.Errorf("service = %q", got)
	}
	scopeValues := fh.gotQuery["scope"]
	want := []string{
		"repository:production/img:pull",
		"repository:shared/base:pull,push",
	}
	if len(scopeValues) != len(want) {
		t.Fatalf("scope count = %d, want %d (got %v)", len(scopeValues), len(want), scopeValues)
	}
	for i, w := range want {
		if scopeValues[i] != w {
			t.Errorf("scope[%d] = %q, want %q", i, scopeValues[i], w)
		}
	}
}

func TestMint_AcceptsAccessTokenAlias(t *testing.T) {
	// Some registries (and Harbor under some config) use access_token
	// instead of token. The client must accept both.
	fh := newTokenFakeHarbor(t)
	fh.respBody = `{"access_token":"alias.jwt","expires_in":120}`

	tok, err := fh.client(t).Mint(context.Background(), "u", "p", "harbor-registry",
		[]Scope{{Type: "repository", Resource: "p/i", Actions: []string{"pull"}}})
	if err != nil {
		t.Fatal(err)
	}
	if tok.Token != "alias.jwt" {
		t.Errorf("Token = %q", tok.Token)
	}
}

func TestMint_AppliesDefaultExpiryWhenMissing(t *testing.T) {
	// Older Harbor versions omit expires_in. The cache TTL must not be
	// zero — otherwise entries are evicted on the first Get.
	fh := newTokenFakeHarbor(t)
	fh.respBody = `{"token":"x"}`

	tok, err := fh.client(t).Mint(context.Background(), "u", "p", "harbor-registry",
		[]Scope{{Type: "repository", Resource: "p/i", Actions: []string{"pull"}}})
	if err != nil {
		t.Fatal(err)
	}
	if tok.ExpiresIn <= 0 {
		t.Errorf("ExpiresIn = %v, want > 0", tok.ExpiresIn)
	}
}

func TestMint_401IsErrTokenAuth(t *testing.T) {
	fh := newTokenFakeHarbor(t)
	fh.respStatus = http.StatusUnauthorized
	fh.respBody = `{"errors":[{"code":"UNAUTHORIZED","message":"invalid credentials"}]}`

	_, err := fh.client(t).Mint(context.Background(), "u", "p", "harbor-registry",
		[]Scope{{Type: "repository", Resource: "p/i", Actions: []string{"pull"}}})
	if !errors.Is(err, ErrTokenAuth) {
		t.Errorf("expected ErrTokenAuth, got %v", err)
	}
}

func TestMint_5xxIsWrappedError(t *testing.T) {
	fh := newTokenFakeHarbor(t)
	fh.respStatus = http.StatusInternalServerError
	fh.respBody = `harbor is on fire`

	_, err := fh.client(t).Mint(context.Background(), "u", "p", "harbor-registry",
		[]Scope{{Type: "repository", Resource: "p/i", Actions: []string{"pull"}}})
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if errors.Is(err, ErrTokenAuth) {
		t.Errorf("500 should not be ErrTokenAuth: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention status: %v", err)
	}
}

func TestMint_RejectsEmptyTokenResponse(t *testing.T) {
	// Defensive: a 200 response missing both token and access_token must
	// fail rather than caching empty creds.
	fh := newTokenFakeHarbor(t)
	fh.respBody = `{"expires_in":60}`

	_, err := fh.client(t).Mint(context.Background(), "u", "p", "harbor-registry",
		[]Scope{{Type: "repository", Resource: "p/i", Actions: []string{"pull"}}})
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestMint_RejectsEmptyCreds(t *testing.T) {
	fh := newTokenFakeHarbor(t)
	cases := []struct {
		name, user, pass string
	}{
		{"empty user", "", "p"},
		{"empty pass", "u", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := fh.client(t).Mint(context.Background(), c.user, c.pass,
				"harbor-registry",
				[]Scope{{Type: "repository", Resource: "p/i", Actions: []string{"pull"}}})
			if err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestMint_StripsAPIBasePath(t *testing.T) {
	// If the caller passes harbor.aetherize/api/v2.0 (which the control
	// plane uses for the v2 SDK), Mint should still hit /service/token,
	// not /api/v2.0/service/token.
	fh := newTokenFakeHarbor(t)
	fh.respBody = `{"token":"x","expires_in":60}`
	u := fh.URL()
	u.Path = "/api/v2.0"

	c, err := NewHarborTokenClient(u, fh.server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Mint(context.Background(), "u", "p", "harbor-registry",
		[]Scope{{Type: "repository", Resource: "p/i", Actions: []string{"pull"}}}); err != nil {
		t.Fatal(err)
	}
	if fh.gotPath != "/service/token" {
		t.Errorf("Path = %q, want /service/token (API base must be stripped)", fh.gotPath)
	}
}

func TestScope_Encode(t *testing.T) {
	cases := []struct {
		in   Scope
		want string
	}{
		{Scope{Type: "repository", Resource: "production/img", Actions: []string{"pull"}}, "repository:production/img:pull"},
		{Scope{Type: "repository", Resource: "shared", Actions: []string{"pull", "push"}}, "repository:shared:pull,push"},
	}
	for _, c := range cases {
		if got := c.in.Encode(); got != c.want {
			t.Errorf("Encode = %q, want %q", got, c.want)
		}
	}
}
