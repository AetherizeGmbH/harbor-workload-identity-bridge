// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// testClient is an apiClient for srv with the token in a temp file.
func testClient(t *testing.T, srv *httptest.Server, token string) (*apiClient, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return newAPIClient(srv.URL, srv.Client(), path), path
}

// TestAPIClient_Requests: the paths, the patch's content type and field
// manager, the bearer token read for every request, and a 404.
func TestAPIClient_Requests(t *testing.T) {
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+r.Header.Get("Authorization")+" "+r.Header.Get("Content-Type")+" "+string(body))
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/daemonsets/missing"):
			http.Error(w, `{"kind":"Status","message":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/daemonsets/"):
			_, _ = io.WriteString(w, `{"metadata":{"generation":4},"spec":{"selector":{"matchLabels":{"app":"plugin"}}},"status":{"desiredNumberScheduled":3,"numberUnavailable":1}}`)
		case r.Method == http.MethodPatch:
			_, _ = io.WriteString(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/pods"):
			_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"p"},"spec":{"nodeName":"n1"}}]}`)
		default:
			http.Error(w, `{"message":"forbidden by the test"}`, http.StatusForbidden)
		}
	}))
	defer srv.Close()
	c, tokenPath := testClient(t, srv, "first")
	ctx := context.Background()

	ds, err := c.getDaemonSet(ctx, "ns", "plugin")
	if err != nil || ds.Metadata.Generation != 4 || ds.Status.DesiredNumberScheduled != 3 || ds.Status.NumberUnavailable != 1 || ds.Spec.Selector.MatchLabels["app"] != "plugin" {
		t.Fatalf("daemonset = %+v, %v", ds, err)
	}
	if err := os.WriteFile(tokenPath, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.patchDaemonSet(ctx, "ns", "plugin", []byte(`{"spec":{}}`)); err != nil {
		t.Fatal(err)
	}
	pods, err := c.listPods(ctx, "ns", map[string]string{"app": "plugin"})
	if err != nil || len(pods) != 1 || pods[0].Spec.NodeName != "n1" {
		t.Fatalf("pods = %+v, %v", pods, err)
	}
	if _, err := c.getDaemonSet(ctx, "ns", "missing"); !errors.Is(err, errNotFound) {
		t.Fatalf("missing daemonset: %v", err)
	}
	if err := c.do(ctx, http.MethodDelete, "/x", "", nil, nil); err == nil || !strings.Contains(err.Error(), "forbidden by the test") {
		t.Fatalf("error status: %v", err)
	}
	want := []string{
		"GET /apis/apps/v1/namespaces/ns/daemonsets/plugin Bearer first  ",
		"PATCH /apis/apps/v1/namespaces/ns/daemonsets/plugin?fieldManager=harbor-bridge-uninstall-hook Bearer rotated application/strategic-merge-patch+json " + `{"spec":{}}`,
		"GET /api/v1/namespaces/ns/pods?labelSelector=app%3Dplugin Bearer rotated  ",
	}
	for i, w := range want {
		if seen[i] != w {
			t.Errorf("request %d = %q, want %q", i, seen[i], w)
		}
	}
}

// TestAPIClient_RefusesRedirects: the token never follows a redirect to
// another host.
func TestAPIClient_RefusesRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, _ := testClient(t, srv, "secret")
	if _, err := c.getDaemonSet(context.Background(), "ns", "plugin"); !errors.Is(err, errRedirect) {
		t.Fatalf("got %v, want a refused redirect", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the request followed the redirect")
	}
}

func TestNewInClusterClient_NeedsTheServiceEnv(t *testing.T) {
	if _, err := newInClusterClient(func(string) string { return "" }); err == nil {
		t.Fatal("accepted without KUBERNETES_SERVICE_HOST")
	}
}
