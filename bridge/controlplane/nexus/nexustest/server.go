// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Package nexustest provides a fake Nexus Repository REST API for tests of
// code that drives Nexus through bridge/controlplane/nexus.Client. It is
// an httptest server, so the code under test runs the real client over
// HTTP, with its read-backs, 404 confirmations and error mapping.
//
// It reproduces the behaviour verified against sonatype/nexus3:3.76.1
// (ADR-0033, and the package-internal fake of bridge/controlplane/nexus):
// the case-insensitive user id prefix search, 500 text/plain for a
// duplicate user, 400 for a duplicate role, a user create that stores an
// unknown role id hidden from reads until a role with that id exists, 400
// for a user update naming an unknown role, 400 for a role create (and
// before 3.91.0 a role update) naming a privilege that does not exist, 409
// for a role update whose body id differs from the path, repository
// deletion stripping the repository's privileges from every role, a role
// deletion removing the role from every user, JSON error documents, 401
// for wrong and 403 for missing credentials, and the built-in privilege
// nx-all. Users authenticate only while their status is active or
// changepassword.
package nexustest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
)

// Admin credentials the fake accepts.
const (
	AdminUsername = "bridge-admin-7f3a"
	AdminPassword = "ADMIN-PASSWORD-123"
)

// repositoryActions are the actions of Nexus's built-in repository-view
// privileges.
var repositoryActions = []string{"browse", "read", "edit", "add", "delete"}

// Server is a fake Nexus. All methods are safe for concurrent use.
type Server struct {
	// URL is the base URL to configure the client with.
	URL *url.URL

	srv *httptest.Server

	mu         sync.Mutex
	users      map[string]nexus.User
	passwords  map[string]string
	roles      map[string]nexus.Role
	privileges map[string]nexus.Privilege
	requests   []Request

	unavailable          bool
	rateLimited          bool
	dropOrphanPrivileges bool
	passwordPolicy       func(string) bool
	hook                 func(w http.ResponseWriter, r *http.Request) bool
}

// Request is one request the fake received.
type Request struct {
	Method string
	Path   string // below /service/rest
	Query  url.Values
	Admin  bool // carried the admin credentials
}

// New starts a fake Nexus that is stopped when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		users:      map[string]nexus.User{},
		passwords:  map[string]string{},
		roles:      map[string]nexus.Role{},
		privileges: map[string]nexus.Privilege{"nx-all": {Type: "application", Name: "nx-all", ReadOnly: true}},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	u, err := url.Parse(s.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.URL = u
	return s
}

// Client returns a nexus.Client for the fake with the admin credentials.
func (s *Server) Client(t testing.TB, opts ...nexus.Option) nexus.Client {
	t.Helper()
	c, err := nexus.NewClient(s.URL, AdminUsername, AdminPassword, s.srv.Client().Transport,
		append([]nexus.Option{nexus.WithAllowInsecureHTTP()}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// AddRepository creates a repository's built-in repository-view
// privileges (nx-repository-view-<format>-<name>-<action>).
func (s *Server) AddRepository(format, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range repositoryActions {
		p := privilegeName(format, name, a)
		s.privileges[p] = nexus.Privilege{Type: "repository-view", Name: p, ReadOnly: true, Format: format, Repository: name, Actions: []string{strings.ToUpper(a)}}
	}
}

// DeleteRepository removes a repository's privileges and strips them from
// every role, as Nexus does; re-creating the repository does not restore
// them.
func (s *Server) DeleteRepository(format, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range repositoryActions {
		p := privilegeName(format, name, a)
		delete(s.privileges, p)
		for id, r := range s.roles {
			r.Privileges = slices.DeleteFunc(slices.Clone(r.Privileges), func(x string) bool { return x == p })
			s.roles[id] = r
		}
	}
}

func privilegeName(format, repo, action string) string {
	return "nx-repository-view-" + format + "-" + repo + "-" + action
}

// SetUnavailable makes every request fail with 503, as a Nexus that is
// down behind a proxy.
func (s *Server) SetUnavailable(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavailable = v
}

// SetRateLimited makes every authenticated request fail with 429 and
// Retry-After: 30, as Nexus 3.94+ does for a username past its failed-login
// limit, whatever the password.
func (s *Server) SetRateLimited(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rateLimited = v
}

// SetDropOrphanPrivileges selects Nexus 3.91.0+'s role update, which
// stores the role without privileges that do not exist and answers 204,
// instead of 3.76.1's 400.
func (s *Server) SetDropOrphanPrivileges(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropOrphanPrivileges = v
}

// SetPasswordPolicy makes user creation refuse passwords for which
// accept returns false, with Nexus's 400 validation error, as an operator's
// nexus.password.validator does. nil accepts every password.
func (s *Server) SetPasswordPolicy(accept func(password string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.passwordPolicy = accept
}

// SetHook installs fn, which sees every request first and returns true
// when it answered it. nil removes it.
func (s *Server) SetHook(fn func(w http.ResponseWriter, r *http.Request) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = fn
}

// Requests returns the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Users returns the stored users as Nexus reads them, sorted by id.
func (s *Server) Users() []nexus.User {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]nexus.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, s.visible(u))
	}
	slices.SortFunc(out, func(a, b nexus.User) int { return strings.Compare(a.UserID, b.UserID) })
	return out
}

// User returns one stored user as Nexus reads it.
func (s *Server) User(id string) (nexus.User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return nexus.User{}, false
	}
	return s.visible(u), true
}

