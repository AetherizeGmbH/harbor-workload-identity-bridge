// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestLive_AgainstNexus runs the client against a real, disposable Nexus
// and pins the Nexus behaviour ADR-0033 builds on (its "Verified at
// runtime" table says which rows this covers). It is skipped unless
// NEXUS_LIVE_URL is set; it creates and deletes users and roles under the
// cluster name "hwib-live", so never point it at a shared Nexus.
//
//	NEXUS_LIVE_URL                base URL without /service/rest,
//	                              e.g. http://127.0.0.1:18081
//	NEXUS_LIVE_USERNAME/PASSWORD  an administrator
//	NEXUS_LIVE_DOCKER_REPOSITORY  an existing docker hosted repository
//	NEXUS_LIVE_REGISTRY_URL       optional: that repository's docker
//	                              endpoint (connector), with the DockerToken
//	                              realm active and anonymous access off;
//	                              enables the bearer-token checks
func TestLive_AgainstNexus(t *testing.T) {
	rawURL := os.Getenv("NEXUS_LIVE_URL")
	if rawURL == "" {
		t.Skip("NEXUS_LIVE_URL is not set")
	}
	repo := os.Getenv("NEXUS_LIVE_DOCKER_REPOSITORY")
	if repo == "" {
		t.Fatal("NEXUS_LIVE_DOCKER_REPOSITORY is required")
	}
	u := mustParse(t, rawURL)
	var opts []Option
	if u.Scheme == "http" {
		opts = append(opts, WithAllowInsecureHTTP())
	}
	adminName, adminPassword := os.Getenv("NEXUS_LIVE_USERNAME"), os.Getenv("NEXUS_LIVE_PASSWORD")
	admin, err := NewClient(u, adminName, adminPassword, nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	raw := liveRaw{api: strings.TrimRight(rawURL, "/") + apiBasePath, username: adminName, password: adminPassword}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := admin.Status(ctx); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if ok, err := admin.Writable(ctx); err != nil || !ok {
		t.Fatalf("Writable = %v, %v", ok, err)
	}
	// Protected endpoints answer 403 to a request without credentials.
	if code := raw.status(ctx, t, http.MethodGet, "/v1/security/users", "", false); code != http.StatusForbidden {
		t.Errorf("user listing without credentials = %d, want 403", code)
	}

	// Repository-view privileges exist exactly for existing repositories,
	// and their names are case-sensitive.
	privileges, err := RepositoryPrivileges(FormatDocker, repo, AccessPull)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range privileges {
		if _, err := admin.GetPrivilege(ctx, p); err != nil {
			t.Fatalf("GetPrivilege(%q): %v", p, err)
		}
	}
	gone, _ := RepositoryPrivileges(FormatDocker, "hwib-live-no-such-repository", AccessPull)
	if _, err := admin.GetPrivilege(ctx, gone[0]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetPrivilege of a missing repository = %v, want ErrNotFound", err)
	}
	if upper := strings.ToUpper(privileges[0]); upper != privileges[0] {
		if _, err := admin.GetPrivilege(ctx, upper); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetPrivilege(%q) = %v, want ErrNotFound (names are case-sensitive)", upper, err)
		}
	}

	suffix, err := NewGeneration()
	if err != nil {
		t.Fatal(err)
	}
	const cluster = "hwib-live"
	ns, sa := "ns-"+suffix[:8], "sa"
	roleID, err := RoleID(cluster, ns, sa)
	if err != nil {
		t.Fatal(err)
	}
	var createdUsers, createdRoles []string
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range createdUsers {
			if err := admin.DeleteUser(ctx, id); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
		for _, id := range createdRoles {
			if err := admin.DeleteRole(ctx, id); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
	})

	role := Role{ID: roleID, Name: roleID, Description: "managed-by=harbor-workload-identity-bridge cluster=" + cluster, Privileges: privileges}
	if _, err := admin.CreateRole(ctx, role); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	createdRoles = append(createdRoles, roleID)
	if _, err := admin.CreateRole(ctx, role); !errors.Is(err, ErrBadRequest) {
		t.Errorf("CreateRole of an existing id = %v, want ErrBadRequest", err)
	}
	withGone := role
	withGone.ID, withGone.Name = roleID+".gone", roleID+".gone"
	withGone.Privileges = append(slices.Clone(privileges), gone[0])
	if _, err := admin.CreateRole(ctx, withGone); !errors.Is(err, ErrBadRequest) {
		t.Errorf("CreateRole with a missing repository's privilege = %v, want ErrBadRequest", err)
	}
	gotRole, err := admin.GetRole(ctx, roleID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSet(gotRole.Privileges, privileges) {
		t.Errorf("role privileges = %v, want %v", gotRole.Privileges, privileges)
	}
	if roles, err := admin.ListRoles(ctx, ClusterPrefix(cluster)); err != nil || !slices.ContainsFunc(roles, func(r Role) bool { return r.ID == roleID }) {
		t.Errorf("ListRoles = %v, %v; want it to contain %q", roles, err, roleID)
	}
	// A role update naming a privilege that does not exist: 400 and
	// nothing stored before Nexus 3.91.0, the role stored without it from
	// 3.91.0 on. Either way the stored role never holds it.
	withGone = role
	withGone.Privileges = append(slices.Clone(privileges), gone[0])
	err = admin.UpdateRole(ctx, withGone)
	if !errors.Is(err, ErrBadRequest) && !errors.Is(err, ErrPrivilegesDropped) {
		t.Errorf("UpdateRole with a missing repository's privilege = %v, want ErrBadRequest or ErrPrivilegesDropped", err)
	}
	t.Logf("role update with a missing privilege: %v", err)
	if gotRole, err := admin.GetRole(ctx, roleID); err != nil || !sameSet(gotRole.Privileges, privileges) {
		t.Errorf("role after the update with a missing privilege = %+v, %v; want exactly %v", gotRole, err, privileges)
	}

	newUserWithRoles := func(generation string, roles []string) (string, string, *User) {
		t.Helper()
		id, err := UserID(cluster, ns, sa, generation)
		if err != nil {
			t.Fatal(err)
		}
		password, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		user := User{
			UserID: id, FirstName: "managed-by=harbor-workload-identity-bridge cluster=" + cluster,
			LastName: "nexusaccess=" + ns + "/live", EmailAddress: "nexus-bridge@harbor-workload-identity-bridge.invalid",
			Status: UserActive, Roles: roles,
		}
		got, err := admin.CreateUser(ctx, user, password)
		if err != nil {
			t.Fatalf("CreateUser(%q): %v", id, err)
		}
		createdUsers = append(createdUsers, id)
		// A duplicate user id is a 500 on Nexus.
		if _, err := admin.CreateUser(ctx, user, password); !errors.Is(err, ErrServer) || !strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), password) {
			t.Errorf("CreateUser of an existing id = %v, want ErrServer naming the duplicate", err)
		}
		return id, password, got
	}
	newUser := func(generation string) (string, string) {
		t.Helper()
		id, password, got := newUserWithRoles(generation, []string{roleID})
		if !slices.Equal(got.Roles, []string{roleID}) {
			t.Fatalf("CreateUser roles = %v", got.Roles)
		}
		return id, password
	}

	// A user create naming a role that does not exist succeeds; the
	// read-back hides the role, and the user gains it once the role is
	// created.
	later := roleID + ".later"
	genLater, _ := NewGeneration()
	laterUser, _, got := newUserWithRoles(genLater, []string{roleID, later})
	if !slices.Equal(got.Roles, []string{roleID}) {
		t.Errorf("CreateUser with a role that does not exist returned roles %v, want only %q", got.Roles, roleID)
	}
	if _, err := admin.CreateRole(ctx, Role{ID: later, Name: later}); err != nil {
		t.Fatalf("CreateRole(%q): %v", later, err)
	}
	createdRoles = append(createdRoles, later)
	if again, err := admin.GetUser(ctx, laterUser); err != nil || !sameSet(again.Roles, []string{roleID, later}) {
		t.Errorf("user after the role was created = %+v, %v; want roles %q and %q", again, err, roleID, later)
	}
	// A user update naming a role that does not exist is refused.
	if again, err := admin.GetUser(ctx, laterUser); err == nil {
		again.Roles = append(again.Roles, roleID+".nope")
		if err := admin.UpdateUser(ctx, *again); !errors.Is(err, ErrBadRequest) {
			t.Errorf("UpdateUser with a role that does not exist = %v, want ErrBadRequest", err)
		}
	}

	// User ids: 200 characters are accepted, 201 fail with 500.
	long200 := ClusterPrefix(cluster) + strings.Repeat("x", maxIDLen-len(ClusterPrefix(cluster)))
	longPassword, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CreateUser(ctx, User{UserID: long200, FirstName: "f", LastName: "l", EmailAddress: "x@hwib-live.invalid", Status: UserActive, Roles: []string{roleID}}, longPassword); err != nil {
		t.Errorf("CreateUser with a %d-character id: %v", len(long200), err)
	} else {
		createdUsers = append(createdUsers, long200)
	}
	body201, err := json.Marshal(createUserBody{UserID: long200 + "x", FirstName: "f", LastName: "l", EmailAddress: "x@hwib-live.invalid", Password: longPassword, Status: UserActive, Roles: []string{roleID}})
	if err != nil {
		t.Fatal(err)
	}
	if code := raw.status(ctx, t, http.MethodPost, "/v1/security/users", string(body201), true); code != http.StatusInternalServerError {
		t.Errorf("user create with a 201-character id = %d, want 500", code)
		if code/100 == 2 {
			createdUsers = append(createdUsers, long200+"x")
		}
	}

	gen1, _ := NewGeneration()
	user1, password1 := newUser(gen1)

	// Case-insensitive search, case-sensitive client.
	upper := strings.ToUpper(user1)
	if _, err := admin.CreateUser(ctx, User{UserID: upper, FirstName: "f", LastName: "l", EmailAddress: "x@hwib-live.invalid", Status: UserActive, Roles: []string{roleID}}, password1); err != nil {
		t.Fatalf("CreateUser(%q): %v", upper, err)
	}
	createdUsers = append(createdUsers, upper)
	if got, err := admin.GetUser(ctx, user1); err != nil || got.UserID != user1 {
		t.Errorf("GetUser(%q) = %+v, %v", user1, got, err)
	}
	if users, err := admin.ListUsers(ctx, ClusterPrefix(cluster)+ns+"."+sa+"_"+gen1[:4]); err != nil || len(users) != 1 || users[0].UserID != user1 {
		t.Errorf("ListUsers = %+v, %v; want exactly %q", users, err, user1)
	}

	// The generated password authenticates: the user is known but lacks
	// nexus:users:read (403), a wrong password is 401.
	asUser, err := NewClient(u, user1, password1, nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := asUser.ListUsers(ctx, "x"); !errors.Is(err, ErrForbidden) {
		t.Errorf("ListUsers as the new user = %v, want ErrForbidden", err)
	}
	wrong, err := NewClient(u, user1, password1+"x", nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.ListUsers(ctx, "x"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("ListUsers with a wrong password = %v, want ErrUnauthorized", err)
	}

	// Disable and re-enable, echoing everything else.
	liveSetStatus(ctx, t, admin, user1, UserDisabled)
	if again, err := admin.GetUser(ctx, user1); err != nil || again.Status != UserDisabled || !slices.Equal(again.Roles, []string{roleID}) {
		t.Errorf("after disabling: %+v, %v", again, err)
	}
	liveSetStatus(ctx, t, admin, user1, UserActive)

	if registry := os.Getenv("NEXUS_LIVE_REGISTRY_URL"); registry != "" {
		liveBearerTokens(ctx, t, admin, registry, user1, password1, newUser)
	}

	liveLeastPrivilegeAdmin(ctx, t, admin, u, opts, suffix, privileges, &createdUsers, &createdRoles)

	// Deleting the role removes it from the user (the upper-case one: the
	// bearer-token checks replace user1).
	if err := admin.DeleteRole(ctx, roleID); err != nil {
		t.Fatalf("DeleteRole: %v", err)
	}
	if again, err := admin.GetUser(ctx, upper); err != nil || len(again.Roles) != 0 {
		t.Errorf("user after role delete: %+v, %v", again, err)
	}
	if err := admin.DeleteRole(ctx, roleID); err != nil {
		t.Errorf("DeleteRole of a missing role = %v, want nil", err)
	}
	if err := admin.UpdateRole(ctx, role); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateRole of a missing role = %v, want ErrNotFound", err)
	}
	if _, err := admin.GetRole(ctx, roleID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetRole of a missing role = %v, want ErrNotFound", err)
	}
	for _, id := range createdUsers {
		if err := admin.DeleteUser(ctx, id); err != nil {
			t.Errorf("DeleteUser(%q): %v", id, err)
		}
		if err := admin.DeleteUser(ctx, id); err != nil {
			t.Errorf("DeleteUser(%q) again = %v, want nil", id, err)
		}
		if _, err := admin.GetUser(ctx, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetUser(%q) after delete = %v, want ErrNotFound", id, err)
		}
	}
	createdUsers = nil
}

