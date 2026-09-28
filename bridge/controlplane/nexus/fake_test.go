// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

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
)

// fakeNexus is a Nexus REST API stand-in that reproduces the behaviour
// verified against sonatype/nexus3:3.76.1 (ADR-0033): the case-insensitive
// user id prefix search, 500 text/plain for a duplicate user, 400 for a
// duplicate role, silently dropped unknown roles on user create, 409 for
// a role update whose body id differs from the path, JSON error documents
// {"id":"*","message":"\"…\""} and validation lists, 401 for wrong and
// 403 for missing credentials, and unauthenticated status endpoints.
type fakeNexus struct {
	t        *testing.T
	username string
	password string

	mu         sync.Mutex
	users      map[string]User
	passwords  map[string]string
	roles      map[string]Role
	privileges map[string]Privilege
	writable   bool
	requests   []recorded

	// override, when set, answers a request before the fake does; it
	// returns false to let the fake handle it.
	override func(w http.ResponseWriter, r *http.Request) bool
}

type recorded struct {
	Method      string
	Path        string // escaped path
	Query       url.Values
	Auth        string
	ContentType string
	Accept      string
	UserAgent   string
	Body        string
}

func newFakeNexus(t *testing.T) *fakeNexus {
	t.Helper()
	return &fakeNexus{
		t:          t,
		username:   "bridge-admin",
		password:   "ADMIN-PASSWORD-123",
		users:      map[string]User{},
		passwords:  map[string]string{},
		roles:      map[string]Role{},
		privileges: map[string]Privilege{},
		writable:   true,
	}
}

// server starts the fake below prefix (a context path such as "/nexus",
// or "").
func (f *fakeNexus) server(prefix string) *httptest.Server {
	f.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recorded{
			Method:      r.Method,
			Path:        r.URL.EscapedPath(),
			Query:       r.URL.Query(),
			Auth:        r.Header.Get("Authorization"),
			ContentType: r.Header.Get("Content-Type"),
			Accept:      r.Header.Get("Accept"),
			UserAgent:   r.Header.Get("User-Agent"),
			Body:        string(body),
		})
		override := f.override
		f.mu.Unlock()
		if override != nil && override(w, r) {
			return
		}
		path, ok := strings.CutPrefix(r.URL.Path, prefix+"/service/rest")
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.route(w, r, path, body)
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