// Roles returns the stored roles, sorted by id.
func (s *Server) Roles() []nexus.Role {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]nexus.Role, 0, len(s.roles))
	for _, r := range s.roles {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b nexus.Role) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Role returns one stored role.
func (s *Server) Role(id string) (nexus.Role, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.roles[id]
	return r, ok
}

// Authenticate reports whether Nexus would accept the credentials of a
// local user: the user exists, its status is active or changepassword,
// and the password matches.
func (s *Server) Authenticate(userID, password string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	return ok && u.Status.Active() && password != "" && s.passwords[userID] == password
}

// PutUser stores a user as an administrator would create it.
func (s *Server) PutUser(u nexus.User, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.Source == "" {
		u.Source = nexus.DefaultSource
	}
	s.users[u.UserID] = u
	s.passwords[u.UserID] = password
}

// PutRole stores a role as an administrator would create it.
func (s *Server) PutRole(r nexus.Role) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Source = nexus.DefaultSource
	s.roles[r.ID] = r
}

// SetUserStatus changes a user's status, as an administrator disabling or
// locking it in the UI.
func (s *Server) SetUserStatus(id string, status nexus.UserStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[id]; ok {
		u.Status = status
		s.users[id] = u
	}
}

// RemoveUser deletes a user out of band.
func (s *Server) RemoveUser(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.users, id)
	delete(s.passwords, id)
}

// RemoveRole deletes a role out of band; Nexus also removes it from every
// user.
func (s *Server) RemoveRole(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeRole(id)
}