func liveSetStatus(ctx context.Context, t *testing.T, admin Client, userID string, status UserStatus) {
	t.Helper()
	got, err := admin.GetUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	got.Status = status
	if err := admin.UpdateUser(ctx, *got); err != nil {
		t.Fatalf("UpdateUser(%q, %s): %v", userID, status, err)
	}
}

// liveBearerTokens pins why ADR-0033 rotates by replacing the user under a
// new id: Nexus's docker bearer token is a persistent per-user API key
// that keeps working for a changepassword user, is valid again after a
// disable plus re-enable, and survives a delete plus re-create under the
// same id; it is dropped once presented while its user id does not exist,
// and refused once its user is replaced under a new id.
func liveBearerTokens(ctx context.Context, t *testing.T, admin Client, registry, userID, password string, newUser func(string) (string, string)) {
	t.Helper()
	token := dockerToken(ctx, t, registry, userID, password)
	if token == "" {
		t.Fatal("no docker bearer token for the new user")
	}
	if code := registryStatus(ctx, t, registry, token); code != http.StatusOK {
		t.Fatalf("GET /v2/ with the token = %d, want 200", code)
	}

	// changepassword counts as active: password and token keep working.
	liveSetStatus(ctx, t, admin, userID, UserChangePassword)
	if code := registryStatus(ctx, t, registry, token); code != http.StatusOK {
		t.Errorf("token of a changepassword user: GET /v2/ = %d, want 200", code)
	}
	if again := dockerToken(ctx, t, registry, userID, password); again != token {
		t.Errorf("a changepassword user got a different token")
	}
	liveSetStatus(ctx, t, admin, userID, UserActive)

	// Disabled: refused; re-enabled: the same token is valid again.
	liveSetStatus(ctx, t, admin, userID, UserDisabled)
	if code := registryStatus(ctx, t, registry, token); code != http.StatusUnauthorized {
		t.Errorf("token of a disabled user: GET /v2/ = %d, want 401", code)
	}
	liveSetStatus(ctx, t, admin, userID, UserActive)
	if code := registryStatus(ctx, t, registry, token); code != http.StatusOK {
		t.Errorf("token after re-enabling: GET /v2/ = %d, want 200 (ADR-0033 rejects S2 because of this)", code)
	}

	identity, _, _ := ParseUserID(userID)
	role := identity // the role id is the identity name
	recreate := func() string {
		t.Helper()
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.CreateUser(ctx, User{
			UserID: userID, FirstName: "f", LastName: "l", EmailAddress: "nexus-bridge@harbor-workload-identity-bridge.invalid",
			Status: UserActive, Roles: []string{role},
		}, pw); err != nil {
			t.Fatal(err)
		}
		return pw
	}

	// Delete and re-create under the same id, without presenting the token
	// in between: the old token stays valid.
	if err := admin.DeleteUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	newPassword := recreate()
	if code := registryStatus(ctx, t, registry, token); code != http.StatusOK {
		t.Errorf("token after delete and re-create under the same id: GET /v2/ = %d; Nexus revoked it, ADR-0033's premise changed", code)
	} else {
		t.Logf("confirmed: the docker bearer token survives a delete and re-create under the same id")
	}
	if again := dockerToken(ctx, t, registry, userID, newPassword); again != token {
		t.Errorf("the re-created user got a different token; ADR-0033's premise changed")
	}

	// Presented while its user id does not exist, the token is dropped:
	// 401, still 401 after re-creation, and the re-created user gets a new
	// token.
	if err := admin.DeleteUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if code := registryStatus(ctx, t, registry, token); code != http.StatusUnauthorized {
		t.Errorf("token presented while its user is absent: GET /v2/ = %d, want 401", code)
	}
	thirdPassword := recreate()
	if code := registryStatus(ctx, t, registry, token); code != http.StatusUnauthorized {
		t.Errorf("token presented while absent, after re-creation: GET /v2/ = %d, want 401", code)
	}
	token = dockerToken(ctx, t, registry, userID, thirdPassword)
	if code := registryStatus(ctx, t, registry, token); code != http.StatusOK {
		t.Errorf("the re-created user's new token: GET /v2/ = %d, want 200", code)
	}

	// Replace the user under a new id and delete the old one: the old
	// token is refused.
	gen2, err := NewGeneration()
	if err != nil {
		t.Fatal(err)
	}
	user2, password2 := newUser(gen2)
	if err := admin.DeleteUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	if code := registryStatus(ctx, t, registry, token); code != http.StatusUnauthorized {
		t.Errorf("old token after replacing the user under a new id: GET /v2/ = %d, want 401", code)
	}
	token2 := dockerToken(ctx, t, registry, user2, password2)
	if token2 == "" || token2 == token {
		t.Errorf("the replacement user's token is empty or equals the old one")
	}
	if code := registryStatus(ctx, t, registry, token2); code != http.StatusOK {
		t.Errorf("replacement user's token: GET /v2/ = %d, want 200", code)
	}
}

