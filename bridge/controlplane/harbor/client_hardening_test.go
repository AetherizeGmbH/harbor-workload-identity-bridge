// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goharbor/go-client/pkg/sdk/v2.0/models"
)

// captureOutput redirects the process's stdout and stderr while fn runs
// and returns what was written to either.
func captureOutput(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	fn()
	os.Stdout, os.Stderr = stdout, stderr
	_ = w.Close()
	return string(<-done)
}

// The go-openapi runtime dumps every request and response when DEBUG or
// SWAGGER_DEBUG is set. The admin credentials (Authorization header) and
// robot passwords (create/refresh responses) must never reach the output.
func TestNewClient_DebugEnvDoesNotDumpSecrets(t *testing.T) {
	t.Setenv("DEBUG", "1")
	t.Setenv("SWAGGER_DEBUG", "1")
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "admin", "ADMIN-PASSWORD-123")

	var created *Robot
	var refreshed string
	out := captureOutput(t, func() {
		var err error
		created, err = c.Create(context.Background(), "bridge-c.ns.sa", "", []ProjectPermission{{Project: "p", Action: "pull"}})
		if err != nil {
			t.Error(err)
			return
		}
		refreshed, err = c.RefreshSecret(context.Background(), created.ID)
		if err != nil {
			t.Error(err)
		}
	})
	if created == nil || created.Secret == "" || refreshed == "" {
		t.Fatalf("setup: create/refresh returned no secret (created=%+v refreshed=%q)", created, refreshed)
	}
	for _, secret := range []string{"ADMIN-PASSWORD-123", "YWRtaW46QURNSU4tUEFTU1dPUkQtMTIz", created.Secret, refreshed} {
		if strings.Contains(out, secret) {
			t.Errorf("output contains a secret (%q):\n%s", secret, out)
		}
	}
}

// A Harbor that accepts the request and never answers must not block the
// caller past the per-call deadline.
func TestClient_CallTimeoutBoundsAStalledHarbor(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(u, "", "", srv.Client().Transport, WithCallTimeout(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = c.GetByName(context.Background(), "bridge-c.ns.sa")
	if err == nil {
		t.Fatal("GetByName against a stalled Harbor returned no error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("GetByName took %s; the per-call deadline did not apply", elapsed)
	}
}

func TestClient_Create_RejectsEmptySecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(&models.RobotCreated{ID: 7, Name: "robot$bridge-c.ns.sa"})
	}))
	defer srv.Close()
	c := newClientFor(t, srv, "", "")
	if _, err := c.Create(context.Background(), "bridge-c.ns.sa", "", []ProjectPermission{{Project: "p", Action: "pull"}}); err == nil || !strings.Contains(err.Error(), "no secret") {
		t.Fatalf("err = %v, want a no-secret error", err)
	}
}

// A Harbor (or proxy) that ignores the page parameter returns the same
// full page forever; the walk must stop.
func TestClient_List_StopsWhenPagingIsIgnored(t *testing.T) {
	page := make([]*models.Robot, pageSize)
	for i := range page {
		page[i] = &models.Robot{ID: int64(i + 1), Name: "robot$other"}
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(page); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body.Bytes())
	}))
	defer srv.Close()
	c := newClientFor(t, srv, "", "")
	if _, err := c.List(context.Background()); err == nil || !strings.Contains(err.Error(), "pages") {
		t.Fatalf("err = %v, want the page-cap error", err)
	}
}

func TestDefaultTransport_HasTimeoutsAndTLSFloor(t *testing.T) {
	tr, ok := defaultTransport().(*http.Transport)
	if !ok {
		t.Fatalf("defaultTransport is %T", defaultTransport())
	}
	if tr.ResponseHeaderTimeout <= 0 || tr.TLSHandshakeTimeout <= 0 {
		t.Errorf("timeouts: header=%s handshake=%s, want both set", tr.ResponseHeaderTimeout, tr.TLSHandshakeTimeout)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion < 0x0303 {
		t.Errorf("TLS floor not 1.2: %+v", tr.TLSClientConfig)
	}
}

func TestNewTransport_TrustsOnlyTheGivenCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := NewTransport(path)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatalf("server signed by the given CA refused: %v", err)
	}
	_ = resp.Body.Close()
	if _, err := NewTransport(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing CA file accepted")
	}
}