func (s *Server) removeRole(id string) {
	delete(s.roles, id)
	for uid, u := range s.users {
		u.Roles = slices.DeleteFunc(slices.Clone(u.Roles), func(x string) bool { return x == id })
		s.users[uid] = u
	}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	user, pass, hasAuth := r.BasicAuth()
	admin := hasAuth && user == AdminUsername && pass == AdminPassword
	path := strings.TrimPrefix(r.URL.Path, "/service/rest")

	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: path, Query: r.URL.Query(), Admin: admin})
	hook, unavailable, rateLimited := s.hook, s.unavailable, s.rateLimited
	s.mu.Unlock()

	if hook != nil && hook(w, r) {
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/service/rest/") {
		http.NotFound(w, r)
		return
	}
	if unavailable {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	switch path {
	case "/v1/status", "/v1/status/writable":
		w.WriteHeader(http.StatusOK)
		return
	}
	switch {
	case !hasAuth:
		w.WriteHeader(http.StatusForbidden)
		return
	case rateLimited:
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	case !admin:
		w.Header().Set("WWW-Authenticate", `BASIC realm="Sonatype Nexus Repository Manager"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case path == "/v1/security/users" && r.Method == http.MethodGet:
		s.listUsers(w, r)
	case path == "/v1/security/users" && r.Method == http.MethodPost:
		s.createUser(w, body)
	case strings.HasPrefix(path, "/v1/security/users/") && r.Method == http.MethodPut:
		s.updateUser(w, strings.TrimPrefix(path, "/v1/security/users/"), body)
	case strings.HasPrefix(path, "/v1/security/users/") && r.Method == http.MethodDelete:
		s.deleteUser(w, r, strings.TrimPrefix(path, "/v1/security/users/"))
	case path == "/v1/security/roles" && r.Method == http.MethodGet:
		s.listRoles(w)
	case path == "/v1/security/roles" && r.Method == http.MethodPost:
		s.createRole(w, body)
	case strings.HasPrefix(path, "/v1/security/roles/"):
		s.role(w, r, strings.TrimPrefix(path, "/v1/security/roles/"), body)
	case strings.HasPrefix(path, "/v1/security/privileges/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(path, "/v1/security/privileges/")
		p, ok := s.privileges[name]
		if !ok {
			nexusError(w, http.StatusNotFound, fmt.Sprintf("Privilege '%s' not found.", name))
			return
		}
		writeJSON(w, http.StatusOK, p)
	default:
		http.NotFound(w, r)
	}
}

// userBody is ApiCreateUser and the part of ApiUser Nexus reads on update.
type userBody struct {
	UserID       string           `json:"userId"`
	FirstName    string           `json:"firstName"`
	LastName     string           `json:"lastName"`
	EmailAddress string           `json:"emailAddress"`
	Password     string           `json:"password"`
	Source       string           `json:"source"`
	Status       nexus.UserStatus `json:"status"`
	Roles        []string         `json:"roles"`
}

// roleBody is RoleXORequest.
type roleBody struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Privileges  []string `json:"privileges"`
	Roles       []string `json:"roles"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// nexusError writes Nexus's WebApplicationMessageException document.
func nexusError(w http.ResponseWriter, status int, msg string) {
	quoted, _ := json.Marshal(msg)
	writeJSON(w, status, map[string]string{"id": "*", "message": string(quoted)})
}

func validationError(w http.ResponseWriter, id, msg string) {
	w.Header().Set("Content-Type", "application/vnd.siesta-validation-errors-v1+json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode([]map[string]string{{"id": id, "message": msg}})
}

// visible is a stored user as Nexus reads it: role ids without a role are
// hidden, and appear once a role with that id is created.
func (s *Server) visible(u nexus.User) nexus.User {
	u.Roles = slices.DeleteFunc(slices.Clone(u.Roles), func(id string) bool {
		_, ok := s.roles[id]
		return !ok
	})
	return u
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	term := strings.ToLower(r.URL.Query().Get("userId"))
	source := r.URL.Query().Get("source")
	out := []nexus.User{}
	for _, u := range s.users {
		if strings.HasPrefix(strings.ToLower(u.UserID), term) && (source == "" || source == u.Source) {
			out = append(out, s.visible(u))
		}
	}
	slices.SortFunc(out, func(a, b nexus.User) int { return strings.Compare(a.UserID, b.UserID) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createUser(w http.ResponseWriter, body []byte) {
	var in userBody
	if err := json.Unmarshal(body, &in); err != nil {
		validationError(w, "PARAMETER createUser", "invalid JSON")
		return
	}
	switch {
	case in.EmailAddress == "":
		validationError(w, "PARAMETER emailAddress", "must not be empty")
		return
	case len(in.Roles) == 0:
		validationError(w, "PARAMETER roles", "must not be empty")
		return
	case in.Password == "":
		nexusError(w, http.StatusBadRequest, "A non-empty password is required.")
		return
	case s.passwordPolicy != nil && !s.passwordPolicy(in.Password):
		validationError(w, "PARAMETER password", "Password does not match corporate policy")
		return
	case len(in.UserID) > 200:
		w.Header().Set("Content-Type", "text/plain;charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "ERROR: (ID 1) value too long for type character varying(200)")
		return
	}
	if _, dup := s.users[in.UserID]; dup {
		w.Header().Set("Content-Type", "text/plain;charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, "ERROR: (ID 78742119-005e-4ea8-9605-86c4451dea58) org.sonatype.nexus.security.user.DuplicateUserException: User %s already exists.", in.UserID)
		return
	}
	u := nexus.User{
		UserID: in.UserID, FirstName: in.FirstName, LastName: in.LastName, EmailAddress: in.EmailAddress,
		Source: nexus.DefaultSource, Status: in.Status, Roles: slices.Clone(in.Roles), ExternalRoles: []string{},
	}
	s.users[in.UserID] = u
	s.passwords[in.UserID] = in.Password
	// The answer echoes the request, unknown roles included.
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) updateUser(w http.ResponseWriter, id string, body []byte) {
	var in userBody
	if err := json.Unmarshal(body, &in); err != nil {
		validationError(w, "PARAMETER apiUser", "invalid JSON")
		return
	}
	if in.UserID != id {
		nexusError(w, http.StatusBadRequest, "The path's userId does not match the body")
		return
	}
	for _, role := range in.Roles {
		if _, ok := s.roles[role]; !ok {
			validationError(w, "roles", "Unable to locate roleId: "+role)
			return
		}
	}
	old, ok := s.users[id]
	if !ok {
		nexusError(w, http.StatusNotFound, fmt.Sprintf("User '%s' not found.", id))
		return
	}
	old.FirstName, old.LastName, old.EmailAddress, old.Status, old.Roles = in.FirstName, in.LastName, in.EmailAddress, in.Status, slices.Clone(in.Roles)
	s.users[id] = old
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, id string) {
	if r.URL.Query().Get("realm") != nexus.LocalRealm {
		nexusError(w, http.StatusBadRequest, "Invalid or empty realm name.")
		return
	}
	if _, ok := s.users[id]; !ok {
		nexusError(w, http.StatusNotFound, fmt.Sprintf("User '%s' not found.", id))
		return
	}
	delete(s.users, id)
	delete(s.passwords, id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listRoles(w http.ResponseWriter) {
	out := []nexus.Role{}
	for _, r := range s.roles {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b nexus.Role) int { return strings.Compare(a.ID, b.ID) })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) missingPrivilege(in roleBody) (string, bool) {
	for _, p := range in.Privileges {
		if _, ok := s.privileges[p]; !ok {
			return p, true
		}
	}
	return "", false
}

func (s *Server) createRole(w http.ResponseWriter, body []byte) {
	var in roleBody
	if err := json.Unmarshal(body, &in); err != nil {
		validationError(w, "PARAMETER roleXO", "invalid JSON")
		return
	}
	if _, dup := s.roles[in.ID]; dup {
		nexusError(w, http.StatusBadRequest, fmt.Sprintf("Role '%s' already exists, use a unique roleId.", in.ID))
		return
	}
	if p, missing := s.missingPrivilege(in); missing {
		nexusError(w, http.StatusBadRequest, fmt.Sprintf("Privilege '%s' contained in role '%s' not found.", p, in.ID))
		return
	}
	role := nexus.Role{ID: in.ID, Name: in.Name, Description: in.Description, Privileges: slices.Clone(in.Privileges), Roles: slices.Clone(in.Roles), Source: nexus.DefaultSource}
	s.roles[in.ID] = role
	writeJSON(w, http.StatusOK, role)
}

func (s *Server) role(w http.ResponseWriter, r *http.Request, id string, body []byte) {
	role, ok := s.roles[id]
	switch r.Method {
	case http.MethodGet:
		if !ok {
			nexusError(w, http.StatusNotFound, fmt.Sprintf("Role '%s' not found.", id))
			return
		}
		writeJSON(w, http.StatusOK, role)
	case http.MethodPut:
		var in roleBody
		if err := json.Unmarshal(body, &in); err != nil {
			validationError(w, "PARAMETER roleXO", "invalid JSON")
			return
		}
		if in.ID != id {
			nexusError(w, http.StatusConflict, fmt.Sprintf("The Role id '%s' does not match the id used in the path '%s'.", in.ID, id))
			return
		}
		if !ok {
			nexusError(w, http.StatusNotFound, fmt.Sprintf("Role '%s' not found.", id))
			return
		}
		privileges := slices.Clone(in.Privileges)
		if s.dropOrphanPrivileges {
			privileges = slices.DeleteFunc(privileges, func(p string) bool {
				_, ok := s.privileges[p]
				return !ok
			})
		} else if p, missing := s.missingPrivilege(in); missing {
			nexusError(w, http.StatusBadRequest, fmt.Sprintf("Privilege '%s' contained in role '%s' not found.", p, in.ID))
			return
		}
		s.roles[id] = nexus.Role{ID: id, Name: in.Name, Description: in.Description, Privileges: privileges, Roles: slices.Clone(in.Roles), Source: nexus.DefaultSource}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if !ok {
			nexusError(w, http.StatusNotFound, fmt.Sprintf("Role '%s' not found.", id))
			return
		}
		s.removeRole(id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}