// liveLeastPrivilegeAdmin runs every client call as a user whose only role
// holds nx-users-all, nx-roles-all and nx-privileges-read: the privileges
// ADR-0033 says the bridge's credential needs, without nx-all.
func liveLeastPrivilegeAdmin(ctx context.Context, t *testing.T, admin Client, u *url.URL, opts []Option, suffix string, privileges []string, createdUsers, createdRoles *[]string) {
	t.Helper()
	adminRole := "hwib-live-admin-" + suffix
	if _, err := admin.CreateRole(ctx, Role{ID: adminRole, Name: adminRole, Privileges: []string{"nx-privileges-read", "nx-roles-all", "nx-users-all"}}); err != nil {
		t.Fatalf("CreateRole(%q): %v", adminRole, err)
	}
	*createdRoles = append(*createdRoles, adminRole)
	adminUser := adminRole
	adminPassword, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CreateUser(ctx, User{UserID: adminUser, FirstName: "f", LastName: "l", EmailAddress: "x@hwib-live.invalid", Status: UserActive, Roles: []string{adminRole}}, adminPassword); err != nil {
		t.Fatalf("CreateUser(%q): %v", adminUser, err)
	}
	*createdUsers = append(*createdUsers, adminUser)
	c, err := NewClient(u, adminUser, adminPassword, nil, opts...)
	if err != nil {
		t.Fatal(err)
	}

	const cluster = "hwib-live-lp"
	roleID, _ := RoleID(cluster, "ns-"+suffix[:8], "sa")
	gen, _ := NewGeneration()
	userID, _ := UserID(cluster, "ns-"+suffix[:8], "sa", gen)
	password, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range privileges {
		if _, err := c.GetPrivilege(ctx, p); err != nil {
			t.Errorf("least privilege: GetPrivilege(%q): %v", p, err)
		}
	}
	if _, err := c.GetPrivilege(ctx, "nx-repository-view-docker-hwib-live-no-such-repository-read"); !errors.Is(err, ErrNotFound) {
		t.Errorf("least privilege: GetPrivilege of a missing repository = %v, want ErrNotFound", err)
	}
	if _, err := c.CreateRole(ctx, Role{ID: roleID, Name: roleID, Privileges: privileges}); err != nil {
		t.Fatalf("least privilege: CreateRole: %v", err)
	}
	*createdRoles = append(*createdRoles, roleID)
	if err := c.UpdateRole(ctx, Role{ID: roleID, Name: roleID, Description: "updated", Privileges: privileges}); err != nil {
		t.Errorf("least privilege: UpdateRole: %v", err)
	}
	if roles, err := c.ListRoles(ctx, ClusterPrefix(cluster)); err != nil || len(roles) != 1 {
		t.Errorf("least privilege: ListRoles = %v, %v", roles, err)
	}
	created, err := c.CreateUser(ctx, User{UserID: userID, FirstName: "f", LastName: "l", EmailAddress: "x@hwib-live.invalid", Status: UserActive, Roles: []string{roleID}}, password)
	if err != nil {
		t.Fatalf("least privilege: CreateUser: %v", err)
	}
	*createdUsers = append(*createdUsers, userID)
	created.Status = UserDisabled
	if err := c.UpdateUser(ctx, *created); err != nil {
		t.Errorf("least privilege: UpdateUser: %v", err)
	}
	if users, err := c.ListUsers(ctx, ClusterPrefix(cluster)); err != nil || len(users) != 1 || users[0].Status != UserDisabled {
		t.Errorf("least privilege: ListUsers = %+v, %v", users, err)
	}
	if err := c.DeleteUser(ctx, userID); err != nil {
		t.Errorf("least privilege: DeleteUser: %v", err)
	}
	if err := c.DeleteRole(ctx, roleID); err != nil {
		t.Errorf("least privilege: DeleteRole: %v", err)
	}
	if _, err := c.GetRole(ctx, roleID); !errors.Is(err, ErrNotFound) {
		t.Errorf("least privilege: GetRole after delete = %v, want ErrNotFound", err)
	}
}

// liveRaw sends requests the client refuses to build (no credentials, an
// id longer than Nexus's column).
type liveRaw struct {
	api                string
	username, password string
}

func (r liveRaw) status(ctx context.Context, t *testing.T, method, path, body string, auth bool) int {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, r.api+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.SetBasicAuth(r.username, r.password)
	}
	resp, err := registryHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

var registryHTTP = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func dockerToken(ctx context.Context, t *testing.T, registry, username, password string) string {
	t.Helper()
	q := url.Values{"account": {username}, "service": {registry + "/v2/token"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registry+"/v2/token?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(username, password)
	resp, err := registryHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("docker token for %q: status %d", username, resp.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Token
}

func registryStatus(ctx context.Context, t *testing.T, registry, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registry+"/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := registryHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
