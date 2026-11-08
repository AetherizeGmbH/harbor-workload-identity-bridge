// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// gkeIssuer is the shape of a GKE cluster's ServiceAccount token issuer:
// not the URL the bridge reads the discovery document from.
const gkeIssuer = "https://container.googleapis.com/v1/projects/p/locations/europe-west3/clusters/c"

// fakeAPIServer is an in-cluster apiserver on an httptest TLS server: it
// serves a discovery document (doc, written as JSON) and the fixture's
// keys at /openid/v1/jwks, and records the Authorization header per path.
type fakeAPIServer struct {
	srv  *httptest.Server
	fi   *fixtureIssuer
	seen sync.Map // path -> Authorization
}

func newFakeAPIServer(t *testing.T, doc map[string]any) *fakeAPIServer {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return newFakeAPIServerRaw(t, body)
}

func newFakeAPIServerRaw(t *testing.T, doc []byte) *fakeAPIServer {
	t.Helper()
	a := &fakeAPIServer{fi: newFixtureIssuer(t)}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.seen.Store(r.URL.Path, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case discoveryPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(doc)
		case apiServerJWKSPath:
			a.fi.handleJWKS(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// client returns the bridge's OIDC client for the fake apiserver, with
// its CA and a token file, as the chart configures it.
func (a *fakeAPIServer) client(t *testing.T) *http.Client {
	t.Helper()
	c, err := NewOIDCHTTPClient(writeCAFile(t, a.srv), writeTokenFile(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (a *fakeAPIServer) authorization(path string) (string, bool) {
	v, ok := a.seen.Load(path)
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// apiServerDoc is the discovery document of a managed cluster's apiserver:
// its issuer and jwks_uri are cloud URLs, not the apiserver's.
func apiServerDoc(issuer string) map[string]any {
	return map[string]any{
		"issuer":                                issuer,
		"jwks_uri":                              issuer + "/jwks",
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
}

// ADR-0044: on a managed cluster the issuer differs from the URL the
// bridge reads the document from. DiscoverIssuer takes it, the keys come
// from the apiserver's own endpoint, and a validator built from the result
// accepts the cluster's tokens and no others.
func TestDiscoverIssuer_IssuerOtherThanTheAPIServer(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1") // the fake apiserver is in-cluster
	a := newFakeAPIServer(t, apiServerDoc(gkeIssuer))
	client := a.client(t)

	d, err := DiscoverIssuer(context.Background(), client, a.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := &IssuerDiscovery{
		DocumentURL: a.srv.URL + "/.well-known/openid-configuration",
		Issuer:      gkeIssuer,
		JWKSURL:     a.srv.URL + "/openid/v1/jwks",
		SigningAlgs: []string{"RS256"},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("DiscoverIssuer = %+v, want %+v", d, want)
	}

	v, err := NewValidator(context.Background(), Config{
		Issuer: d.Issuer, JWKSURL: d.JWKSURL, SigningAlgs: d.SigningAlgs,
		HTTPClient: client, MaxTokenLifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ok := a.fi.standardClaims()
	ok["iss"] = gkeIssuer
	if _, err := v.Validate(context.Background(), a.fi.signToken(t, ok)); err != nil {
		t.Fatalf("token of the discovered issuer: %v", err)
	}
	other := a.fi.standardClaims()
	other["iss"] = a.srv.URL // signed by the same key, another issuer
	_, err = v.Validate(context.Background(), a.fi.signToken(t, other))
	if !errors.Is(err, ErrInvalidToken) || classifyOIDCError(err) != OIDCReasonWrongIssuer {
		t.Fatalf("token of another issuer: err = %v (category %q), want %q", err, classifyOIDCError(err), OIDCReasonWrongIssuer)
	}

	// The bridge's token went to the in-cluster apiserver, for the
	// document and the keys; nothing else was requested.
	for _, p := range []string{discoveryPath, apiServerJWKSPath} {
		if got, _ := a.authorization(p); got != "Bearer BRIDGE-SA-TOKEN" {
			t.Errorf("%s: Authorization %q, want the bridge token", p, got)
		}
	}
	a.seen.Range(func(k, _ any) bool {
		if p, _ := k.(string); p != discoveryPath && p != apiServerJWKSPath {
			t.Errorf("unexpected request for %s", p)
		}
		return true
	})
}

// Why the bridge does not use go-oidc's NewProvider for this (ADR-0044):
// discovery at the apiserver's URL refuses the managed cluster's document.
func TestNewValidator_DiscoveryAtTheAPIServerRefusesAManagedClustersDocument(t *testing.T) {
	a := newFakeAPIServer(t, apiServerDoc(gkeIssuer))
	_, err := NewValidator(context.Background(), Config{Issuer: a.srv.URL, HTTPClient: a.client(t), MaxTokenLifetime: time.Hour})
	var mismatch *oidc.IssuerMismatchError
	if !errors.As(err, &mismatch) || mismatch.Discovered != gkeIssuer {
		t.Fatalf("err = %v, want go-oidc's issuer mismatch", err)
	}
}

func TestDiscoverIssuer_RefusesIssuers(t *testing.T) {
	for _, tc := range []struct {
		name, doc, want string
		secret          string // must not appear in the error
	}{
		{name: "no issuer", doc: `{"jwks_uri":"https://k/keys"}`, want: "names no issuer"},
		{name: "empty issuer", doc: `{"issuer":""}`, want: "names no issuer"},
		{name: "not JSON", doc: `<html>`, want: "decode"},
		{name: "issuer not a string", doc: `{"issuer":42}`, want: "decode"},
		{name: "plain http", doc: `{"issuer":"http://kubernetes.default.svc.cluster.local"}`, want: "not an https URL"},
		{name: "no scheme", doc: `{"issuer":"kubernetes.default.svc"}`, want: "not an https URL"},
		{name: "no host", doc: `{"issuer":"https:///clusters/c"}`, want: "no host"},
		{name: "user:password@", doc: `{"issuer":"https://admin:s3cret@issuer.example"}`, want: "contains an '@'", secret: "s3cret"},
		{name: "user@", doc: `{"issuer":"https://TOKEN123@issuer.example"}`, want: "contains an '@'", secret: "TOKEN123"},
		{name: "@ after the host", doc: `{"issuer":"https://issuer.example/a@b"}`, want: "contains an '@'"},
		{name: "query", doc: `{"issuer":"https://issuer.example/c?x=1"}`, want: "query or a fragment"},
		{name: "empty query", doc: `{"issuer":"https://issuer.example/c?"}`, want: "query or a fragment"},
		{name: "fragment", doc: `{"issuer":"https://issuer.example/c#f"}`, want: "query or a fragment"},
		{name: "upper-case scheme", doc: `{"issuer":"HTTPS://issuer.example"}`, want: "does not read back unchanged"},
		{name: "leading space", doc: `{"issuer":" https://issuer.example"}`, want: "not a URL"},
		{name: "space in the path", doc: `{"issuer":"https://issuer.example/a b"}`, want: "does not read back unchanged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newFakeAPIServerRaw(t, []byte(tc.doc))
			d, err := DiscoverIssuer(context.Background(), a.client(t), a.srv.URL)
			if err == nil {
				t.Fatalf("accepted %+v", d)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
				t.Errorf("err repeats the credential: %v", err)
			}
		})
	}
}

// Issuers the rules accept, including the AKS shape with its trailing
// slash, which must be kept byte for byte.
func TestDiscoverIssuer_AcceptsIssuers(t *testing.T) {
	for _, issuer := range []string{
		"https://kubernetes.default.svc.cluster.local",
		gkeIssuer,
		"https://oidc.eks.eu-central-1.amazonaws.com/id/0123456789ABCDEF0123456789ABCDEF",
		"https://westeurope.oic.prod-aks.azure.com/00000000-0000-0000-0000-000000000000/11111111-1111-1111-1111-111111111111/",
		"https://10.0.0.1:6443",
		"https://issuer.example/a%40b",
	} {
		t.Run(issuer, func(t *testing.T) {
			a := newFakeAPIServer(t, apiServerDoc(issuer))
			d, err := DiscoverIssuer(context.Background(), a.client(t), a.srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if d.Issuer != issuer {
				t.Fatalf("Issuer = %q, want %q", d.Issuer, issuer)
			}
		})
	}
}

func TestDiscoverIssuer_DocumentSizeBound(t *testing.T) {
	doc := func(size int) []byte {
		prefix := `{"issuer":"` + gkeIssuer + `","padding":"`
		suffix := `"}`
		return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
	}
	a := newFakeAPIServerRaw(t, doc(maxDiscoverySize))
	if _, err := DiscoverIssuer(context.Background(), a.client(t), a.srv.URL); err != nil {
		t.Fatalf("document of exactly %d bytes: %v", maxDiscoverySize, err)
	}
	a = newFakeAPIServerRaw(t, doc(maxDiscoverySize+1))
	_, err := DiscoverIssuer(context.Background(), a.client(t), a.srv.URL)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("document of %d bytes: err = %v, want the size refusal", maxDiscoverySize+1, err)
	}
}

func TestDiscoverIssuer_RefusedRequest(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"kind":"Status","message":"forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()
	client, err := NewOIDCHTTPClient(writeCAFile(t, srv), writeTokenFile(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = DiscoverIssuer(context.Background(), client, srv.URL)
	if err == nil || !strings.Contains(err.Error(), "403 Forbidden") || !strings.Contains(err.Error(), discoveryPath) {
		t.Fatalf("err = %v, want the status and the URL", err)
	}
}

// A redirect is not followed: its target would get the bridge's token
// (if allowed) or could serve another issuer.
func TestDiscoverIssuer_DoesNotFollowRedirects(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
	target := newFakeAPIServer(t, apiServerDoc(gkeIssuer))
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL+r.URL.Path, http.StatusFound)
	}))
	defer api.Close()
	client, err := NewOIDCHTTPClient(writeCAFile(t, api), writeTokenFile(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = DiscoverIssuer(context.Background(), client, api.URL)
	if err == nil || !strings.Contains(err.Error(), "302 Found") {
		t.Fatalf("err = %v, want the redirect status", err)
	}
	if _, hit := target.authorization(discoveryPath); hit {
		t.Fatal("the redirect target was requested")
	}
}

// The bridge's token goes with the request only to the in-cluster
// apiserver and only over https, as for the key fetches.
func TestDiscoverIssuer_SendsTokenOnlyToTheInClusterAPIServer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inCluster bool
		want      string
	}{
		{"in-cluster apiserver", true, "Bearer BRIDGE-SA-TOKEN"},
		{"other host", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.inCluster {
				t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
			} else {
				t.Setenv("KUBERNETES_SERVICE_HOST", "")
			}
			a := newFakeAPIServer(t, apiServerDoc(gkeIssuer))
			if _, err := DiscoverIssuer(context.Background(), a.client(t), a.srv.URL); err != nil {
				t.Fatal(err)
			}
			if got, ok := a.authorization(discoveryPath); !ok || got != tc.want {
				t.Errorf("Authorization %q (requested: %v), want %q", got, ok, tc.want)
			}
		})
	}

	t.Run("plain http to the in-cluster address", func(t *testing.T) {
		t.Setenv("KUBERNETES_SERVICE_HOST", "127.0.0.1")
		var auth sync.Map
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth.Store("v", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(apiServerDoc(gkeIssuer))
		}))
		defer srv.Close()
		client, err := NewOIDCHTTPClient("", writeTokenFile(t))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DiscoverIssuer(context.Background(), client, srv.URL); err != nil {
			t.Fatal(err)
		}
		if got, _ := auth.Load("v"); got != "" {
			t.Fatalf("token sent over plain http: %q", got)
		}
	})
}

func TestDiscoverIssuer_RefusesPlainHTTPToAnotherHost(t *testing.T) {
	_, err := DiscoverIssuer(context.Background(), nil, "http://10.0.0.5:8001")
	if err == nil || !strings.Contains(err.Error(), "plain http is allowed only to a loopback host") {
		t.Fatalf("err = %v, want the plain-http refusal", err)
	}
}

func TestDiscoverIssuer_EndsWithTheContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := DiscoverIssuer(ctx, srv.Client(), srv.URL)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("returned after %s", d)
	}
}

// The algorithms come from the document (ADR-0044): those the key set can
// verify, RS256 when there are none of them.
func TestDiscoverIssuer_SigningAlgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		algs any // nil: the field is absent
		want []string
	}{
		{"RSA key", []string{"RS256"}, []string{"RS256"}},
		{"ECDSA key", []string{"ES256"}, []string{"ES256"}},
		{"two keys", []string{"ES384", "RS256"}, []string{"ES384", "RS256"}},
		{"unknown and symmetric ones dropped", []string{"HS256", "none", "XX1", "ES256"}, []string{"ES256"}},
		{"none known", []string{"HS256"}, []string{"RS256"}},
		{"absent", nil, []string{"RS256"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := map[string]any{"issuer": gkeIssuer}
			if tc.algs != nil {
				doc["id_token_signing_alg_values_supported"] = tc.algs
			}
			a := newFakeAPIServer(t, doc)
			d, err := DiscoverIssuer(context.Background(), a.client(t), a.srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(d.SigningAlgs, tc.want) {
				t.Fatalf("SigningAlgs = %v, want %v", d.SigningAlgs, tc.want)
			}
		})
	}
}

// With a JWKS URL, Config.SigningAlgs limits the algorithms: an apiserver
// that advertises ES256 has its ES256 tokens accepted, and an RS256 token,
// even one an RSA key in the set verifies, is refused.
func TestValidator_JWKSURLWithSigningAlgs(t *testing.T) {
	ei := newECIssuer(t)
	const issuer = gkeIssuer
	v, err := NewValidator(context.Background(), Config{
		Issuer: issuer, JWKSURL: ei.server.URL + "/keys", SigningAlgs: []string{"ES256"}, MaxTokenLifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(context.Background(), signES256(t, ei.key, "ec-1", kubeletClaims(issuer))); err != nil {
		t.Fatalf("ES256 token: %v", err)
	}

	fi := newFixtureIssuer(t) // one RSA key
	v, err = NewValidator(context.Background(), Config{
		Issuer: issuer, JWKSURL: fi.URL() + "/keys", SigningAlgs: []string{"ES256"}, MaxTokenLifetime: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := fi.standardClaims()
	claims["iss"] = issuer
	if _, err := v.Validate(context.Background(), fi.signToken(t, claims)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("RS256 token with only ES256 advertised: err = %v, want ErrInvalidToken", err)
	}
	// Without SigningAlgs the JWKS URL alone accepts every algorithm the
	// key set verifies, as before.
	v, err = NewValidator(context.Background(), Config{Issuer: issuer, JWKSURL: fi.URL() + "/keys", MaxTokenLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(context.Background(), fi.signToken(t, claims)); err != nil {
		t.Fatalf("RS256 token without SigningAlgs: %v", err)
	}
}
