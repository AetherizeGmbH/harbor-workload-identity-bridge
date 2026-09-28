// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
	if err != nil {
		return nil, err
	}
	if role.ID != id {
		return nil, fmt.Errorf("%s: Nexus returned role %q", op, role.ID)
	}
	return &role, nil
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
// (Nexus answers 409 otherwise).
func (c *httpClient) UpdateRole(ctx context.Context, role Role) error {
	op := fmt.Sprintf("update role %q", role.ID)
	body, err := role.body(op)
	if err != nil {
		return err
	}
	return c.do(ctx, request{
		op:          op,
		method:      http.MethodPut,
		path:        "/v1/security/roles/" + pathSegment(role.ID),
		body:        body,
		contentType: "application/json",
	})
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
	// err itself is not wrapped: it matches ErrNotFound, and a caller
	// must not read these outcomes as "the role is gone".
	roles, listErr := c.ListRoles(ctx, id)
	if listErr != nil {
		return fmt.Errorf("%s; confirming that the role is gone failed: %w", err.Error(), listErr)
	}
	for _, r := range roles {
		if r.ID == id {
			return fmt.Errorf("%s, but Nexus still lists the role", err.Error())
		}
	}
	return nil
}

// GetPrivilege implements Client.
func (c *httpClient) GetPrivilege(ctx context.Context, name string) (*Privilege, error) {
	op := fmt.Sprintf("get privilege %q", name)
	if err := validateID("privilege", name); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
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
