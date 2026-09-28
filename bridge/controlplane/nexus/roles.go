// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Role is a Nexus role as the REST API reports it (RoleXOResponse).
type Role struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Privileges  []string `json:"privileges"`
	Roles       []string `json:"roles"`
	// Source and ReadOnly are reported by Nexus and ignored on write.
	Source   string `json:"source,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// roleBody is RoleXORequest.
type roleBody struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Privileges  []string `json:"privileges"`
	Roles       []string `json:"roles"`
}

// Privilege is a Nexus privilege as GET /v1/security/privileges/{name}
// reports it. Actions are upper-case ("READ", "BROWSE").
type Privilege struct {
	Type        string   `json:"type"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	ReadOnly    bool     `json:"readOnly"`
	Format      string   `json:"format,omitempty"`
	Repository  string   `json:"repository,omitempty"`
	Actions     []string `json:"actions,omitempty"`
}

func (r Role) body(op string) ([]byte, error) {
	if err := validateID("role id", r.ID); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if r.Name == "" {
		return nil, fmt.Errorf("%s: a role name is required", op)
	}
	privileges, roles := r.Privileges, r.Roles
	// Nexus reads an absent list as null; send empty lists instead.
	if privileges == nil {
		privileges = []string{}
	}
	if roles == nil {
		roles = []string{}
	}
	for _, p := range privileges {
		if err := validateID("privilege", p); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
	}
	for _, id := range roles {
		if err := validateID("contained role id", id); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
	}
	body, err := jsonBody(roleBody{ID: r.ID, Name: r.Name, Description: r.Description, Privileges: privileges, Roles: roles})
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", op, err)
	}
	return body, nil
}

// GetRole implements Client.
func (c *httpClient) GetRole(ctx context.Context, id string) (*Role, error) {
	op := fmt.Sprintf("get role %q", id)
	if err := validateID("role id", id); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	var role Role
	err := c.do(ctx, request{
		op:     op,
		method: http.MethodGet,
		path:   "/v1/security/roles/" + pathSegment(id),
		query:  url.Values{"source": {DefaultSource}},
		out:    &role,
	})
	if errors.Is(err, ErrNotFound) {
		if confirmErr := confirmAbsence(err, "role", c.rolePresent(ctx, id)); confirmErr != nil {
			return nil, confirmErr
		}
	}
	if err != nil {
		return nil, err
	}
	if role.ID != id {
		return nil, fmt.Errorf("%s: Nexus returned role %q", op, role.ID)
	}
	return &role, nil
}

// rolePresent reports whether Nexus lists the local role (confirmAbsence).
func (c *httpClient) rolePresent(ctx context.Context, id string) func() (bool, error) {
	return func() (bool, error) {
		roles, err := c.ListRoles(ctx, id)
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(roles, func(r Role) bool { return r.ID == id }), nil
	}
}

// ListRoles implements Client. Nexus cannot filter roles by id; the client
// lists the local source and keeps the ids that begin with prefix.
func (c *httpClient) ListRoles(ctx context.Context, prefix string) ([]Role, error) {
	op := fmt.Sprintf("list roles with prefix %q", prefix)
	if err := validateID("role id prefix", prefix); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	var roles []Role
	err := c.do(ctx, request{
		op:     op,
		method: http.MethodGet,
		path:   "/v1/security/roles",
		query:  url.Values{"source": {DefaultSource}},
		out:    &roles,
	})
	if err != nil {
		return nil, listingError(err)
	}
	out := make([]Role, 0, len(roles))
	for _, r := range roles {
		if strings.HasPrefix(r.ID, prefix) && (r.Source == "" || r.Source == DefaultSource) {
			out = append(out, r)
		}
	}
	return out, nil
}

// CreateRole implements Client. The new role is always local: Nexus
// ignores the source of a role create.
func (c *httpClient) CreateRole(ctx context.Context, role Role) (*Role, error) {
	op := fmt.Sprintf("create role %q", role.ID)
	body, err := role.body(op)
	if err != nil {
		return nil, err
	}
	var created Role
	err = c.do(ctx, request{
		op:          op,
		method:      http.MethodPost,
		path:        "/v1/security/roles",
		body:        body,
		contentType: "application/json",
		out:         &created,
	})
	if err != nil {
		return nil, err
	}
	if created.ID != role.ID {
		return nil, fmt.Errorf("%s: Nexus reported the new role as %q", op, created.ID)
	}
	return &created, nil
}

