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

	// prefix mimics Harbor's robot_name_prefix: names are STORED without
	// it and RENDERED with it on every read path and in the Create
	// response (goharbor/harbor src/controller/robot/controller.go
	// populate). Defaults to Harbor's "robot$".
	prefix string

	// ignoreQuery makes GET /robots ignore the q filter, modelling a
	// Harbor build whose query filtering behaves differently, so the
	// client's full-scan fallback is exercised.
	ignoreQuery bool

	// queries records every q value GET /robots received.
	queries []string
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
		t:      t,
		mu:     &fakeHarborState{robots: map[int64]*models.Robot{}, nextID: 100},
		prefix: "robot$",
	}
}

// render returns the read-path view of a stored robot (prefixed name, no
// secret — Harbor never returns secrets on read paths).
func (f *fakeHarbor) render(r *models.Robot) *models.Robot {
	out := *r
	out.Name = f.prefix + r.Name
	out.Secret = ""
	return &out
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
		for _, existing := range f.mu.robots {
			if existing.Name == body.Name {
				writeHarborError(w, http.StatusConflict, "CONFLICT", "robot already exists")
				return
			}
		}
		stored := &models.Robot{
			ID:          id,
			Name:        body.Name,
			Description: body.Description,
			Level:       body.Level,
			ExpiresAt:   -1,
			Editable:    true,
			Secret:      "generated-secret-for-" + body.Name,
			Permissions: body.Permissions,
		}
		f.mu.robots[id] = stored
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(models.RobotCreated{
			ID:     id,
			Name:   f.prefix + body.Name,
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
		// q=name=<exact> filters on the STORED (un-prefixed) name, like
		// Harbor's ORM filter on the robot.name column.
		if q := r.URL.Query().Get("q"); q != "" {
			f.queries = append(f.queries, q)
			if want, ok := strings.CutPrefix(q, "name="); ok && !f.ignoreQuery {
				kept := ids[:0]
				for _, id := range ids {
					if f.mu.robots[id].Name == want {
						kept = append(kept, id)
					}
				}
				ids = kept
			}
		}
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
			out = append(out, f.render(f.mu.robots[id]))
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
		_ = json.NewEncoder(w).Encode(f.render(robot))
	case http.MethodPut:
		var body models.Robot
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeHarborError(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
			return
		}
		// Harbor's updateV2Robot: the name (on-wire form) and level are
		// immutable and must be echoed verbatim, and the disable flag is
		// overwritten with whatever the body carries.
		if body.Level != robot.Level || body.Name != f.prefix+robot.Name {
			writeHarborError(w, http.StatusBadRequest, "BAD_REQUEST", "cannot update the level or name of robot")
			return
		}
		if len(body.Permissions) == 0 {
			writeHarborError(w, http.StatusBadRequest, "BAD_REQUEST", "Permission list cannot be empty")
			return
		}
		robot.Description = body.Description
		robot.Permissions = body.Permissions
		robot.Disable = body.Disable
		if body.Duration != nil && *body.Duration == -1 {
			robot.ExpiresAt = -1
		}
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

// TestClient_ReadPathsStripRobotPrefix locks in ADR-0014: Harbor stores
// robot names without its robot prefix but renders them WITH it on every
// read path and in the Create response. The client must hand callers the
// internal name (so comparisons against RobotName work) while keeping the
// on-wire form for Basic Auth and updates.
func TestClient_ReadPathsStripRobotPrefix(t *testing.T) {
	for _, prefix := range []string{"robot$", "robot_"} {
		t.Run(prefix, func(t *testing.T) {
			fake := newFakeHarbor(t)
			fake.prefix = prefix
			srv := fake.server()
			defer srv.Close()
			u, _ := url.Parse(srv.URL)
			c, err := NewClient(u, "", "", srv.Client().Transport, WithRobotPrefix(prefix))
			if err != nil {
				t.Fatal(err)
			}

			const internalName = "bridge-dev.test-pull.image-puller"
			created, err := c.Create(context.Background(), internalName, "",
				[]ProjectPermission{{Project: "p", Action: "pull"}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if created.Name != internalName || created.WireName != prefix+internalName {
				t.Errorf("Create: Name=%q WireName=%q", created.Name, created.WireName)
			}

			got, err := c.GetByName(context.Background(), internalName)
			if err != nil {
				t.Fatalf("GetByName: %v", err)
			}
			if got.Name != internalName || got.WireName != prefix+internalName {
				t.Errorf("GetByName: Name=%q WireName=%q", got.Name, got.WireName)
			}
		})
	}
}

// TestClient_GetByName_UsesExactNameQuery pins the O(1) lookup: the client
// asks Harbor for q=name=<internal name> instead of paging every robot.
func TestClient_GetByName_UsesExactNameQuery(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	if _, err := c.Create(context.Background(), "bridge-a.b.c", "", []ProjectPermission{{Project: "p", Action: "pull"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetByName(context.Background(), "bridge-a.b.c"); err != nil {
		t.Fatal(err)
	}
	if len(fake.queries) != 1 || fake.queries[0] != "name=bridge-a.b.c" {
		t.Errorf("queries = %q, want exactly [name=bridge-a.b.c]", fake.queries)
	}
}

// TestClient_GetByName_FallsBackToFullScan covers a Harbor whose q filter
// does not behave as expected: the client must still find the robot (and
// must not report NotFound, which would drive a create/409 loop).
func TestClient_GetByName_FallsBackToFullScan(t *testing.T) {
	fake := newFakeHarbor(t)
	fake.ignoreQuery = true
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	for _, n := range []string{"bridge-a.b.c", "bridge-a.b.d"} {
		if _, err := c.Create(context.Background(), n, "", []ProjectPermission{{Project: "p", Action: "pull"}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.GetByName(context.Background(), "bridge-a.b.d")
	if err != nil || got.Name != "bridge-a.b.d" {
		t.Fatalf("GetByName = %+v, %v", got, err)
	}
}

func TestClient_Create_409IsAlreadyExists(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")
	perms := []ProjectPermission{{Project: "p", Action: "pull"}}
	if _, err := c.Create(context.Background(), "bridge-a.b.c", "", perms); err != nil {
		t.Fatal(err)
	}
	_, err := c.Create(context.Background(), "bridge-a.b.c", "", perms)
	if !errors.Is(err, ErrRobotAlreadyExists) {
		t.Fatalf("second Create: got %v, want ErrRobotAlreadyExists", err)
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
	if _, err := c.GetByName(context.Background(), "bridge-x-y-z"); !errors.Is(err, ErrRobotNotFound) {
		t.Errorf("after Delete, GetByName should be ErrRobotNotFound; got %v", err)
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

// TestClient_Update_EchoesWireNameAndPreservesDisable is the regression
// test for audit C1: Harbor rejects every PUT /robots/{id} whose name is
// not the stored on-wire name, so an update that omits (or un-prefixes)
// the name fails with 400 and permission changes never reach Harbor. It
// also pins that an administrator's "disable" survives the update.
func TestClient_Update_EchoesWireNameAndPreservesDisable(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	created, err := c.Create(context.Background(), "bridge-x.y.z", "old-desc",
		[]ProjectPermission{{Project: "production", Action: "pull"}})
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.robots[created.ID].Disable = true // an admin disabled it in the UI

	current, err := c.GetByName(context.Background(), "bridge-x.y.z")
	if err != nil {
		t.Fatal(err)
	}
	if !current.Disabled {
		t.Fatal("read path lost the disable flag")
	}
	if err := c.Update(context.Background(), current, "new-desc",
		[]ProjectPermission{{Project: "production", Action: "pull,push"}}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	stored := fake.mu.robots[created.ID]
	if stored.Description != "new-desc" {
		t.Errorf("Description not updated: %q", stored.Description)
	}
	if len(stored.Permissions) != 1 || len(stored.Permissions[0].Access) != 2 {
		t.Errorf("permissions not updated: %+v", stored.Permissions)
	}
	if !stored.Disable {
		t.Error("Update re-enabled a robot an administrator disabled")
	}

	after, err := c.GetByName(context.Background(), "bridge-x.y.z")
	if err != nil {
		t.Fatal(err)
	}
	if !PermissionsMatch(after, []ProjectPermission{{Project: "production", Action: "pull,push"}}) {
		t.Errorf("read-back permissions %+v do not match what was written", after.Permissions)
	}
}

// TestClient_Update_WithoutWireNameIsRejectedByHarbor proves the fake
// enforces Harbor's name immutability, i.e. that the test above would
// fail if the client sent the internal name.
func TestClient_Update_WithoutWireNameIsRejectedByHarbor(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")

	created, err := c.Create(context.Background(), "bridge-x.y.z", "",
		[]ProjectPermission{{Project: "production", Action: "pull"}})
	if err != nil {
		t.Fatal(err)
	}
	bad := *created
	bad.WireName = created.Name // un-prefixed: what the pre-fix client effectively sent
	err = c.Update(context.Background(), &bad, "", []ProjectPermission{{Project: "production", Action: "push"}})
	if err == nil || !strings.Contains(err.Error(), "cannot update the level or name") {
		t.Fatalf("Update with un-prefixed name: got %v, want Harbor 400", err)
	}
	if err := c.Update(context.Background(), &Robot{ID: created.ID}, "", nil); err == nil {
		t.Fatal("Update without a WireName must fail before calling Harbor")
	}
}

func TestPermissionsMatch(t *testing.T) {
	cases := []struct {
		name    string
		robot   Robot
		desired []ProjectPermission
		want    bool
	}{
		{"equal", Robot{Permissions: []ProjectPermission{{Project: "a", Action: "pull"}}}, []ProjectPermission{{Project: "a", Action: "pull"}}, true},
		{"order and split insensitive",
			Robot{Permissions: []ProjectPermission{{Project: "b", Action: "push,pull"}, {Project: "a", Action: "pull"}}},
			[]ProjectPermission{{Project: "a", Action: "pull"}, {Project: "b", Action: "pull"}, {Project: "b", Action: "push"}}, true},
		{"missing project", Robot{Permissions: []ProjectPermission{{Project: "a", Action: "pull"}}}, []ProjectPermission{{Project: "a", Action: "pull"}, {Project: "b", Action: "pull"}}, false},
		{"revoked action", Robot{Permissions: []ProjectPermission{{Project: "a", Action: "pull,push"}}}, []ProjectPermission{{Project: "a", Action: "pull"}}, false},
		{"foreign access", Robot{ForeignAccess: true, Permissions: []ProjectPermission{{Project: "a", Action: "pull"}}}, []ProjectPermission{{Project: "a", Action: "pull"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PermissionsMatch(&tc.robot, tc.desired); got != tc.want {
				t.Errorf("PermissionsMatch = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFromHarborRobot_FlagsForeignAccess(t *testing.T) {
	c := &goClient{robotPrefix: "robot$"}
	r := c.fromHarborRobot(&models.Robot{
		Name: "robot$bridge-a.b.c",
		Permissions: []*models.RobotPermission{
			{Kind: "project", Namespace: "p", Access: []*models.Access{
				{Resource: "repository", Action: "pull"},
				{Resource: "artifact", Action: "delete"},
			}},
		},
	})
	if !r.ForeignAccess {
		t.Error("an artifact:delete grant must be flagged as foreign access")
	}
	if r.Name != "bridge-a.b.c" {
		t.Errorf("Name = %q", r.Name)
	}
	r = c.fromHarborRobot(&models.Robot{
		Name:        "robot$bridge-a.b.c",
		Permissions: []*models.RobotPermission{{Kind: "system", Namespace: "/", Access: []*models.Access{{Resource: "robot", Action: "create"}}}},
	})
	if !r.ForeignAccess {
		t.Error("a system-kind grant must be flagged as foreign access")
	}
}

func TestToHarborPermissions_MergesDuplicateProjects(t *testing.T) {
	got := toHarborPermissions([]ProjectPermission{
		{Project: "b", Action: "push"}, {Project: "a", Action: "pull"}, {Project: "b", Action: "pull"},
	})
	if len(got) != 2 || got[0].Namespace != "a" || got[1].Namespace != "b" {
		t.Fatalf("got %+v, want one entry per project, sorted", got)
	}
	if len(got[1].Access) != 2 || got[1].Access[0].Action != "pull" || got[1].Access[1].Action != "push" {
		t.Errorf("project b accesses = %+v, want pull then push", got[1].Access)
	}
}

// TestNewClient_BasePathHandling pins that every documented spelling of
// BRIDGE_HARBOR_URL reaches /api/v2.0/robots exactly once — a trailing
// slash used to defeat the suffix check and double the base path.
func TestNewClient_BasePathHandling(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	for _, suffix := range []string{"", "/", "/api/v2.0", "/api/v2.0/"} {
		u, _ := url.Parse(srv.URL + suffix)
		c, err := NewClient(u, "", "", srv.Client().Transport)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.List(context.Background()); err != nil {
			t.Fatalf("%q: List: %v", suffix, err)
		}
		if gotPath != "/api/v2.0/robots" {
			t.Errorf("URL suffix %q: request path %q, want /api/v2.0/robots", suffix, gotPath)
		}
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
