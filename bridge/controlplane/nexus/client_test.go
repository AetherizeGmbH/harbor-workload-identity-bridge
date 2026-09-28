// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// newTestClient connects a client to srv (an httptest server, plain http)
// with the fake's admin credentials.
func newTestClient(t *testing.T, f *fakeNexus, srv *httptest.Server, path string, opts ...Option) Client {
	t.Helper()
	opts = append([]Option{WithAllowInsecureHTTP()}, opts...)
	c, err := NewClient(mustParse(t, srv.URL+path), f.username, f.password, srv.Client().Transport, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func basicAuth(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

func TestNewClient_RefusesUnsafeOrInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name string
		url  string
		user string
		opts []Option
		want string
	}{
		{"plain http", "http://nexus.example.com", "admin", nil, "plain http"},
		{"credentials in URL", "https://admin:secret@nexus.example.com", "admin", nil, "must not contain credentials"},
		{"user name only in URL", "https://TOKEN@nexus.example.com", "admin", nil, "must not contain credentials"},
		{"other scheme", "ftp://nexus.example.com", "admin", nil, "must use https"},
		{"no host", "https:///service/rest", "admin", nil, "must include a host"},
		{"query", "https://nexus.example.com/?x=1", "admin", nil, "query or fragment"},
		{"fragment", "https://nexus.example.com/#x", "admin", nil, "query or fragment"},
		{"no credentials", "https://nexus.example.com", "", nil, "credentials are required"},
		{"zero timeout", "https://nexus.example.com", "admin", []Option{WithCallTimeout(0)}, "timeout must be positive"},
		{"zero response limit", "https://nexus.example.com", "admin", []Option{WithMaxResponseBytes(0)}, "limit must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(mustParse(t, tc.url), tc.user, "secret", nil, tc.opts...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewClient(%q) error = %v, want one containing %q", tc.url, err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "TOKEN") {
				t.Errorf("error discloses a credential: %v", err)
			}
		})
	}
	if _, err := NewClient(nil, "admin", "secret", nil); err == nil {
		t.Error("NewClient(nil) succeeded")
	}
}

func TestNewClient_AcceptsHTTPOnlyWhenAllowed(t *testing.T) {
	if _, err := NewClient(mustParse(t, "http://nexus.nexus.svc:8081"), "admin", "pw", nil, WithAllowInsecureHTTP()); err != nil {
		t.Fatalf("http with WithAllowInsecureHTTP: %v", err)
	}
	if _, err := NewClient(mustParse(t, "https://nexus.example.com"), "", "", nil,
		WithCredentialSource(func() (string, string, error) { return "a", "b", nil })); err != nil {
		t.Fatalf("https with a credential source: %v", err)
	}
}

// The API root is /service/rest below the configured base URL, which may
// carry a context path; a URL that already ends in /service/rest (with or
// without a trailing slash) is not doubled.
func TestClient_BasePath(t *testing.T) {
	for _, tc := range []struct{ prefix, configured string }{
		{"", ""},
		{"", "/"},
		{"/nexus", "/nexus"},
		{"/nexus", "/nexus/"},
		{"/nexus", "/nexus/service/rest"},
		{"/nexus", "/nexus/service/rest/"},
	} {
		t.Run(tc.configured, func(t *testing.T) {
			f := newFakeNexus(t)
			srv := f.server(tc.prefix)
			c := newTestClient(t, f, srv, tc.configured)
			if err := c.Status(context.Background()); err != nil {
				t.Fatalf("Status: %v", err)
			}
			if got, want := f.lastRequest().Path, tc.prefix+"/service/rest/v1/status"; got != want {
				t.Errorf("request path = %q, want %q", got, want)
			}
		})
	}
}

func TestClient_SendsHeadersAndBasicAuth(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	if _, err := c.ListUsers(context.Background(), "bridge-c."); err != nil {
		t.Fatal(err)
	}
	r := f.lastRequest()
	if r.Auth != basicAuth(f.username, f.password) {
		t.Errorf("Authorization = %q, want Basic auth of the admin credentials", r.Auth)
	}
	if r.Accept != "application/json" || r.UserAgent != userAgent {
		t.Errorf("Accept = %q, User-Agent = %q", r.Accept, r.UserAgent)
	}
}

// The status endpoints need no authentication (verified on Nexus 3.76.1);
// the client sends no credentials to them.
func TestClient_StatusEndpointsSendNoCredentials(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	if err := c.Status(context.Background()); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if ok, err := c.Writable(context.Background()); err != nil || !ok {
		t.Fatalf("Writable = %v, %v; want true, nil", ok, err)
	}
	f.mu.Lock()
	f.writable = false
	f.mu.Unlock()
	if ok, err := c.Writable(context.Background()); err != nil || ok {
		t.Fatalf("Writable on 503 = %v, %v; want false, nil", ok, err)
	}
	for _, r := range f.recordedRequests() {
		if r.Auth != "" {
			t.Errorf("%s %s carried Authorization %q", r.Method, r.Path, r.Auth)
		}
	}
}

