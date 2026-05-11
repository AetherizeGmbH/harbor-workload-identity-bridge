// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/goharbor/go-client/pkg/sdk/v2.0/models"
)

// fakeHarbor is a minimal in-memory Harbor /robots implementation used by
// these tests. It validates the request shape (Basic Auth, paths, methods,
// JSON body keys), keeps state across calls so multi-step flows work, and
// records the requests it received for assertions.
type fakeHarbor struct {
	t *testing.T

	mu       *fakeHarborState
	requests []recordedRequest

	// expectedUser/Pass let tests assert the wrapper sent the right
	// credentials. If empty, the check is skipped.
	expectedUser string
	expectedPass string
}

type fakeHarborState struct {
	robots map[int64]*models.Robot
	nextID int64
}

type recordedRequest struct {
	Method string
	Path   string
}

func newFakeHarbor(t *testing.T) *fakeHarbor {
	return &fakeHarbor{
		t:  t,
		mu: &fakeHarborState{robots: map[int64]*models.Robot{}, nextID: 100},
	}
}

func (f *fakeHarbor) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2.0/robots", f.handleCollection)
	mux.HandleFunc("/api/v2.0/robots/", f.handleItem)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: r.URL.Path})
		if !f.checkAuth(w, r) {
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

func (f *fakeHarbor) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	if f.expectedUser == "" && f.expectedPass == "" {
		return true
	}
	got := r.Header.Get("Authorization")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(f.expectedUser+":"+f.expectedPass))
	if got != want {
		http.Error(w, "bad auth", http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *fakeHarbor) handleCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var body models.RobotCreate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Name == "" {
			writeHarborError(w, http.StatusBadRequest, "BAD_REQUEST", "name required")
			return
		}
		id := f.mu.nextID
		f.mu.nextID++
		stored := &models.Robot{
			ID:          id,
			Name:        body.Name,
			Description: body.Description,
			Level:       body.Level,
			Secret:      "generated-secret-for-" + body.Name,
			Permissions: body.Permissions,
		}
		f.mu.robots[id] = stored
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(models.RobotCreated{
			ID:     id,
			Name:   body.Name,
			Secret: stored.Secret,
		})
	case http.MethodGet:
		// Honor page / page_size so the wrapper's pagination walk
		// actually terminates. Harbor pages are 1-indexed.
		page := parseInt64Default(r.URL.Query().Get("page"), 1)
		size := parseInt64Default(r.URL.Query().Get("page_size"), 10)
		// Stable order so pagination is deterministic.
		ids := make([]int64, 0, len(f.mu.robots))
		for id := range f.mu.robots {
			ids = append(ids, id)
		}
		sortInt64s(ids)
		start := (page - 1) * size
		end := start + size
		out := make([]*models.Robot, 0, size)
		for i, id := range ids {
			if int64(i) < start {
				continue
			}
			if int64(i) >= end {
				break
			}
			out = append(out, f.mu.robots[id])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (f *fakeHarbor) handleItem(w http.ResponseWriter, r *http.Request) {
	// Path: /api/v2.0/robots/{id}
	// Methods used by the SDK: GET, PUT (UpdateRobot), PATCH (RefreshSec), DELETE.
	idStr := strings.TrimPrefix(r.URL.Path, "/api/v2.0/robots/")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeHarborError(w, http.StatusBadRequest, "BAD_REQUEST", "bad id")
		return
	}
	robot, ok := f.mu.robots[id]
	if !ok {
		writeHarborError(w, http.StatusNotFound, "NOT_FOUND", fmt.Sprintf("robot %d not found", id))
		return
	}

	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(robot)
	case http.MethodPut:
		var body models.Robot
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeHarborError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
			return
		}
		robot.Description = body.Description
		robot.Permissions = body.Permissions
		w.WriteHeader(http.StatusOK)
	case http.MethodPatch:
		// Harbor uses PATCH /robots/{id} as the password-refresh endpoint
		// (operation RefreshSec). The body is a RobotSec, ignored here.
		robot.Secret = "refreshed-secret-for-" + robot.Name
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.RobotSec{Secret: robot.Secret})
	case http.MethodDelete:
		delete(f.mu.robots, id)
		w.WriteHeader(http.StatusOK)
	default:
		writeHarborError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", r.Method)
	}
}

