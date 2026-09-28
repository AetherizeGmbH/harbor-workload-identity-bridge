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

const (
	testUserID = "bridge-c.ns.sa_0123456789abcdef"
	testRoleID = "bridge-c.ns.sa"
)

func testUser() User {
	return User{
		UserID:       testUserID,
		FirstName:    "managed-by=harbor-workload-identity-bridge cluster=c",
		LastName:     "nexusaccess=ns/name",
		EmailAddress: "nexus-bridge@harbor-workload-identity-bridge.invalid",
		Status:       UserActive,
		Roles:        []string{testRoleID},
	}
}

func seedRole(f *fakeNexus, id string, privileges ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if privileges == nil {
		privileges = []string{}
	}
	f.roles[id] = Role{ID: id, Name: id, Privileges: privileges, Roles: []string{}, Source: DefaultSource}
}

func seedUser(f *fakeNexus, u User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u.Source == "" {
		u.Source = DefaultSource
	}
	f.users[u.UserID] = u
}

// Nexus authenticates active and changepassword users (UserStatus.isActive;
// changepassword verified on Nexus 3.76.1), not locked or disabled ones.
func TestUserStatus_Active(t *testing.T) {
	for s, want := range map[UserStatus]bool{
		UserActive: true, UserChangePassword: true, UserLocked: false, UserDisabled: false, "": false, "enabled": false,
	} {
		if got := s.Active(); got != want {
			t.Errorf("UserStatus(%q).Active() = %v, want %v", s, got, want)
		}
	}
}

// Nexus's user search matches a case-insensitive id prefix, across the
// sources unless one is named (verified on Nexus 3.76.1). GetUser must
// return exactly the local user with exactly the requested id.
func TestGetUser_ExactCaseSensitiveLocalMatch(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	seedUser(f, User{UserID: "BRIDGE-C.NS.SA_0123456789ABCDEF", Status: UserActive})
	seedUser(f, User{UserID: testUserID + "0", Status: UserActive})

	if _, err := c.GetUser(context.Background(), testUserID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser without an exact match = %v, want ErrNotFound", err)
	}
	r := f.lastRequest()
	if r.Method != http.MethodGet || r.Path != "/service/rest/v1/security/users" ||
		r.Query.Get("userId") != testUserID || r.Query.Get("source") != DefaultSource {
		t.Errorf("request = %s %s?%s, want the user search with userId and source=default", r.Method, r.Path, r.Query.Encode())
	}

	want := testUser()
	want.Source = DefaultSource
	seedRole(f, testRoleID)
	seedUser(f, want)
	got, err := c.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.UserID != testUserID || got.Source != DefaultSource || got.FirstName != want.FirstName ||
		got.Status != UserActive || !slices.Equal(got.Roles, want.Roles) {
		t.Errorf("GetUser = %+v, want %+v", got, want)
	}
}

// A listing that ignored source=default (a proxy, a future Nexus) must not
// make GetUser return another source's user of the same id.
func TestGetUser_IgnoresOtherSources(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, _ *http.Request) bool {
		writeJSON(w, http.StatusOK, []User{{UserID: testUserID, Source: "LDAP", Status: UserActive}})
		return true
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	if _, err := c.GetUser(context.Background(), testUserID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser with only an LDAP match = %v, want ErrNotFound", err)
	}
}

func TestGetUser_RefusesDuplicateListing(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, _ *http.Request) bool {
		u := User{UserID: testUserID, Source: DefaultSource, Status: UserActive}
		writeJSON(w, http.StatusOK, []User{u, u})
		return true
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	if _, err := c.GetUser(context.Background(), testUserID); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("GetUser with a duplicate listing = %v, want an error", err)
	}
}

// ListUsers keeps only local users whose id begins with the prefix,
// compared case-sensitively, whatever else Nexus's case-insensitive search
// returns.
func TestListUsers_FiltersCaseSensitively(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	for _, id := range []string{"bridge-c.a.x_0000000000000001", "bridge-c.b.y_0000000000000002", "BRIDGE-C.a.x_0000000000000003", "Bridge-c.b.y_0000000000000004", "bridge-cx.a.x_0000000000000005", "admin"} {
		seedUser(f, User{UserID: id, Status: UserActive})
	}
	got, err := c.ListUsers(context.Background(), "bridge-c.")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, u := range got {
		ids = append(ids, u.UserID)
	}
	slices.Sort(ids)
	if want := []string{"bridge-c.a.x_0000000000000001", "bridge-c.b.y_0000000000000002"}; !slices.Equal(ids, want) {
		t.Errorf("ListUsers = %v, want %v", ids, want)
	}
	if r := f.lastRequest(); r.Query.Get("userId") != "bridge-c." || r.Query.Get("source") != DefaultSource {
		t.Errorf("query = %s, want userId=bridge-c. and source=default", r.Query.Encode())
	}
}

