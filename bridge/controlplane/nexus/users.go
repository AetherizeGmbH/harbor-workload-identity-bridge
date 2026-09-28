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

// UserStatus is a Nexus user's status.
type UserStatus string

// User statuses. Nexus refuses to authenticate a user in any status but
// active, with a password and with its docker bearer token alike; the
// token becomes valid again when the user is re-activated (ADR-0033).
const (
	UserActive         UserStatus = "active"
	UserLocked         UserStatus = "locked"
	UserDisabled       UserStatus = "disabled"
	UserChangePassword UserStatus = "changepassword"
)

// User is a Nexus user as the REST API reports it (ApiUser). It carries no
// password: Nexus never returns one, and CreateUser takes it separately so
// no struct that might be logged holds it.
type User struct {
	UserID       string     `json:"userId"`
	FirstName    string     `json:"firstName"`
	LastName     string     `json:"lastName"`
	EmailAddress string     `json:"emailAddress"`
	Source       string     `json:"source"`
	Status       UserStatus `json:"status"`
	// ReadOnly and ExternalRoles are reported by Nexus and ignored on
	// update.
	ReadOnly      bool     `json:"readOnly,omitempty"`
	Roles         []string `json:"roles"`
	ExternalRoles []string `json:"externalRoles,omitempty"`
}

// createUserBody is ApiCreateUser. It exists only inside CreateUser.
type createUserBody struct {
	UserID       string     `json:"userId"`
	FirstName    string     `json:"firstName"`
	LastName     string     `json:"lastName"`
	EmailAddress string     `json:"emailAddress"`
	Password     string     `json:"password"`
	Status       UserStatus `json:"status"`
	Roles        []string   `json:"roles"`
}

// updateUserBody is the part of ApiUser Nexus reads on update.
type updateUserBody struct {
	UserID       string     `json:"userId"`
	FirstName    string     `json:"firstName"`
	LastName     string     `json:"lastName"`
	EmailAddress string     `json:"emailAddress"`
	Source       string     `json:"source"`
	Status       UserStatus `json:"status"`
	Roles        []string   `json:"roles"`
}

func validStatus(s UserStatus) bool {
	switch s {
	case UserActive, UserLocked, UserDisabled, UserChangePassword:
		return true
	}
	return false
}

// listUsers returns the local users Nexus's id search returns for term:
// every user whose id begins with term, compared case-insensitively
// (verified on Nexus 3.76.1, ADR-0033). source=default restricts it to
// local users and lifts the limit of 100 Nexus applies to other sources.
func (c *httpClient) listUsers(ctx context.Context, op, term string) ([]User, error) {
	var users []User
	err := c.do(ctx, request{
		op:     op,
		method: http.MethodGet,
		path:   "/v1/security/users",
		query:  url.Values{"userId": {term}, "source": {DefaultSource}},
		out:    &users,
	})
	if err != nil {
		return nil, listingError(err)
	}
	return users, nil
}

// GetUser implements Client.
func (c *httpClient) GetUser(ctx context.Context, userID string) (*User, error) {
	op := fmt.Sprintf("get user %q", userID)
	if err := validateID("user id", userID); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	users, err := c.listUsers(ctx, op, userID)
	if err != nil {
		return nil, err
	}
	var found *User
	for i := range users {
		u := &users[i]
		if u.UserID != userID || u.Source != DefaultSource {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%s: Nexus listed the user twice", op)
		}
		found = u
	}
	if found == nil {
		return nil, fmt.Errorf("%s: %w", op, ErrNotFound)
	}
	return found, nil
}

// ListUsers implements Client.
func (c *httpClient) ListUsers(ctx context.Context, prefix string) ([]User, error) {
	op := fmt.Sprintf("list users with prefix %q", prefix)
	if err := validateID("user id prefix", prefix); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	users, err := c.listUsers(ctx, op, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]User, 0, len(users))
	for _, u := range users {
		if u.Source == DefaultSource && strings.HasPrefix(u.UserID, prefix) {
			out = append(out, u)
		}
	}
	return out, nil
}