// writeHarborError emits the JSON error shape Harbor's API uses (models.Errors).
// Plain text bodies would be rejected by go-client's typed response readers.
func writeHarborError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(models.Errors{
		Errors: []*models.Error{{Code: code, Message: msg}},
	})
}

func newClientFor(t *testing.T, srv *httptest.Server, username, password string) Client {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(u, username, password, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClient_Create_PopulatesSecret(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	got, err := c.Create(context.Background(),
		"bridge-prod-flux-system-source-controller",
		"managed-by=bridge cluster=prod",
		[]ProjectPermission{
			{Project: "production", Action: "pull"},
			{Project: "shared", Action: "pull,push"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID < 100 {
		t.Errorf("ID not assigned: %d", got.ID)
	}
	if got.Secret == "" {
		t.Errorf("Secret should be populated on Create response")
	}
	if got.Name != "bridge-prod-flux-system-source-controller" {
		t.Errorf("Name = %q", got.Name)
	}
}

func TestClient_Create_TranslatesCommaActionToTwoAccess(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	got, err := c.Create(context.Background(), "bridge-x-y-z", "",
		[]ProjectPermission{{Project: "shared", Action: "pull,push"}})
	if err != nil {
		t.Fatal(err)
	}
	stored := fake.mu.robots[got.ID]
	if len(stored.Permissions) != 1 {
		t.Fatalf("expected 1 RobotPermission, got %d", len(stored.Permissions))
	}
	if got, want := len(stored.Permissions[0].Access), 2; got != want {
		t.Fatalf("comma action should expand to 2 Access entries; got %d", got)
	}
	actions := []string{stored.Permissions[0].Access[0].Action, stored.Permissions[0].Access[1].Action}
	hasPull, hasPush := false, false
	for _, a := range actions {
		if a == "pull" {
			hasPull = true
		}
		if a == "push" {
			hasPush = true
		}
	}
	if !hasPull || !hasPush {
		t.Errorf("expected pull+push access, got %v", actions)
	}
}

func TestClient_Create_SendsBasicAuth(t *testing.T) {
	fake := newFakeHarbor(t)
	fake.expectedUser = "admin"
	fake.expectedPass = "s3cret"
	srv := fake.server()
	defer srv.Close()

	c := newClientFor(t, srv, "admin", "s3cret")
	if _, err := c.Create(context.Background(), "bridge-x-y-z", "",
		[]ProjectPermission{{Project: "p", Action: "pull"}}); err != nil {
		t.Fatal(err)
	}

	// Same client with wrong credentials → 401 wrapped into our error.
	bad := newClientFor(t, srv, "admin", "wrong")
	_, err := bad.Create(context.Background(), "bridge-a-b-c", "",
		[]ProjectPermission{{Project: "p", Action: "pull"}})
	if err == nil {
		t.Fatal("expected error from wrong credentials")
	}
}

func TestClient_GetByID_NotFoundIsTyped(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	_, err := c.GetByID(context.Background(), 99999)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrRobotNotFound) {
		t.Errorf("expected ErrRobotNotFound, got %v", err)
	}
}

func TestClient_GetByName_FilteringWorks(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	names := []string{"bridge-prod-a-b", "bridge-prod-c-d", "bridge-staging-x-y"}
	for _, n := range names {
		if _, err := c.Create(context.Background(), n, "",
			[]ProjectPermission{{Project: "p", Action: "pull"}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.GetByName(context.Background(), "bridge-prod-c-d")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "bridge-prod-c-d" {
		t.Errorf("Name = %q", got.Name)
	}

	if _, err := c.GetByName(context.Background(), "does-not-exist"); !errors.Is(err, ErrRobotNotFound) {
		t.Errorf("expected ErrRobotNotFound, got %v", err)
	}
}

func TestClient_Delete_IsIdempotent(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	created, err := c.Create(context.Background(), "bridge-x-y-z", "",
		[]ProjectPermission{{Project: "p", Action: "pull"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	// Second delete must not error — bridge-level idempotency.
	if err := c.Delete(context.Background(), created.ID); err != nil {
		t.Errorf("second Delete returned error: %v", err)
	}
	if _, err := c.GetByID(context.Background(), created.ID); !errors.Is(err, ErrRobotNotFound) {
		t.Errorf("after Delete, GetByID should be ErrRobotNotFound; got %v", err)
	}
}

func TestClient_RefreshSecret_ReturnsNewValue(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	created, err := c.Create(context.Background(), "bridge-x-y-z", "",
		[]ProjectPermission{{Project: "p", Action: "pull"}})
	if err != nil {
		t.Fatal(err)
	}
	newSecret, err := c.RefreshSecret(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newSecret == "" || newSecret == created.Secret {
		t.Errorf("RefreshSecret returned %q (vs old %q)", newSecret, created.Secret)
	}
}

func TestClient_UpdatePermissions(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	created, err := c.Create(context.Background(), "bridge-x-y-z", "old-desc",
		[]ProjectPermission{{Project: "production", Action: "pull"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UpdatePermissions(context.Background(), created.ID, "new-desc",
		[]ProjectPermission{{Project: "production", Action: "pull,push"}}); err != nil {
		t.Fatal(err)
	}
	stored := fake.mu.robots[created.ID]
	if stored.Description != "new-desc" {
		t.Errorf("Description not updated: %q", stored.Description)
	}
	if len(stored.Permissions[0].Access) != 2 {
		t.Errorf("permissions not updated; got %d Access entries", len(stored.Permissions[0].Access))
	}
}

func TestClient_List_PaginatesAcrossPages(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	// Create more robots than one page would hold to verify the wrapper
	// keeps walking. Page size in production is 100; the fake ignores
	// page params and returns all robots in one shot, so we can't truly
	// test pagination round-trips here, but we can at least assert the
	// wrapper returns all of them.
	for i := 0; i < 150; i++ {
		if _, err := c.Create(context.Background(),
			fmt.Sprintf("bridge-x-y-z%d", i), "",
			[]ProjectPermission{{Project: "p", Action: "pull"}}); err != nil {
			t.Fatal(err)
		}
	}
	robots, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(robots) != 150 {
		t.Errorf("List returned %d robots, want 150", len(robots))
	}
}

func TestFilterOwned_PrefixFilter(t *testing.T) {
	robots := []Robot{
		{Name: "bridge-prod-flux"},
		{Name: "bridge-prod-system"},
		{Name: "bridge-staging-flux"},
		{Name: "robot-foo"},
		{Name: "bridge-prod-eu-thing"}, // documented prefix-collision case
	}
	got := FilterOwned(robots, "prod")
	// Per ADR-0009 known limitation: cluster "prod" also picks up
	// "bridge-prod-eu-thing". The test pins the behaviour so a change has
	// to update the ADR and this expectation together.
	wantNames := map[string]bool{
		"bridge-prod-flux":     true,
		"bridge-prod-system":   true,
		"bridge-prod-eu-thing": true,
	}
	if len(got) != len(wantNames) {
		t.Errorf("FilterOwned returned %d, want %d", len(got), len(wantNames))
	}
	for _, r := range got {
		if !wantNames[r.Name] {
			t.Errorf("FilterOwned included unexpected robot %q", r.Name)
		}
	}
}

func parseInt64Default(raw string, def int64) int64 {
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}

func sortInt64s(s []int64) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func TestFilterOwned_EmptyClusterReturnsNothing(t *testing.T) {
	robots := []Robot{{Name: "bridge-prod-flux"}, {Name: "bridge-anything"}}
	got := FilterOwned(robots, "")
	if len(got) != 0 {
		t.Errorf("empty cluster name should match nothing; got %d", len(got))
	}
}
