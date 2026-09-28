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
	"sync"
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

// A Harbor (or proxy) that ignores the keyset query (sort=id and the id
// range) returns the same first page forever, or robots out of ID order;
// either could hide robots, so the walk must stop with an error.
func TestClient_List_FailsWhenTheKeysetQueryIsIgnored(t *testing.T) {
	serve := func(page []*models.Robot) *httptest.Server {
		var body bytes.Buffer
		if err := json.NewEncoder(&body).Encode(page); err != nil {
			t.Fatal(err)
		}
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body.Bytes())
		}))
	}
	full := make([]*models.Robot, pageSize)
	for i := range full {
		full[i] = &models.Robot{ID: int64(i + 1), Name: "robot$other"}
	}
	unsorted := []*models.Robot{{ID: 7, Name: "robot$a"}, {ID: 3, Name: "robot$b"}}
	for name, page := range map[string][]*models.Robot{"id range ignored": full, "sort ignored": unsorted} {
		srv := serve(page)
		c := newClientFor(t, srv, "", "")
		if _, err := c.List(context.Background()); err == nil || !strings.Contains(err.Error(), "does not honour") {
			t.Errorf("%s: err = %v, want the keyset refusal", name, err)
		}
		srv.Close()
	}
}

// The client must follow no redirect. An https Harbor (or the proxy in
// front of it) that redirects to http:// on the same host would otherwise
// receive the admin credentials in clear text: net/http re-sends the
// Authorization header to the same host whatever the scheme, and on
// 307/308 it replays the body too. The plaintext server below answers like
// Harbor, so a followed redirect would also make the call succeed.
func TestClient_RefusesRedirects(t *testing.T) {
	var mu sync.Mutex
	var leaked []string // "METHOD path Authorization" seen by the plaintext server
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte("[]"))
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(models.RobotCreated{ID: 7, Name: "robot$bridge-c.ns.sa", Secret: "ROBOT-PW"})
		case http.MethodPatch:
			_ = json.NewEncoder(w).Encode(models.RobotSec{Secret: "ROBOT-PW"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer plain.Close()

	perms := []ProjectPermission{{Project: "p", Action: "pull"}}
	current := &Robot{ID: 7, WireName: "robot$bridge-c.ns.sa"}
	ops := map[string]func(Client) error{
		"List": func(c Client) error { _, err := c.List(context.Background()); return err },
		"GetByName": func(c Client) error {
			_, err := c.GetByName(context.Background(), "bridge-c.ns.sa")
			return err
		},
		"Create": func(c Client) error {
			_, err := c.Create(context.Background(), "bridge-c.ns.sa", "", perms)
			return err
		},
		"RefreshSecret": func(c Client) error { _, err := c.RefreshSecret(context.Background(), 7); return err },
		"Update":        func(c Client) error { return c.Update(context.Background(), current, "", perms) },
		"Delete":        func(c Client) error { return c.Delete(context.Background(), 7) },
	}
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, plain.URL+r.URL.RequestURI(), status)
		}))
		c := newClientFor(t, tlsSrv, "admin", "ADMIN-PW")
		for name, op := range ops {
			err := op(c)
			if err == nil || !strings.Contains(err.Error(), "refusing to follow a redirect") {
				t.Errorf("%s after a %d redirect: err = %v, want a refused-redirect error", name, status, err)
				continue
			}
			// The operator needs the setting and what to put there: the
			// target is a full request URL, not a base URL to copy.
			for _, want := range []string{"BRIDGE_HARBOR_URL", "path prefix in front of /api/v2.0"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s after a %d redirect: error %q does not mention %q", name, status, err, want)
				}
			}
		}
		tlsSrv.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) > 0 {
		t.Errorf("the plaintext redirect target received %d requests, want none:\n%s", len(leaked), strings.Join(leaked, "\n"))
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
