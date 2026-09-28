// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

const testPrivilege = "nx-repository-view-docker-apps-read"

func seedPrivilege(f *fakeNexus, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.privileges[name] = Privilege{Type: "repository-view", Name: name, ReadOnly: true, Format: "docker", Repository: "apps", Actions: []string{"READ"}}
}

func TestGetRole(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	if _, err := c.GetRole(context.Background(), testRoleID); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("GetRole of a missing role = %v, want ErrNotFound with Nexus's message", err)
	}
	seedPrivilege(f, testPrivilege)
	seedRole(f, testRoleID, testPrivilege)
	got, err := c.GetRole(context.Background(), testRoleID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != testRoleID || !slices.Equal(got.Privileges, []string{testPrivilege}) {
		t.Errorf("GetRole = %+v", got)
	}
	r := f.lastRequest()
	if r.Path != "/service/rest/v1/security/roles/"+testRoleID || r.Query.Get("source") != DefaultSource {
		t.Errorf("request = %s?%s, want the role path with source=default", r.Path, r.Query.Encode())
	}
}

func TestGetRole_RefusesAnswerForAnotherRole(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, _ *http.Request) bool {
		writeJSON(w, http.StatusOK, Role{ID: "nx-admin"})
		return true
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	if _, err := c.GetRole(context.Background(), testRoleID); err == nil || !strings.Contains(err.Error(), "nx-admin") {
		t.Fatalf("GetRole = %v, want the mismatch reported", err)
	}
}

// Nexus cannot filter roles; ListRoles lists the local source and keeps
// the ids with the prefix, compared case-sensitively.
func TestListRoles_FiltersByPrefix(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	for _, id := range []string{"bridge-c.a.x", "bridge-c.b.y", "BRIDGE-C.a.x", "bridge-cx.a.x", "nx-admin"} {
		seedRole(f, id)
	}
	got, err := c.ListRoles(context.Background(), "bridge-c.")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if want := []string{"bridge-c.a.x", "bridge-c.b.y"}; !slices.Equal(ids, want) {
		t.Errorf("ListRoles = %v, want %v", ids, want)
	}
	if r := f.lastRequest(); r.Path != "/service/rest/v1/security/roles" || r.Query.Get("source") != DefaultSource {
		t.Errorf("request = %s?%s", r.Path, r.Query.Encode())
	}
}

func TestListRoles_IgnoresOtherSources(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, _ *http.Request) bool {
		writeJSON(w, http.StatusOK, []Role{{ID: "bridge-c.a.x", Source: "LDAP"}, {ID: "bridge-c.b.y", Source: DefaultSource}})
		return true
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	got, err := c.ListRoles(context.Background(), "bridge-c.")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "bridge-c.b.y" {
		t.Errorf("ListRoles = %+v, want only the local role", got)
	}
}

func TestCreateRole(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	seedPrivilege(f, testPrivilege)
	role := Role{ID: testRoleID, Name: testRoleID, Description: "managed-by=harbor-workload-identity-bridge cluster=c", Privileges: []string{testPrivilege}}
	got, err := c.CreateRole(context.Background(), role)
	if err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if got.ID != testRoleID || got.Source != DefaultSource {
		t.Errorf("CreateRole = %+v", got)
	}
	r := f.lastRequest()
	if r.Method != http.MethodPost || r.Path != "/service/rest/v1/security/roles" || r.ContentType != "application/json" {
		t.Errorf("request = %s %s (%s)", r.Method, r.Path, r.ContentType)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatal(err)
	}
	// Absent lists are sent as [] (Nexus reads a missing list as null).
	if mustJSON(t, body["roles"]) != "[]" || mustJSON(t, body["privileges"]) != `["`+testPrivilege+`"]` || body["description"] != role.Description {
		t.Errorf("body = %v", body)
	}

	// A duplicate role id is 400 on Nexus, not 409 (verified on 3.76.1).
	_, err = c.CreateRole(context.Background(), role)
	if !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("CreateRole of an existing id = %v, want ErrBadRequest with Nexus's message", err)
	}
	// So is a privilege that does not exist (a repository that is gone).
	_, err = c.CreateRole(context.Background(), Role{ID: "bridge-c.ns.other", Name: "x", Privileges: []string{"nx-repository-view-docker-gone-read"}})
	if !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "not found") {
		t.Errorf("CreateRole with a missing privilege = %v, want ErrBadRequest", err)
	}
}