func TestClient_StatusErrors(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	if err := c.Status(context.Background()); !errors.Is(err, ErrServer) {
		t.Errorf("Status on 503 = %v, want ErrServer", err)
	}
	f.setOverride(func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusInternalServerError)
		return true
	})
	if ok, err := c.Writable(context.Background()); !errors.Is(err, ErrServer) || ok {
		t.Errorf("Writable on 500 = %v, %v; want false, ErrServer", ok, err)
	}
}

// With a credential source the client asks it on every request, so
// rotated admin credentials take effect without a new client, and the
// static pair is never sent.
func TestClient_CredentialSourcePerRequest(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	var mu sync.Mutex
	password := f.password
	source := func() (string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		return f.username, password, nil
	}
	c, err := NewClient(mustParse(t, srv.URL), "static-user", "static-password", srv.Client().Transport,
		WithAllowInsecureHTTP(), WithCredentialSource(source))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRoles(context.Background(), "bridge-c."); err != nil {
		t.Fatalf("ListRoles: %v", err)
	}
	mu.Lock()
	password = "rotated"
	mu.Unlock()
	_, err = c.ListRoles(context.Background(), "bridge-c.")
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("ListRoles with rotated credentials = %v, want ErrUnauthorized from the fake", err)
	}
	reqs := f.recordedRequests()
	want := []string{basicAuth(f.username, f.password), basicAuth(f.username, "rotated")}
	if len(reqs) != len(want) {
		t.Fatalf("Nexus saw %d requests, want %d", len(reqs), len(want))
	}
	for i := range want {
		if reqs[i].Auth != want[i] {
			t.Errorf("request %d Authorization = %q, want %q", i, reqs[i].Auth, want[i])
		}
	}
}

// When the credentials cannot be read, the call fails before a request
// goes out; nothing falls back to stale or static credentials.
func TestClient_CredentialSourceErrorSendsNothing(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	readErr := errors.New("read /etc/bridge/nexus-admin/password: no such file or directory")
	c, err := NewClient(mustParse(t, srv.URL), "static-user", "static-password", srv.Client().Transport,
		WithAllowInsecureHTTP(), WithCredentialSource(func() (string, string, error) { return "", "", readErr }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetUser(context.Background(), "bridge-c.ns.sa_0123456789abcdef")
	if !errors.Is(err, readErr) || !strings.Contains(err.Error(), "nexus credentials") {
		t.Errorf("GetUser error = %v, want it to wrap the read error", err)
	}
	if got := f.recordedRequests(); len(got) != 0 {
		t.Errorf("Nexus saw %d requests, want none", len(got))
	}
}

// A redirect is refused: neither the admin credentials nor a request body
// (which may hold a new user's password) reach the target, whatever its
// scheme or host.
func TestClient_RefusesRedirects(t *testing.T) {
	var targetHits int
	var mu sync.Mutex
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetHits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFakeNexus(t)
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				http.Redirect(w, r, target.URL+r.URL.Path, status)
				return true
			}
			srv := f.server("")
			c := newTestClient(t, f, srv, "")
			_, err := c.CreateUser(context.Background(), User{
				UserID: "bridge-c.ns.sa_0123456789abcdef", FirstName: "f", LastName: "l",
				EmailAddress: "e@x.invalid", Status: UserActive, Roles: []string{"bridge-c.ns.sa"},
			}, "USER-PASSWORD-456")
			if err == nil || !strings.Contains(err.Error(), "refusing to follow a redirect") {
				t.Fatalf("CreateUser through a redirect = %v, want the redirect refused", err)
			}
			for _, secret := range []string{f.password, "USER-PASSWORD-456"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error discloses %q: %v", secret, err)
				}
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if targetHits != 0 {
		t.Errorf("the redirect target received %d requests, want none", targetHits)
	}
}

// A Nexus that accepts the request and never answers must not block the
// caller past the per-call deadline.
func TestClient_CallTimeoutBoundsAStalledNexus(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c, err := NewClient(mustParse(t, srv.URL), "admin", "pw", srv.Client().Transport,
		WithAllowInsecureHTTP(), WithCallTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = c.GetUser(context.Background(), "bridge-c.ns.sa_0123456789abcdef")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetUser against a stalled Nexus = %v, want a deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("GetUser took %s; the per-call deadline did not apply", elapsed)
	}
}

// A successful response larger than the limit is refused instead of being
// decoded into memory.
func TestClient_ResponseBodyIsBounded(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[" + strings.Repeat(`{"userId":"bridge-c.x","source":"default"},`, 1000) + `{"userId":"y"}]`))
		return true
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "", WithMaxResponseBytes(1024))
	_, err := c.ListUsers(context.Background(), "bridge-c.")
	if err == nil || !strings.Contains(err.Error(), "larger than 1024 bytes") {
		t.Fatalf("ListUsers of an oversized response = %v, want the size refusal", err)
	}
}

// A 200 that is not JSON (a login portal, the Nexus UI) is an error that
// points at the URL, not a decode failure.
func TestClient_RefusesNonJSONSuccess(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>Sign in</html>"))
		return true
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	_, err := c.GetRole(context.Background(), "bridge-c.ns.sa")
	if err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("GetRole of an HTML page = %v, want the content-type refusal", err)
	}
}