// UpdateRole implements Client. The body's id always equals the path's
// (Nexus answers 409 otherwise). After a success it reads the role back,
// because from 3.91.0 on Nexus stores a role update without the
// privileges that do not exist and still answers 204
// (ErrPrivilegesDropped).
func (c *httpClient) UpdateRole(ctx context.Context, role Role) error {
	op := fmt.Sprintf("update role %q", role.ID)
	body, err := role.body(op)
	if err != nil {
		return err
	}
	err = c.do(ctx, request{
		op:          op,
		method:      http.MethodPut,
		path:        "/v1/security/roles/" + pathSegment(role.ID),
		body:        body,
		contentType: "application/json",
	})
	if errors.Is(err, ErrNotFound) {
		if confirmErr := confirmAbsence(err, "role", c.rolePresent(ctx, role.ID)); confirmErr != nil {
			return confirmErr
		}
	}
	if err != nil {
		return err
	}
	stored, err := c.GetRole(ctx, role.ID)
	if err != nil {
		return fmt.Errorf("%s: the role was updated, but reading it back failed: %w", op, err)
	}
	var dropped, added []string
	for _, p := range role.Privileges {
		if !slices.Contains(stored.Privileges, p) {
			dropped = append(dropped, p)
		}
	}
	for _, p := range stored.Privileges {
		if !slices.Contains(role.Privileges, p) {
			added = append(added, p)
		}
	}
	slices.Sort(dropped)
	slices.Sort(added)
	switch {
	case len(added) > 0:
		return fmt.Errorf("%s: Nexus stored the role with privileges the update did not name: %s", op, strings.Join(added, ", "))
	case len(dropped) > 0:
		return &PrivilegesDroppedError{Op: op, Dropped: slices.Compact(dropped)}
	}
	return nil
}

// DeleteRole implements Client. As with DeleteUser, a 404 counts as
// success only once the role listing confirms the role is gone.
func (c *httpClient) DeleteRole(ctx context.Context, id string) error {
	op := fmt.Sprintf("delete role %q", id)
	if err := validateID("role id", id); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	err := c.do(ctx, request{
		op:     op,
		method: http.MethodDelete,
		path:   "/v1/security/roles/" + pathSegment(id),
	})
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	return confirmAbsence(err, "role", c.rolePresent(ctx, id))
}

// GetPrivilege implements Client.
func (c *httpClient) GetPrivilege(ctx context.Context, name string) (*Privilege, error) {
	op := fmt.Sprintf("get privilege %q", name)
	if err := validateID("privilege", name); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	p, err := c.getPrivilege(ctx, op, name)
	if errors.Is(err, ErrNotFound) {
		// Listing every privilege to confirm the absence can take
		// megabytes on a large Nexus; asking the same endpoint for a
		// privilege that always exists shows as well that the 404 came
		// from Nexus and not from a URL that points elsewhere.
		if confirmErr := confirmAbsence(err, "privilege", func() (bool, error) {
			_, probeErr := c.getPrivilege(ctx, fmt.Sprintf("get built-in privilege %q", builtInPrivilege), builtInPrivilege)
			return false, listingError(probeErr)
		}); confirmErr != nil {
			return nil, confirmErr
		}
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// builtInPrivilege is a privilege every Nexus has: read-only, contributed
// by Nexus itself, never deletable (verified on 3.76.1: readOnly true).
const builtInPrivilege = "nx-all"

func (c *httpClient) getPrivilege(ctx context.Context, op, name string) (*Privilege, error) {
	var p Privilege
	err := c.do(ctx, request{
		op:     op,
		method: http.MethodGet,
		path:   "/v1/security/privileges/" + pathSegment(name),
		out:    &p,
	})
	if err != nil {
		return nil, err
	}
	if p.Name != name {
		return nil, fmt.Errorf("%s: Nexus returned privilege %q", op, p.Name)
	}
	return &p, nil
}