func TestCreateUser_SendsApiCreateUser(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	seedRole(f, testRoleID)

	got, err := c.CreateUser(context.Background(), testUser(), "USER-PASSWORD-456")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if got.UserID != testUserID || got.Source != DefaultSource || !slices.Equal(got.Roles, []string{testRoleID}) {
		t.Errorf("CreateUser = %+v", got)
	}
	reqs := f.recordedRequests()
	if len(reqs) != 2 {
		t.Fatalf("Nexus saw %d requests, want the create and the read-back", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/service/rest/v1/security/users" || r.ContentType != "application/json" {
		t.Errorf("request = %s %s (%s)", r.Method, r.Path, r.ContentType)
	}
	if back := reqs[1]; back.Method != http.MethodGet || back.Query.Get("userId") != testUserID || back.Query.Get("source") != DefaultSource {
		t.Errorf("read-back = %s %s?%s, want the user search for the new id", back.Method, back.Path, back.Query.Encode())
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"userId": testUserID, "firstName": testUser().FirstName, "lastName": testUser().LastName,
		"emailAddress": testUser().EmailAddress, "password": "USER-PASSWORD-456", "status": "active",
		"roles": []any{testRoleID},
	}
	if len(body) != len(want) {
		t.Errorf("body has fields %v, want exactly %v", body, want)
	}
	for k, v := range want {
		if b, _ := json.Marshal(body[k]); string(b) != mustJSON(t, v) {
			t.Errorf("body[%q] = %s, want %s", k, b, mustJSON(t, v))
		}
	}
	f.mu.Lock()
	stored := f.passwords[testUserID]
	f.mu.Unlock()
	if stored != "USER-PASSWORD-456" {
		t.Errorf("Nexus stored password %q", stored)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Nexus answers a user create with the request and does not refuse a role
// that does not exist: it stores the id, hides it from reads, and the user
// gains the role once a role with that id is created (verified on Nexus
// 3.76.1). CreateUser returns the user as read back, so the caller sees the
// role missing; the answer to the create would have shown it.
func TestCreateUser_ReturnsTheUserAsReadBack(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	got, err := c.CreateUser(context.Background(), testUser(), "pw")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if len(got.Roles) != 0 {
		t.Errorf("CreateUser roles = %v, want the read-back without the role that does not exist", got.Roles)
	}
	if _, err := c.CreateRole(context.Background(), Role{ID: testRoleID, Name: testRoleID}); err != nil {
		t.Fatal(err)
	}
	later, err := c.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(later.Roles, []string{testRoleID}) {
		t.Errorf("roles after the role was created = %v, want the stored reference in effect", later.Roles)
	}
}

// A read-back that fails is an error although the user exists; one that
// does not find the user must not read as "the user does not exist".
func TestCreateUser_ReadBackFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		want   string
		is     error
	}{
		{"server error", func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) }, "reading it back failed", ErrServer},
		{"not listed", func(w http.ResponseWriter) { writeJSON(w, http.StatusOK, []User{}) }, "does not list it", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeNexus(t)
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodGet {
					return false
				}
				tc.answer(w)
				return true
			}
			srv := f.server("")
			c := newTestClient(t, f, srv, "")
			seedRole(f, testRoleID)
			_, err := c.CreateUser(context.Background(), testUser(), "pw")
			if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, ErrNotFound) {
				t.Fatalf("CreateUser = %v, want %q and not ErrNotFound", err, tc.want)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("CreateUser = %v, want it to match %v", err, tc.is)
			}
		})
	}
}

// Nexus answers a duplicate user id with 500 (DuplicateUserException), not
// 409 (verified on Nexus 3.76.1).
func TestCreateUser_DuplicateIsServerError(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	seedRole(f, testRoleID)
	seedUser(f, testUser())
	_, err := c.CreateUser(context.Background(), testUser(), "USER-PASSWORD-456")
	if !errors.Is(err, ErrServer) || errors.Is(err, ErrConflict) {
		t.Fatalf("CreateUser of an existing id = %v, want ErrServer", err)
	}
	if !strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "USER-PASSWORD-456") {
		t.Errorf("error = %q, want Nexus's explanation without the password", err)
	}
}

func TestCreateUser_ValidatesBeforeSending(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	cases := map[string]struct {
		user     func(*User)
		password string
		want     string
	}{
		"no password":  {func(*User) {}, "", "password is required"},
		"no status":    {func(u *User) { u.Status = "" }, "pw", "not a Nexus user status"},
		"bad status":   {func(u *User) { u.Status = "enabled" }, "pw", "not a Nexus user status"},
		"no roles":     {func(u *User) { u.Roles = nil }, "pw", "at least one role"},
		"other source": {func(u *User) { u.Source = "LDAP" }, "pw", "only local users"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			u := testUser()
			tc.user(&u)
			if _, err := c.CreateUser(context.Background(), u, tc.password); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateUser = %v, want %q", err, tc.want)
			}
		})
	}
	if got := f.recordedRequests(); len(got) != 0 {
		t.Errorf("Nexus saw %d requests, want none", len(got))
	}
}