func TestClient_RefusesMalformedJSON(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":`))
		return true
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	if _, err := c.GetRole(context.Background(), "bridge-c.ns.sa"); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("GetRole of malformed JSON = %v, want a decode error", err)
	}
}

// Ids go into URL paths; anything that could be read as a separator, a
// traversal or an escape is refused before a request is sent.
func TestClient_RefusesUnsafeIDs(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	ctx := context.Background()
	for _, id := range []string{"", "..", ".hidden", "a/b", "a%2Fb", "a;b", "a b", "a?b", "a#b", "ä", strings.Repeat("a", 201)} {
		checks := map[string]error{
			"GetUser":    func() error { _, err := c.GetUser(ctx, id); return err }(),
			"ListUsers":  func() error { _, err := c.ListUsers(ctx, id); return err }(),
			"DeleteUser": c.DeleteUser(ctx, id),
			"UpdateUser": c.UpdateUser(ctx, User{UserID: id, Source: DefaultSource, Status: UserActive}),
			"CreateUser": func() error {
				_, err := c.CreateUser(ctx, User{UserID: id, Status: UserActive, Roles: []string{"r"}}, "pw")
				return err
			}(),
			"GetRole":                  func() error { _, err := c.GetRole(ctx, id); return err }(),
			"ListRoles":                func() error { _, err := c.ListRoles(ctx, id); return err }(),
			"CreateRole":               func() error { _, err := c.CreateRole(ctx, Role{ID: id, Name: "n"}); return err }(),
			"UpdateRole":               c.UpdateRole(ctx, Role{ID: id, Name: "n"}),
			"DeleteRole":               c.DeleteRole(ctx, id),
			"GetPrivilege":             func() error { _, err := c.GetPrivilege(ctx, id); return err }(),
			"role with that privilege": c.UpdateRole(ctx, Role{ID: "r", Name: "n", Privileges: []string{id}}),
			"user with that role": func() error {
				_, err := c.CreateUser(ctx, User{UserID: "u", Status: UserActive, Roles: []string{id}}, "pw")
				return err
			}(),
		}
		for name, err := range checks {
			if err == nil || !strings.Contains(err.Error(), "is not an id this client sends") {
				t.Errorf("%s(%q) = %v, want the id refused", name, id, err)
			}
		}
	}
	if got := f.recordedRequests(); len(got) != 0 {
		t.Errorf("Nexus saw %d requests, want none: %+v", len(got), got)
	}
}

// NewTransport trusts exactly the given CA bundle; without one the system
// roots apply, which do not trust a private CA. TLS 1.2 is the floor.
func TestNewTransport_PrivateCA(t *testing.T) {
	f := newFakeNexus(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// The refused handshake below is expected; keep it out of the output.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	rt, err := NewTransport(caPEM)
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := rt.(*http.Transport)
	if !ok || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || tr.ResponseHeaderTimeout != DefaultCallTimeout {
		t.Fatalf("transport = %#v, want TLS 1.2 floor and the header timeout", rt)
	}
	c, err := NewClient(mustParse(t, srv.URL), f.username, f.password, rt)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Status(context.Background()); err != nil {
		t.Fatalf("Status over TLS with the CA bundle: %v", err)
	}

	system, err := NewTransport(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err = NewClient(mustParse(t, srv.URL), f.username, f.password, system)
	if err != nil {
		t.Fatal(err)
	}
	var certErr *tls.CertificateVerificationError
	if err := c.Status(context.Background()); !errors.As(err, &certErr) {
		t.Fatalf("Status with system roots against a private CA = %v, want a verification error", err)
	}

	if _, err := NewTransport([]byte("not a certificate")); err == nil {
		t.Error("NewTransport accepted a bundle without certificates")
	}
}
