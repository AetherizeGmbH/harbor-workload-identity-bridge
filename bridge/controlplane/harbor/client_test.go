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

	"github.com/go-openapi/runtime"
	sdkrobot "github.com/goharbor/go-client/pkg/sdk/v2.0/client/robot"
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

	// addRobotDollarPrefix mimics real Harbor's behaviour of prepending
	// "robot$" to system-level robot names on GET paths even though POST
	// /robots accepts (and we send) the un-prefixed form. Off by default
	// to keep older tests untouched; the prefix-asymmetry regression
	// test below toggles it on.
	addRobotDollarPrefix bool
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
			r := f.mu.robots[id]
			if f.addRobotDollarPrefix && !strings.HasPrefix(r.Name, "robot$") {
				// Mimic Harbor: render names on read paths with the
				// "robot$" prefix. Construct a fresh value to avoid
				// mutating the canonical store (Create + GetByID still
				// use the un-prefixed form).
				display := *r
				display.Name = "robot$" + r.Name
				out = append(out, &display)
			} else {
				out = append(out, r)
			}
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

// TestClient_GetByName_ToleratesHarborRobotDollarPrefix locks in the
// regression discovered during the first manual e2e: Harbor's
// GET /robots renders system-level robot names as "robot$<name>" even
// though POST /robots stores them under the un-prefixed name we sent.
// Before the fix, GetByName looked for "bridge-..." in a List that
// reported "robot$bridge-...", missed it on every reconcile, and
// looped on the 409-on-create path. This test fails (NotFound) without
// the matcher tolerating the prefix.
func TestClient_GetByName_ToleratesHarborRobotDollarPrefix(t *testing.T) {
	fake := newFakeHarbor(t)
	fake.addRobotDollarPrefix = true
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	const internalName = "bridge-dev-test-pull-image-puller"
	if _, err := c.Create(context.Background(), internalName, "", nil); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := c.GetByName(context.Background(), internalName)
	if err != nil {
		t.Fatalf("GetByName: %v (expected match against the robot$-prefixed entry)", err)
	}
	if got.Name != "robot$"+internalName {
		t.Errorf("Robot.Name = %q, want the on-wire form %q", got.Name, "robot$"+internalName)
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
		{Name: "bridge-prod.flux"},
		{Name: "bridge-prod.system"},
		{Name: "bridge-staging.flux"},
		{Name: "robot-foo"},
		{Name: "bridge-prod-eu.thing"}, // prod-eu's robot — must NOT be owned by prod
	}
	got := FilterOwned(robots, "prod")
	// ADR-0018: the dot-terminated prefix "bridge-prod." excludes
	// "bridge-prod-eu.thing" (cluster prod-eu), fixing the ADR-0009
	// hyphen-prefix false positive. Only prod's own robots are owned.
	wantNames := map[string]bool{
		"bridge-prod.flux":   true,
		"bridge-prod.system": true,
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

// TestFormatHarborMessage_TypedPayload exercises the decoder against the
// exact SDK response type the user hit during the first manual e2e: a
// CreateRobotNotFound carrying a models.Errors payload. Before this
// change the user saw "&{Errors:[0x71c4c2a8cfe0]}" — a pointer string —
// because fmt's default rendering doesn't dereference the slice. The
// decoder must lift Code+Message out.
func TestFormatHarborMessage_TypedPayload(t *testing.T) {
	sdkErr := sdkrobot.NewCreateRobotNotFound()
	sdkErr.Payload = &models.Errors{
		Errors: []*models.Error{
			{Code: "NOT_FOUND", Message: `project "your-project" not found`},
		},
	}
	got := formatHarborMessage(sdkErr)
	if !strings.Contains(got, "NOT_FOUND") {
		t.Errorf("missing code: %q", got)
	}
	if !strings.Contains(got, `project "your-project" not found`) {
		t.Errorf("missing message: %q", got)
	}
	if strings.Contains(got, "0x") {
		t.Errorf("formatted message still contains a raw pointer: %q", got)
	}
}

// TestFormatHarborMessage_MultipleErrorsAreJoined covers Harbor responses
// that return a non-trivial Errors slice (rare but possible: e.g. a
// validation pass that surfaces multiple defects).
func TestFormatHarborMessage_MultipleErrorsAreJoined(t *testing.T) {
	sdkErr := sdkrobot.NewCreateRobotBadRequest()
	sdkErr.Payload = &models.Errors{
		Errors: []*models.Error{
			{Code: "BAD_REQUEST", Message: "name required"},
			{Code: "BAD_REQUEST", Message: "level required"},
		},
	}
	got := formatHarborMessage(sdkErr)
	if !strings.Contains(got, "name required") || !strings.Contains(got, "level required") {
		t.Errorf("missing one of the two messages: %q", got)
	}
	if !strings.Contains(got, ";") {
		t.Errorf("expected '; '-joined output, got %q", got)
	}
}

// TestFormatHarborMessage_FallbackOnUntyped covers the swagger-undeclared
// path: Harbor returns 409 on POST /robots but the SDK doesn't enumerate
// that status code in the response handler, so the wrapper sees a
// runtime.APIError. The decoder must not crash and must surface
// something better than a Go pointer.
func TestFormatHarborMessage_FallbackOnUntyped(t *testing.T) {
	apiErr := runtime.NewAPIError("create robot", "{}", 409)
	got := formatHarborMessage(apiErr)
	if !strings.Contains(got, "409") {
		t.Errorf("expected status 409 in fallback message, got %q", got)
	}
	if strings.Contains(got, "0x") {
		t.Errorf("fallback message contains a raw pointer: %q", got)
	}
}

// TestWrapHarborOp_409TaggedAsAlreadyExists ensures the reconciler's
// errors.Is(err, ErrRobotAlreadyExists) branch fires on the same untyped
// runtime.APIError the SDK returns for the 409 case. If this regresses,
// the reconciler will fall through to markTransientError and loop on
// the 409 forever instead of recovering.
func TestWrapHarborOp_409TaggedAsAlreadyExists(t *testing.T) {
	apiErr := runtime.NewAPIError("create robot", "{}", 409)
	wrapped := wrapHarborOp("create robot \"bridge-x\"", apiErr)
	if !errors.Is(wrapped, ErrRobotAlreadyExists) {
		t.Fatalf("errors.Is should match ErrRobotAlreadyExists: %v", wrapped)
	}
	// The wrapped error must still expose the SDK error for code-based
	// branching downstream.
	var hse harborStatusErr
	if !errors.As(wrapped, &hse) || !hse.IsCode(409) {
		t.Errorf("errors.As to harborStatusErr lost the underlying code")
	}
}
