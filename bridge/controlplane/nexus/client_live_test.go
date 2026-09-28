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
// and pins the Nexus behaviour ADR-0033 builds on. It is skipped unless
// NEXUS_LIVE_URL is set; it creates and deletes users and roles under the
// cluster name "hwib-live", so never point it at a shared Nexus.
//
//	NEXUS_LIVE_URL                base URL, e.g. http://127.0.0.1:18081
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
	admin, err := NewClient(u, os.Getenv("NEXUS_LIVE_USERNAME"), os.Getenv("NEXUS_LIVE_PASSWORD"), nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := admin.Status(ctx); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if ok, err := admin.Writable(ctx); err != nil || !ok {
		t.Fatalf("Writable = %v, %v", ok, err)
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
	var created []string
	t.Cleanup(func() {
		ctx := context.Background()
		for _, id := range created {
			if err := admin.DeleteUser(ctx, id); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
		if err := admin.DeleteRole(ctx, roleID); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	role := Role{ID: roleID, Name: roleID, Description: "managed-by=harbor-workload-identity-bridge cluster=" + cluster, Privileges: privileges}
	if _, err := admin.CreateRole(ctx, role); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if _, err := admin.CreateRole(ctx, role); !errors.Is(err, ErrBadRequest) {
		t.Errorf("CreateRole of an existing id = %v, want ErrBadRequest", err)
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
	withGone := role
	withGone.Privileges = append(slices.Clone(privileges), gone[0])
	if err := admin.UpdateRole(ctx, withGone); !errors.Is(err, ErrBadRequest) {
		t.Errorf("UpdateRole with a missing repository's privilege = %v, want ErrBadRequest", err)
	}

	newUser := func(generation string) (string, string) {
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
			Status: UserActive, Roles: []string{roleID},
		}
		got, err := admin.CreateUser(ctx, user, password)
		if err != nil {
			t.Fatalf("CreateUser(%q): %v", id, err)
		}
		created = append(created, id)
		if !slices.Equal(got.Roles, []string{roleID}) {
			t.Fatalf("CreateUser roles = %v", got.Roles)
		}
		// A duplicate user id is a 500 on Nexus.
		if _, err := admin.CreateUser(ctx, user, password); !errors.Is(err, ErrServer) || !strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), password) {
			t.Errorf("CreateUser of an existing id = %v, want ErrServer naming the duplicate", err)
		}
		return id, password
	}

	gen1, _ := NewGeneration()
	user1, password1 := newUser(gen1)

	// Case-insensitive search, case-sensitive client.
	upper := strings.ToUpper(user1)
	if _, err := admin.CreateUser(ctx, User{UserID: upper, FirstName: "f", LastName: "l", EmailAddress: "x@hwib-live.invalid", Status: UserActive, Roles: []string{roleID}}, password1); err != nil {
		t.Fatalf("CreateUser(%q): %v", upper, err)
	}
	created = append(created, upper)
	if got, err := admin.GetUser(ctx, user1); err != nil || got.UserID != user1 {
		t.Errorf("GetUser(%q) = %+v, %v", user1, got, err)
	}
	if users, err := admin.ListUsers(ctx, ClusterPrefix(cluster)); err != nil || len(users) != 1 || users[0].UserID != user1 {
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
	got, err := admin.GetUser(ctx, user1)
	if err != nil {
		t.Fatal(err)
	}
	got.Status = UserDisabled
	if err := admin.UpdateUser(ctx, *got); err != nil {
		t.Fatalf("UpdateUser(disabled): %v", err)
	}
	if again, err := admin.GetUser(ctx, user1); err != nil || again.Status != UserDisabled || !slices.Equal(again.Roles, []string{roleID}) {
		t.Errorf("after disabling: %+v, %v", again, err)
	}
	got.Status = UserActive
	if err := admin.UpdateUser(ctx, *got); err != nil {
		t.Fatalf("UpdateUser(active): %v", err)
	}

	if registry := os.Getenv("NEXUS_LIVE_REGISTRY_URL"); registry != "" {
		liveBearerTokens(ctx, t, admin, registry, user1, password1, newUser)
	}

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
	for _, id := range created {
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
	created = nil
}

// liveBearerTokens pins why ADR-0033 rotates by replacing the user under a
// new id: Nexus's docker bearer token is a persistent per-user API key
// that survives a password change and a delete plus re-create under the
// same id, and is refused once its user id no longer exists.
func liveBearerTokens(ctx context.Context, t *testing.T, admin Client, registry, userID, password string, newUser func(string) (string, string)) {
	t.Helper()
	token := dockerToken(ctx, t, registry, userID, password)
	if token == "" {
		t.Fatal("no docker bearer token for the new user")
	}
	if code := registryStatus(ctx, t, registry, token); code != http.StatusOK {
		t.Fatalf("GET /v2/ with the token = %d, want 200", code)
	}

	// Delete and re-create under the same id, without presenting the token
	// in between: the old token stays valid.
	if err := admin.DeleteUser(ctx, userID); err != nil {
		t.Fatal(err)
	}
	identity, _, _ := ParseUserID(userID)
	role := identity // the role id is the identity name
	newPassword, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CreateUser(ctx, User{
		UserID: userID, FirstName: "f", LastName: "l", EmailAddress: "nexus-bridge@harbor-workload-identity-bridge.invalid",
		Status: UserActive, Roles: []string{role},
	}, newPassword); err != nil {
		t.Fatal(err)
	}
	if code := registryStatus(ctx, t, registry, token); code != http.StatusOK {
		t.Errorf("token after delete and re-create under the same id: GET /v2/ = %d; Nexus revoked it, ADR-0033's premise changed", code)
	} else {
		t.Logf("confirmed: the docker bearer token survives a delete and re-create under the same id")
	}
	if again := dockerToken(ctx, t, registry, userID, newPassword); again != token {
		t.Errorf("the re-created user got a different token; ADR-0033's premise changed")
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