// setOverride replaces the override while the server runs.
func (f *fakeNexus) setOverride(fn func(w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.override = fn
}

func (f *fakeNexus) recordedRequests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeNexus) lastRequest() recorded {
	f.t.Helper()
	reqs := f.recordedRequests()
	if len(reqs) == 0 {
		f.t.Fatal("the fake Nexus saw no request")
	}
	return reqs[len(reqs)-1]
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

func (f *fakeNexus) route(w http.ResponseWriter, r *http.Request, path string, body []byte) {
	switch path {
	case "/v1/status":
		w.WriteHeader(http.StatusOK)
		return
	case "/v1/status/writable":
		f.mu.Lock()
		writable := f.writable
		f.mu.Unlock()
		if writable {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if user != f.username || pass != f.password {
		w.Header().Set("WWW-Authenticate", `BASIC realm="Sonatype Nexus Repository Manager"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case path == "/v1/security/users" && r.Method == http.MethodGet:
		f.listUsers(w, r)
	case path == "/v1/security/users" && r.Method == http.MethodPost:
		f.createUser(w, body)
	case strings.HasPrefix(path, "/v1/security/users/") && r.Method == http.MethodPut:
		f.updateUser(w, strings.TrimPrefix(path, "/v1/security/users/"), body)
	case strings.HasPrefix(path, "/v1/security/users/") && r.Method == http.MethodDelete:
		f.deleteUser(w, r, strings.TrimPrefix(path, "/v1/security/users/"))
	case path == "/v1/security/roles" && r.Method == http.MethodGet:
		f.listRoles(w)
	case path == "/v1/security/roles" && r.Method == http.MethodPost:
		f.createRole(w, body)
	case strings.HasPrefix(path, "/v1/security/roles/"):
		f.role(w, r, strings.TrimPrefix(path, "/v1/security/roles/"), body)
	case strings.HasPrefix(path, "/v1/security/privileges/") && r.Method == http.MethodGet:
		name := strings.TrimPrefix(path, "/v1/security/privileges/")
		p, ok := f.privileges[name]
		if !ok {
			nexusError(w, http.StatusNotFound, fmt.Sprintf("Privilege '%s' not found.", name))
			return
		}
		writeJSON(w, http.StatusOK, p)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeNexus) listUsers(w http.ResponseWriter, r *http.Request) {
	term := strings.ToLower(r.URL.Query().Get("userId"))
	source := r.URL.Query().Get("source")
	out := []User{}
	for _, u := range f.users {
		if strings.HasPrefix(strings.ToLower(u.UserID), term) && (source == "" || source == u.Source) {
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b User) int { return strings.Compare(a.UserID, b.UserID) })
	writeJSON(w, http.StatusOK, out)
}

func (f *fakeNexus) createUser(w http.ResponseWriter, body []byte) {
	var in createUserBody
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
	}
	if _, dup := f.users[in.UserID]; dup {
		w.Header().Set("Content-Type", "text/plain;charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "ERROR: (ID 78742119-005e-4ea8-9605-86c4451dea58) org.sonatype.nexus.security.user.DuplicateUserException: User %s already exists.", in.UserID)
		return
	}
	roles := []string{}
	for _, id := range in.Roles {
		if _, ok := f.roles[id]; ok { // unknown roles are dropped silently
			roles = append(roles, id)
		}
	}
	u := User{
		UserID: in.UserID, FirstName: in.FirstName, LastName: in.LastName, EmailAddress: in.EmailAddress,
		Source: DefaultSource, Status: in.Status, Roles: roles, ExternalRoles: []string{},
	}
	f.users[in.UserID] = u
	f.passwords[in.UserID] = in.Password
	writeJSON(w, http.StatusOK, u)
}

func (f *fakeNexus) updateUser(w http.ResponseWriter, id string, body []byte) {
	var in updateUserBody
	if err := json.Unmarshal(body, &in); err != nil {
		validationError(w, "PARAMETER apiUser", "invalid JSON")
		return
	}
	if in.UserID != id {
		nexusError(w, http.StatusBadRequest, "The path's userId does not match the body")
		return
	}
	for _, role := range in.Roles {
		if _, ok := f.roles[role]; !ok {
			validationError(w, "roles", "Unable to locate roleId: "+role)
			return
		}
	}
	old, ok := f.users[id]
	if !ok {
		nexusError(w, http.StatusNotFound, fmt.Sprintf("User '%s' not found.", id))
		return
	}
	old.FirstName, old.LastName, old.EmailAddress, old.Status, old.Roles = in.FirstName, in.LastName, in.EmailAddress, in.Status, in.Roles
	f.users[id] = old
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeNexus) deleteUser(w http.ResponseWriter, r *http.Request, id string) {
	if r.URL.Query().Get("realm") != LocalRealm {
		nexusError(w, http.StatusBadRequest, "Invalid or empty realm name.")
		return
	}
	if _, ok := f.users[id]; !ok {
		nexusError(w, http.StatusNotFound, fmt.Sprintf("User '%s' not found.", id))
		return
	}
	delete(f.users, id)
	delete(f.passwords, id)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeNexus) listRoles(w http.ResponseWriter) {
	out := []Role{}
	for _, r := range f.roles {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Role) int { return strings.Compare(a.ID, b.ID) })
	writeJSON(w, http.StatusOK, out)
}

func (f *fakeNexus) checkPrivileges(w http.ResponseWriter, in roleBody) bool {
	for _, p := range in.Privileges {
		if _, ok := f.privileges[p]; !ok {
			nexusError(w, http.StatusBadRequest, fmt.Sprintf("Privilege '%s' contained in role '%s' not found.", p, in.ID))
			return false
		}
	}
	return true
}

func (f *fakeNexus) createRole(w http.ResponseWriter, body []byte) {
	var in roleBody
	if err := json.Unmarshal(body, &in); err != nil {
		validationError(w, "PARAMETER roleXO", "invalid JSON")
		return
	}
	if _, dup := f.roles[in.ID]; dup {
		nexusError(w, http.StatusBadRequest, fmt.Sprintf("Role '%s' already exists, use a unique roleId.", in.ID))
		return
	}
	if !f.checkPrivileges(w, in) {
		return
	}
	role := Role{ID: in.ID, Name: in.Name, Description: in.Description, Privileges: in.Privileges, Roles: in.Roles, Source: DefaultSource}
	f.roles[in.ID] = role
	writeJSON(w, http.StatusOK, role)
}

func (f *fakeNexus) role(w http.ResponseWriter, r *http.Request, id string, body []byte) {
	role, ok := f.roles[id]
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
		if !f.checkPrivileges(w, in) {
			return
		}
		f.roles[id] = Role{ID: id, Name: in.Name, Description: in.Description, Privileges: in.Privileges, Roles: in.Roles, Source: DefaultSource}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if !ok {
			nexusError(w, http.StatusNotFound, fmt.Sprintf("Role '%s' not found.", id))
			return
		}
		delete(f.roles, id)
		for uid, u := range f.users {
			u.Roles = slices.DeleteFunc(slices.Clone(u.Roles), func(s string) bool { return s == id })
			f.users[uid] = u
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}