func TestCreateUser_RefusesAnswerForAnotherUser(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer User
		want   string
	}{
		{"other id", User{UserID: "someone-else", Source: DefaultSource}, "someone-else"},
		{"other source", User{UserID: testUserID, Source: "LDAP"}, `in source "LDAP"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeNexus(t)
			f.override = func(w http.ResponseWriter, _ *http.Request) bool {
				writeJSON(w, http.StatusOK, tc.answer)
				return true
			}
			srv := f.server("")
			c := newTestClient(t, f, srv, "")
			if _, err := c.CreateUser(context.Background(), testUser(), "pw"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateUser = %v, want the mismatch reported", err)
			}
		})
	}
}

// UpdateUser sends the user as read, so the status an administrator set is
// echoed, and addresses only local users.
func TestUpdateUser_EchoesStatusAndPath(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	seedRole(f, testRoleID)
	seedRole(f, "extra")
	disabled := testUser()
	disabled.Status = UserDisabled
	disabled.Roles = []string{testRoleID, "extra"}
	seedUser(f, disabled)

	u, err := c.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatal(err)
	}
	u.Roles = []string{testRoleID}
	if err := c.UpdateUser(context.Background(), *u); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	r := f.lastRequest()
	if r.Method != http.MethodPut || r.Path != "/service/rest/v1/security/users/"+testUserID {
		t.Errorf("request = %s %s", r.Method, r.Path)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "disabled" || body["source"] != DefaultSource || body["userId"] != testUserID {
		t.Errorf("body = %v, want the disabled status echoed", body)
	}
	if _, ok := body["password"]; ok {
		t.Error("update body carries a password field")
	}
	got, err := c.GetUser(context.Background(), testUserID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != UserDisabled || !slices.Equal(got.Roles, []string{testRoleID}) {
		t.Errorf("after update: %+v", got)
	}
}

func TestUpdateUser_Refusals(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	u := testUser()
	u.Source = "LDAP"
	if err := c.UpdateUser(context.Background(), u); err == nil || !strings.Contains(err.Error(), "only local users") {
		t.Errorf("UpdateUser of an LDAP user = %v, want refused", err)
	}
	u.Source = ""
	if err := c.UpdateUser(context.Background(), u); err == nil || !strings.Contains(err.Error(), "only local users") {
		t.Errorf("UpdateUser without source = %v, want refused", err)
	}
	u.Source, u.Status = DefaultSource, ""
	if err := c.UpdateUser(context.Background(), u); err == nil || !strings.Contains(err.Error(), "echo the status") {
		t.Errorf("UpdateUser without status = %v, want refused", err)
	}
	if got := f.recordedRequests(); len(got) != 0 {
		t.Errorf("Nexus saw %d requests, want none", len(got))
	}
	u.Status = UserActive
	if err := c.UpdateUser(context.Background(), u); !errors.Is(err, ErrBadRequest) || !strings.Contains(err.Error(), "Unable to locate roleId") {
		t.Errorf("UpdateUser with an unknown role = %v, want Nexus's 400", err)
	}
	seedRole(f, testRoleID)
	if err := c.UpdateUser(context.Background(), u); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateUser of a missing user = %v, want ErrNotFound", err)
	}
}

func TestDeleteUser_NamesTheLocalRealmAndIsIdempotent(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	seedUser(f, testUser())
	if err := c.DeleteUser(context.Background(), testUserID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	r := f.lastRequest()
	if r.Method != http.MethodDelete || r.Path != "/service/rest/v1/security/users/"+testUserID || r.Query.Get("realm") != LocalRealm {
		t.Errorf("request = %s %s?%s, want DELETE with realm=%s", r.Method, r.Path, r.Query.Encode(), LocalRealm)
	}
	if err := c.DeleteUser(context.Background(), testUserID); err != nil {
		t.Errorf("DeleteUser of a missing user = %v, want nil", err)
	}
	f.setOverride(func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusForbidden)
		return true
	})
	if err := c.DeleteUser(context.Background(), testUserID); !errors.Is(err, ErrForbidden) {
		t.Errorf("DeleteUser on 403 = %v, want ErrForbidden", err)
	}
}

// A 404 can come from a URL that does not point at Nexus. Neither a
// deletion nor a lookup may then report "gone": the user would stay in
// Nexus with a valid password while the bridge released its finalizer.
func TestNotFoundFromAnotherServerIsNotAbsence(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		http.NotFound(w, r)
		return true
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	ctx := context.Background()

	_, err := c.GetUser(ctx, testUserID)
	if err == nil || errors.Is(err, ErrNotFound) || !errors.Is(err, ErrUnexpectedStatus) || !strings.Contains(err.Error(), "check that the Nexus URL") {
		t.Errorf("GetUser against a 404 server = %v, want an unexpected-status error, not ErrNotFound", err)
	}
	if _, err := c.ListUsers(ctx, "bridge-c."); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("ListUsers against a 404 server = %v, want an error other than ErrNotFound", err)
	}
	if _, err := c.ListRoles(ctx, "bridge-c."); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("ListRoles against a 404 server = %v, want an error other than ErrNotFound", err)
	}
	if err := c.DeleteUser(ctx, testUserID); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteUser against a 404 server = %v, want an error other than ErrNotFound", err)
	}
	if err := c.DeleteRole(ctx, testRoleID); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteRole against a 404 server = %v, want an error other than ErrNotFound", err)
	}
	// The methods that address one object confirm a 404 the same way.
	u := testUser()
	u.Source = DefaultSource
	for name, err := range map[string]error{
		"GetRole":      func() error { _, err := c.GetRole(ctx, testRoleID); return err }(),
		"GetPrivilege": func() error { _, err := c.GetPrivilege(ctx, "nx-repository-view-docker-apps-read"); return err }(),
		"UpdateUser":   c.UpdateUser(ctx, u),
		"UpdateRole":   c.UpdateRole(ctx, Role{ID: testRoleID, Name: testRoleID}),
	} {
		if err == nil || errors.Is(err, ErrNotFound) || !errors.Is(err, ErrUnexpectedStatus) || !strings.Contains(err.Error(), "check that the Nexus URL") {
			t.Errorf("%s against a 404 server = %v, want an unexpected-status error, not ErrNotFound", name, err)
		}
	}
}

// A 404 for one object is ErrNotFound when Nexus confirms it: the listing
// lacks the user or role, or the privilege endpoint serves nx-all.
func TestNotFoundConfirmedByNexus(t *testing.T) {
	f := newFakeNexus(t)
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	ctx := context.Background()
	u := testUser()
	u.Source = DefaultSource
	seedRole(f, testRoleID)
	for name, err := range map[string]error{
		"GetRole":      func() error { _, err := c.GetRole(ctx, "bridge-c.ns.other"); return err }(),
		"GetPrivilege": func() error { _, err := c.GetPrivilege(ctx, "nx-repository-view-docker-gone-read"); return err }(),
		"UpdateUser":   c.UpdateUser(ctx, u),
		"UpdateRole":   c.UpdateRole(ctx, Role{ID: "bridge-c.ns.other", Name: "x"}),
	} {
		if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "not found") {
			t.Errorf("%s of a missing object = %v, want ErrNotFound with Nexus's message", name, err)
		}
	}
	var probed bool
	for _, r := range f.recordedRequests() {
		probed = probed || r.Path == "/service/rest/v1/security/privileges/"+builtInPrivilege
	}
	if !probed {
		t.Error("GetPrivilege did not confirm the 404 against the built-in privilege")
	}
}

// A 404 for one object while Nexus still lists it is a contradiction, not
// an absence.
func TestNotFoundContradictedByTheListing(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut || (r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/service/rest/v1/security/roles/")) {
			nexusError(w, http.StatusNotFound, "not found")
			return true
		}
		return false
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	ctx := context.Background()
	seedRole(f, testRoleID)
	seedUser(f, testUser())
	u := testUser()
	u.Source = DefaultSource
	if _, err := c.GetRole(ctx, testRoleID); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "still lists the role") {
		t.Errorf("GetRole = %v, want the contradiction reported", err)
	}
	if err := c.UpdateRole(ctx, Role{ID: testRoleID, Name: testRoleID}); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "still lists the role") {
		t.Errorf("UpdateRole = %v, want the contradiction reported", err)
	}
	if err := c.UpdateUser(ctx, u); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "still lists the user") {
		t.Errorf("UpdateUser = %v, want the contradiction reported", err)
	}
}

// A DELETE answered with 404 while the listing still shows the object is
// an error, not an idempotent success.
func TestDelete404ButStillListed(t *testing.T) {
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			nexusError(w, http.StatusNotFound, "not found")
			return true
		}
		return false
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	seedRole(f, testRoleID)
	seedUser(f, testUser())
	if err := c.DeleteUser(context.Background(), testUserID); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "still lists the user") {
		t.Errorf("DeleteUser = %v, want the contradiction reported", err)
	}
	if err := c.DeleteRole(context.Background(), testRoleID); err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "still lists the role") {
		t.Errorf("DeleteRole = %v, want the contradiction reported", err)
	}
}