// CreateUser implements Client.
func (c *httpClient) CreateUser(ctx context.Context, user User, password string) (*User, error) {
	op := fmt.Sprintf("create user %q", user.UserID)
	if err := validateID("user id", user.UserID); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	switch {
	case password == "":
		return nil, fmt.Errorf("%s: a password is required", op)
	case !validStatus(user.Status):
		return nil, fmt.Errorf("%s: status %q is not a Nexus user status", op, user.Status)
	case len(user.Roles) == 0:
		// Nexus requires at least one role (ApiCreateUser.roles @NotEmpty).
		return nil, fmt.Errorf("%s: at least one role is required", op)
	case user.Source != "" && user.Source != DefaultSource:
		return nil, fmt.Errorf("%s: only local users (source %q) can be created, not source %q", op, DefaultSource, user.Source)
	}
	for _, r := range user.Roles {
		if err := validateID("role id", r); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
	}
	body, err := jsonBody(createUserBody{
		UserID:       user.UserID,
		FirstName:    user.FirstName,
		LastName:     user.LastName,
		EmailAddress: user.EmailAddress,
		Password:     password,
		Status:       user.Status,
		Roles:        user.Roles,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", op, err)
	}
	var created User
	err = c.do(ctx, request{
		op:          op,
		method:      http.MethodPost,
		path:        "/v1/security/users",
		body:        body,
		contentType: "application/json",
		secrets:     []string{password},
		out:         &created,
	})
	if err != nil {
		return nil, err
	}
	if created.UserID != user.UserID || created.Source != DefaultSource {
		return nil, fmt.Errorf("%s: Nexus reported the new user as %q in source %q", op, created.UserID, created.Source)
	}
	return &created, nil
}

// UpdateUser implements Client.
func (c *httpClient) UpdateUser(ctx context.Context, user User) error {
	op := fmt.Sprintf("update user %q", user.UserID)
	if err := validateID("user id", user.UserID); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if user.Source != DefaultSource {
		// For another source Nexus would assign the roles to that
		// source's user with the same id (an LDAP user, say).
		return fmt.Errorf("%s: only local users (source %q) can be updated, not source %q", op, DefaultSource, user.Source)
	}
	if !validStatus(user.Status) {
		return fmt.Errorf("%s: status %q is not a Nexus user status; echo the status read from Nexus", op, user.Status)
	}
	roles := user.Roles
	if roles == nil {
		roles = []string{}
	}
	for _, r := range roles {
		if err := validateID("role id", r); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	body, err := jsonBody(updateUserBody{
		UserID:       user.UserID,
		FirstName:    user.FirstName,
		LastName:     user.LastName,
		EmailAddress: user.EmailAddress,
		Source:       DefaultSource,
		Status:       user.Status,
		Roles:        roles,
	})
	if err != nil {
		return fmt.Errorf("%s: encode request: %w", op, err)
	}
	return c.do(ctx, request{
		op:          op,
		method:      http.MethodPut,
		path:        "/v1/security/users/" + pathSegment(user.UserID),
		body:        body,
		contentType: "application/json",
	})
}

// DeleteUser implements Client. A 404 counts as success only once the user
// listing confirms the user is gone: a 404 can also come from a URL that
// does not point at Nexus, and trusting it would leave the user behind.
func (c *httpClient) DeleteUser(ctx context.Context, userID string) error {
	op := fmt.Sprintf("delete user %q", userID)
	if err := validateID("user id", userID); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	err := c.do(ctx, request{
		op:     op,
		method: http.MethodDelete,
		path:   "/v1/security/users/" + pathSegment(userID),
		query:  url.Values{"realm": {LocalRealm}},
	})
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	switch _, getErr := c.GetUser(ctx, userID); {
	case errors.Is(getErr, ErrNotFound):
		return nil
	// err itself is not wrapped: it matches ErrNotFound, and a caller
	// must not read these outcomes as "the user is gone".
	case getErr != nil:
		return fmt.Errorf("%s; confirming that the user is gone failed: %w", err.Error(), getErr)
	default:
		return fmt.Errorf("%s, but Nexus still lists the user", err.Error())
	}
}
