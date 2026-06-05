// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestBridge stands up an httptest TLS server with the given handler
// and returns a bridgeClient pointed at it. server.Client() already
// trusts the test cert, so we sidestep the CA-bundle plumbing here and
// cover it separately if it ever matters.
func newTestBridge(t *testing.T, handler http.HandlerFunc) (*bridgeClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	hc := srv.Client()
	hc.Timeout = 5 * time.Second // tests must never block on default timeouts
	return newBridgeClientWithHTTPClient(srv.URL, hc), srv
}

func TestFetch_OK(t *testing.T) {
	bc, _ := newTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.URL.Path; got != credentialsPath {
			t.Errorf("path = %s, want %s", got, credentialsPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer the-token" {
			t.Errorf("Authorization = %q", got)
		}
		var body struct {
			Image string `json:"image"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Image != "harbor.example.com/x:1" {
			t.Errorf("image in body = %q", body.Image)
		}
		_ = json.NewEncoder(w).Encode(bridgeResponse{
			Username: "u", Password: "p", ExpiresInSecs: 60, CacheKeyType: "Image",
		})
	})

	resp, err := bc.fetch("harbor.example.com/x:1", "the-token")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if resp.Username != "u" || resp.Password != "p" || resp.ExpiresInSecs != 60 {
		t.Errorf("response = %+v", resp)
	}
}

func TestFetch_Unauthorized_MapsToRefused(t *testing.T) {
	bc, _ := newTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
	})
	_, err := bc.fetch("harbor.example.com/x:1", "tok")
	if !errors.Is(err, errBridgeRefused) {
		t.Fatalf("want errBridgeRefused, got %v", err)
	}
}

func TestFetch_Forbidden_MapsToRefused(t *testing.T) {
	bc, _ := newTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no matching HarborAccess", http.StatusForbidden)
	})
	_, err := bc.fetch("harbor.example.com/x:1", "tok")
	if !errors.Is(err, errBridgeRefused) {
		t.Fatalf("want errBridgeRefused, got %v", err)
	}
}

func TestFetch_503ThenOK_RetriesOnce(t *testing.T) {
	var calls atomic.Int32
	bc, _ := newTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "rotating", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(bridgeResponse{
			Username: "u", Password: "p", ExpiresInSecs: 60, CacheKeyType: "Image",
		})
	})

	start := time.Now()
	resp, err := bc.fetch("harbor.example.com/x:1", "tok")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if resp.Username != "u" {
		t.Errorf("want username u, got %q", resp.Username)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server hit %d times, want 2", got)
	}
	if elapsed := time.Since(start); elapsed < retryBackoff {
		t.Errorf("retry happened too fast (%v), expected >= %v sleep", elapsed, retryBackoff)
	}
}

func TestFetch_503Twice_MapsToUnavailable(t *testing.T) {
	var calls atomic.Int32
	bc, _ := newTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "still rotating", http.StatusServiceUnavailable)
	})

	_, err := bc.fetch("harbor.example.com/x:1", "tok")
	if !errors.Is(err, errBridgeUnavailable) {
		t.Fatalf("want errBridgeUnavailable, got %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("server hit %d times, want 2 (initial + one retry)", got)
	}
}

func TestFetch_500_IsNotRetried(t *testing.T) {
	var calls atomic.Int32
	bc, _ := newTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "explode", http.StatusInternalServerError)
	})

	_, err := bc.fetch("harbor.example.com/x:1", "tok")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, errBridgeRefused) || errors.Is(err, errBridgeUnavailable) {
		t.Errorf("500 must not be classified as refused or unavailable: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server hit %d times; 500 should not retry", got)
	}
}

func TestFetch_200WithEmptyCreds_Errors(t *testing.T) {
	bc, _ := newTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(bridgeResponse{
			Username: "", Password: "", ExpiresInSecs: 60, CacheKeyType: "Image",
		})
	})
	_, err := bc.fetch("harbor.example.com/x:1", "tok")
	if err == nil || !strings.Contains(err.Error(), "empty username") {
		t.Fatalf("want empty-username error, got %v", err)
	}
}

func TestFetch_200WithGarbageBody_Errors(t *testing.T) {
	bc, _ := newTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	})
	_, err := bc.fetch("harbor.example.com/x:1", "tok")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("want decode error, got %v", err)
	}
}

func TestFetch_Unreachable_MapsToUnavailable(t *testing.T) {
	// Point at a TLS server that's already closed — the dial will fail.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	bc := newBridgeClientWithHTTPClient(srv.URL, srv.Client())
	srv.Close()

	_, err := bc.fetch("harbor.example.com/x:1", "tok")
	if !errors.Is(err, errBridgeUnavailable) {
		t.Fatalf("want errBridgeUnavailable, got %v", err)
	}
}

func TestBodySnippet(t *testing.T) {
	long := strings.Repeat("x", maxBodySnippetLen+50)
	out := bodySnippet([]byte(long))
	if !strings.HasSuffix(out, "…") {
		t.Errorf("long body should be ellipsised; got %q", out)
	}
	if len(out) > maxBodySnippetLen+len("…") {
		t.Errorf("snippet too long: %d", len(out))
	}
	if got := bodySnippet([]byte("  short  ")); got != "short" {
		t.Errorf("trim failed: %q", got)
	}
}
