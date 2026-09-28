// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexustest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
)

// The fake behaves as the client expects Nexus to: these are the rows of
// ADR-0036's runtime table that code under test relies on.
func TestServer_BehavesLikeNexus(t *testing.T) {
	ctx := context.Background()
	s := New(t)
	c := s.Client(t)
	s.AddRepository("docker", "hosted")
	read := "nx-repository-view-docker-hosted-read"

	if _, err := c.GetPrivilege(ctx, read); err != nil {
		t.Fatalf("GetPrivilege of an existing repository: %v", err)
	}
	if _, err := c.GetPrivilege(ctx, "nx-repository-view-docker-missing-read"); !errors.Is(err, nexus.ErrNotFound) {
		t.Fatalf("GetPrivilege of a missing repository: %v, want ErrNotFound", err)
	}

	// A user created before its role holds the role hidden until the role
	// exists (ADR-0036 Context 5).
	u, err := c.CreateUser(ctx, nexus.User{UserID: "u1", FirstName: "f", LastName: "l", EmailAddress: "x@y.invalid",
		Status: nexus.UserActive, Roles: []string{"r1"}}, "pw-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Roles) != 0 {
		t.Errorf("an unknown role is visible: %v", u.Roles)
	}
	if _, err := c.CreateRole(ctx, nexus.Role{ID: "r1", Name: "r1", Privileges: []string{read}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.User("u1"); !slices.Equal(got.Roles, []string{"r1"}) {
		t.Errorf("roles after the role was created = %v", got.Roles)
	}
	if !s.Authenticate("u1", "pw-1") || s.Authenticate("u1", "wrong") {
		t.Error("Authenticate does not check the password")
	}
	s.SetUserStatus("u1", nexus.UserDisabled)
	if s.Authenticate("u1", "pw-1") {
		t.Error("a disabled user authenticates")
	}

	// Deleting the repository strips its privileges from the role.
	s.DeleteRepository("docker", "hosted")
	if r, _ := s.Role("r1"); len(r.Privileges) != 0 {
		t.Errorf("privileges after the repository was deleted = %v", r.Privileges)
	}
	// Before 3.91.0 a role update naming it is refused; from 3.91.0 on it
	// is stored without it.
	err = c.UpdateRole(ctx, nexus.Role{ID: "r1", Name: "r1", Privileges: []string{read}})
	if !errors.Is(err, nexus.ErrBadRequest) {
		t.Errorf("UpdateRole with a missing privilege: %v, want ErrBadRequest", err)
	}
	s.SetDropOrphanPrivileges(true)
	err = c.UpdateRole(ctx, nexus.Role{ID: "r1", Name: "r1", Privileges: []string{read}})
	if !errors.Is(err, nexus.ErrPrivilegesDropped) {
		t.Errorf("UpdateRole on 3.91+: %v, want ErrPrivilegesDropped", err)
	}

	// Duplicates.
	if _, err := c.CreateUser(ctx, nexus.User{UserID: "u1", FirstName: "f", LastName: "l", EmailAddress: "x@y.invalid",
		Status: nexus.UserActive, Roles: []string{"r1"}}, "pw-2"); !errors.Is(err, nexus.ErrServer) {
		t.Errorf("duplicate user: %v, want ErrServer", err)
	}
	if _, err := c.CreateRole(ctx, nexus.Role{ID: "r1", Name: "r1"}); !errors.Is(err, nexus.ErrBadRequest) {
		t.Errorf("duplicate role: %v, want ErrBadRequest", err)
	}

	// Deleting the role removes it from every user.
	if err := c.DeleteRole(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.User("u1"); len(got.Roles) != 0 {
		t.Errorf("roles after the role was deleted = %v", got.Roles)
	}
	if err := c.DeleteUser(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteUser(ctx, "u1"); err != nil {
		t.Errorf("deleting a missing user: %v", err)
	}

	s.SetPasswordPolicy(func(p string) bool { return len(p) > 100 })
	if _, err := c.CreateUser(ctx, nexus.User{UserID: "u2", FirstName: "f", LastName: "l", EmailAddress: "x@y.invalid",
		Status: nexus.UserActive, Roles: []string{"r1"}}, "short"); !errors.Is(err, nexus.ErrBadRequest) {
		t.Errorf("password refused by the policy: %v, want ErrBadRequest", err)
	}
	s.SetPasswordPolicy(nil)

	s.SetRateLimited(true)
	if _, err := c.ListRoles(ctx, "r"); !errors.Is(err, nexus.ErrRateLimited) {
		t.Errorf("rate limited: %v, want ErrRateLimited", err)
	}
	s.SetRateLimited(false)
	s.SetUnavailable(true)
	if _, err := c.ListRoles(ctx, "r"); !errors.Is(err, nexus.ErrServer) {
		t.Errorf("unavailable: %v, want ErrServer", err)
	}
	s.SetUnavailable(false)

	if n := len(s.Requests()); n == 0 {
		t.Error("no request recorded")
	}
}