func TestCreateRole_Refusals(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	if _, err := c.CreateRole(context.Background(), Role{ID: testRoleID}); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("CreateRole without a name = %v", err)
	}
	if got := f.recordedRequests(); len(got) != 0 {
		t.Errorf("Nexus saw %d requests, want none", len(got))
	}
	f.setOverride(func(w http.ResponseWriter, _ *http.Request) bool {
		writeJSON(w, http.StatusOK, Role{ID: "nx-admin"})
		return true
	})
	if _, err := c.CreateRole(context.Background(), Role{ID: testRoleID, Name: testRoleID}); err == nil || !strings.Contains(err.Error(), "nx-admin") {
		t.Errorf("CreateRole answered for another role = %v, want the mismatch reported", err)
	}
}

func TestUpdateRole(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	seedPrivilege(f, testPrivilege)
	role := Role{ID: testRoleID, Name: testRoleID, Privileges: []string{testPrivilege}}
	if err := c.UpdateRole(context.Background(), role); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateRole of a missing role = %v, want ErrNotFound", err)
	}
	seedRole(f, testRoleID)
	if err := c.UpdateRole(context.Background(), role); err != nil {
		t.Fatalf("UpdateRole: %v", err)
	}
	r := f.lastRequest()
	var body roleBody
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatal(err)
	}
	if r.Method != http.MethodPut || r.Path != "/service/rest/v1/security/roles/"+testRoleID || body.ID != testRoleID {
		t.Errorf("request = %s %s with body id %q, want PUT with the path's id in the body", r.Method, r.Path, body.ID)
	}
	got, err := c.GetRole(context.Background(), testRoleID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Privileges, []string{testPrivilege}) {
		t.Errorf("after update: %+v", got)
	}
	// A privilege whose repository is gone is refused with 400.
	role.Privileges = append(role.Privileges, "nx-repository-view-docker-gone-read")
	if err := c.UpdateRole(context.Background(), role); !errors.Is(err, ErrBadRequest) {
		t.Errorf("UpdateRole with a missing privilege = %v, want ErrBadRequest", err)
	}
}

func TestDeleteRole_IsIdempotentAndStripsUsers(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	seedRole(f, testRoleID)
	seedUser(f, testUser())
	if err := c.DeleteRole(context.Background(), testRoleID); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if r := f.lastRequest(); r.Method != http.MethodDelete || r.Path != "/service/rest/v1/security/roles/"+testRoleID {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	if err := c.DeleteRole(context.Background(), testRoleID); err != nil {
		t.Errorf("DeleteRole of a missing role = %v, want nil", err)
	}
	u, err := c.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Roles) != 0 {
		t.Errorf("user roles after role delete = %v, want none", u.Roles)
	}
	f.setOverride(func(w http.ResponseWriter, _ *http.Request) bool {
		nexusError(w, http.StatusBadRequest, "Role 'nx-admin' is internal and cannot be modified or deleted.")
		return true
	})
	if err := c.DeleteRole(context.Background(), "nx-admin"); !errors.Is(err, ErrBadRequest) {
		t.Errorf("DeleteRole of a read-only role = %v, want ErrBadRequest", err)
	}
}

// GET /v1/security/privileges/{name} doubles as the repository-exists
// check: Nexus answers 404 for the privilege of a repository that does not
// exist (verified on Nexus 3.76.1).
func TestGetPrivilege(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	if _, err := c.GetPrivilege(context.Background(), testPrivilege); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPrivilege of a missing repository = %v, want ErrNotFound", err)
	}
	seedPrivilege(f, testPrivilege)
	p, err := c.GetPrivilege(context.Background(), testPrivilege)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != testPrivilege || p.Type != "repository-view" || p.Repository != "apps" || p.Format != "docker" || !slices.Equal(p.Actions, []string{"READ"}) {
		t.Errorf("GetPrivilege = %+v", p)
	}
	if r := f.lastRequest(); r.Path != "/service/rest/v1/security/privileges/"+testPrivilege {
		t.Errorf("request path = %s", r.Path)
	}
	f.setOverride(func(w http.ResponseWriter, _ *http.Request) bool {
		writeJSON(w, http.StatusOK, Privilege{Name: "nx-all"})
		return true
	})
	if _, err := c.GetPrivilege(context.Background(), testPrivilege); err == nil || !strings.Contains(err.Error(), "nx-all") {
		t.Errorf("GetPrivilege answered for another privilege = %v, want the mismatch reported", err)
	}
}
