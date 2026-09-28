// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// authRecorder is a Harbor stand-in that answers every robot listing with
// an empty page and records the Authorization header of each request.
type authRecorder struct {
	mu   sync.Mutex
	auth []string
}

func (a *authRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.auth = append(a.auth, r.Header.Get("Authorization"))
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (a *authRecorder) requests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auth...)
}

func basicAuth(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

// TestNewClient_CredentialSourcePerRequest: with a credential source the
// client asks it on every request, so rotated admin credentials take
// effect without a new client, and the static pair is never sent.
func TestNewClient_CredentialSourcePerRequest(t *testing.T) {
	rec := &authRecorder{}
	srv := rec.server(t)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	password := "first"
	source := func() (string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		return "robot$bridge", password, nil
	}
	c, err := NewClient(u, "static-user", "static-password", srv.Client().Transport, WithCredentialSource(source))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	mu.Lock()
	password = "second"
	mu.Unlock()
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("List after rotation: %v", err)
	}

	got := rec.requests()
	want := []string{basicAuth("robot$bridge", "first"), basicAuth("robot$bridge", "second")}
	if len(got) != len(want) {
		t.Fatalf("Harbor saw %d requests, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d Authorization = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestNewClient_CredentialSourceErrorSendsNothing: when the credentials
// cannot be read, the call fails before a request goes out; nothing falls
// back to stale or static credentials.
func TestNewClient_CredentialSourceErrorSendsNothing(t *testing.T) {
	rec := &authRecorder{}
	srv := rec.server(t)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	readErr := errors.New("read /etc/bridge/harbor-admin/password: no such file or directory")
	source := func() (string, string, error) { return "", "", readErr }
	c, err := NewClient(u, "static-user", "static-password", srv.Client().Transport, WithCredentialSource(source))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.List(context.Background())
	if err == nil {
		t.Fatal("List succeeded without credentials")
	}
	if !errors.Is(err, readErr) || !strings.Contains(err.Error(), "harbor credentials") {
		t.Errorf("List error = %v, want it to wrap the read error", err)
	}
	if got := rec.requests(); len(got) != 0 {
		t.Errorf("Harbor saw %d requests (Authorization %q), want none", len(got), got)
	}
}
