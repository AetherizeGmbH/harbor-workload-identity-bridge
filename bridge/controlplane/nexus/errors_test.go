// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Every status maps to exactly one sentinel, through errors.Is on the
// *APIError the client returns.
func TestClient_StatusToSentinel(t *testing.T) {
	sentinels := []error{ErrBadRequest, ErrUnauthorized, ErrForbidden, ErrNotFound, ErrConflict, ErrRateLimited, ErrServer, ErrUnexpectedStatus}
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusBadRequest, ErrBadRequest},
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusNotFound, ErrNotFound},
		{http.StatusConflict, ErrConflict},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusInternalServerError, ErrServer},
		{http.StatusBadGateway, ErrServer},
		{http.StatusServiceUnavailable, ErrServer},
		{http.StatusGatewayTimeout, ErrServer},
		{http.StatusTeapot, ErrUnexpectedStatus},
		{http.StatusUnsupportedMediaType, ErrUnexpectedStatus},
		{http.StatusNotModified, ErrUnexpectedStatus},
		{http.StatusMultipleChoices, ErrUnexpectedStatus},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			f := newFakeNexus(t)
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/service/rest/v1/security/roles" {
					// The listing that confirms a 404 is not the subject.
					writeJSON(w, http.StatusOK, []Role{})
					return true
				}
				w.WriteHeader(tc.status)
				return true
			}
			srv := f.server("")
			c := newTestClient(t, f, srv, "")
			_, err := c.GetRole(context.Background(), "bridge-c.ns.sa")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status {
				t.Fatalf("GetRole = %v, want an *APIError with status %d", err, tc.status)
			}
			for _, s := range sentinels {
				if got, want := errors.Is(err, s), errors.Is(s, tc.want); got != want {
					t.Errorf("errors.Is(%v, %v) = %v, want %v", err, s, got, want)
				}
			}
			if !strings.Contains(err.Error(), `get role "bridge-c.ns.sa"`) || !strings.Contains(err.Error(), fmt.Sprint(tc.status)) {
				t.Errorf("error %q does not name the operation and status", err)
			}
		})
	}
}

// Nexus's error bodies come in several shapes (all observed on Nexus
// 3.76.1); each renders as one readable line, and pages without a message
// (Jetty's or a proxy's HTML) render as nothing.
func TestRenderMessage(t *testing.T) {
	cases := []struct {
		name, contentType, body, want string
	}{
		{"exception document", "application/json", `{"id":"*","message":"\"Role 'x' already exists, use a unique roleId.\""}`, "Role 'x' already exists, use a unique roleId."},
		{"validation list", "application/vnd.siesta-validation-errors-v1+json", `[{"id":"PARAMETER emailAddress","message":"must not be empty"},{"id":"roles","message":"Unable to locate roleId: nope"}]`, "PARAMETER emailAddress: must not be empty; roles: Unable to locate roleId: nope"},
		{"JSON string", "application/json", `"Password must be supplied."`, "Password must be supplied."},
		{"unquoted message", "application/json", `{"id":"*","message":"plain"}`, "plain"},
		{"text/plain 500", "text/plain;charset=utf-8", "ERROR: (ID 7874) org.sonatype.nexus.security.user.DuplicateUserException: User u already exists.", "ERROR: (ID 7874) org.sonatype.nexus.security.user.DuplicateUserException: User u already exists."},
		{"no content type, text", "", "upstream connect error", "upstream connect error"},
		{"no content type, HTML", "", "<html>bad gateway</html>", ""},
		{"Jetty HTML", "text/html;charset=iso-8859-1", "<h1>Bad Message 400</h1><pre>reason: Header Folding</pre>", ""},
		{"empty", "application/json", "", ""},
		{"truncated JSON", "application/json", `{"id":"*","message":"cut`, ""},
		{"JSON without message", "application/json", `{"foo":"bar"}`, ""},
		{"control characters", "text/plain", "line one\nline\x00two\ttab", "line oneline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderMessage([]byte(tc.body), tc.contentType, false, nil)
			if tc.name == "control characters" {
				if strings.ContainsAny(got, "\n\x00\t") || !strings.HasPrefix(got, tc.want) {
					t.Fatalf("renderMessage = %q, want control characters dropped", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("renderMessage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderMessage_IsBounded(t *testing.T) {
	got := renderMessage([]byte(strings.Repeat("é", 2000)), "text/plain", false, nil)
	if n := len([]rune(got)); n != maxMessageLen+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("message has %d runes (%q…), want %d plus an ellipsis", n, got[:10], maxMessageLen)
	}
}

// A message that holds a credential or password of the request is
// withheld as a whole, including a password cut off by the body limit and
// one split by control characters.
func TestRenderMessage_WithholdsSecrets(t *testing.T) {
	secret := "S3cr3t-Pa55word"
	cases := []struct {
		name, contentType, body string
		truncated               bool
	}{
		{"verbatim", "text/plain", "invalid password " + secret, false},
		{"inside JSON", "application/json", `{"id":"*","message":"\"bad password ` + secret + `\""}`, false},
		{"JSON escaped", "application/json", `"bad password S3cr3t\u002dPa55word"`, false},
		{"split by control character", "text/plain", "bad password S3cr3t\x00-Pa55word", false},
		{"cut by the body limit", "text/plain", "long message ending in S3cr3t-Pa", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderMessage([]byte(tc.body), tc.contentType, tc.truncated, []string{secret}); got != messageWithheld {
				t.Fatalf("renderMessage = %q, want it withheld", got)
			}
		})
	}
	// A message without the secret is kept.
	if got := renderMessage([]byte("Role 'x' not found."), "text/plain", true, []string{secret}); got != "Role 'x' not found." {
		t.Errorf("renderMessage of an innocent truncated message = %q", got)
	}
}

// A server that echoes the request (Authorization header and body) in its
// error must not get the admin credentials or the new user's password into
// the error.
func TestClient_ErrorsNeverCarryCredentials(t *testing.T) {
	const userPassword = "USER-PASSWORD-456"
	for _, contentType := range []string{"text/plain", "application/json"} {
		t.Run(contentType, func(t *testing.T) {
			f := newFakeNexus(t)
			f.override = func(w http.ResponseWriter, r *http.Request) bool {
				echo := fmt.Sprintf("rejected %s with %s (password %s)", r.Header.Get("Authorization"), f.password, userPassword)
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(http.StatusBadRequest)
				if contentType == "application/json" {
					fmt.Fprintf(w, `[{"id":"x","message":%q}]`, echo)
				} else {
					fmt.Fprint(w, echo)
				}
				return true
			}
			srv := f.server("")
			c := newTestClient(t, f, srv, "")
			_, err := c.CreateUser(context.Background(), User{
				UserID: "bridge-c.ns.sa_0123456789abcdef", FirstName: "f", LastName: "l",
				EmailAddress: "e@x.invalid", Status: UserActive, Roles: []string{"bridge-c.ns.sa"},
			}, userPassword)
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("CreateUser = %v, want ErrBadRequest", err)
			}
			for _, secret := range []string{f.password, userPassword, basicAuth(f.username, f.password)} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error discloses %q: %v", secret, err)
				}
			}
			if !strings.Contains(err.Error(), messageWithheld) {
				t.Errorf("error %q does not say the message was withheld", err)
			}
		})
	}
}

// For every request without a user password the admin credentials are the
// only secrets: each of them, echoed alone, must be withheld.
func TestClient_ErrorsNeverCarryAdminCredentials(t *testing.T) {
	calls := map[string]func(Client) error{
		"UpdateUser": func(c Client) error {
			u := testUser()
			u.Source = DefaultSource
			return c.UpdateUser(context.Background(), u)
		},
		"ListUsers":  func(c Client) error { _, err := c.ListUsers(context.Background(), "bridge-c."); return err },
		"DeleteRole": func(c Client) error { return c.DeleteRole(context.Background(), testRoleID) },
	}
	echoes := map[string]func(f *fakeNexus, r *http.Request) string{
		"Authorization header": func(_ *fakeNexus, r *http.Request) string {
			return "rejected " + r.Header.Get("Authorization")
		},
		"admin password": func(f *fakeNexus, _ *http.Request) string { return "rejected password " + f.password },
	}
	for callName, call := range calls {
		for echoName, echo := range echoes {
			t.Run(callName+"/"+echoName, func(t *testing.T) {
				f := newFakeNexus(t)
				f.override = func(w http.ResponseWriter, r *http.Request) bool {
					w.Header().Set("Content-Type", "text/plain")
					w.WriteHeader(http.StatusBadRequest)
					fmt.Fprint(w, echo(f, r))
					return true
				}
				srv := f.server("")
				c := newTestClient(t, f, srv, "")
				err := call(c)
				if !errors.Is(err, ErrBadRequest) {
					t.Fatalf("%s = %v, want ErrBadRequest", callName, err)
				}
				for _, secret := range []string{f.password, basicAuth(f.username, f.password)} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error discloses %q: %v", secret, err)
					}
				}
				if !strings.Contains(err.Error(), messageWithheld) {
					t.Errorf("error %q does not say the message was withheld", err)
				}
			})
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in     string
		want   time.Duration
		wantOK bool
	}{
		{"30", 30 * time.Second, true},
		{" 900 ", 900 * time.Second, true},
		{"0", 0, true},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0, true},
		{"", 0, false},
		{"-5", 0, false},
		{"1.5", 0, false},
		{"soon", 0, false},
		{"99999999999999999999", 0, false},
		{"9223372037", 0, false}, // seconds that overflow time.Duration
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.in, now)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("parseRetryAfter(%q) = %s, %v; want %s, %v", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// Nexus 3.93+ answers repeated failed logins with 429 and Retry-After.
// The client never retries; it reports the header, which for Nexus 3.94+
// is not when the block ends (ErrRateLimited).
func TestClient_RateLimitedCarriesRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, header string
		want         time.Duration
		wantOK       bool
	}{
		{"seconds", "30", 30 * time.Second, true},
		{"date", now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute, true},
		{"missing", "", 0, false},
		{"malformed", "later", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeNexus(t)
			calls := 0
			f.override = func(w http.ResponseWriter, _ *http.Request) bool {
				calls++
				if tc.header != "" {
					w.Header().Set("Retry-After", tc.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				return true
			}
			srv := f.server("")
			c := newTestClient(t, f, srv, "", withClock(func() time.Time { return now }))
			_, err := c.ListUsers(context.Background(), "bridge-c.")
			if !errors.Is(err, ErrRateLimited) {
				t.Fatalf("ListUsers = %v, want ErrRateLimited", err)
			}
			d, ok := RetryAfter(err)
			if d != tc.want || ok != tc.wantOK {
				t.Errorf("RetryAfter = %s, %v; want %s, %v", d, ok, tc.want, tc.wantOK)
			}
			if calls != 1 {
				t.Errorf("the client sent %d requests, want exactly 1 (no retries)", calls)
			}
			if tc.wantOK && !strings.Contains(err.Error(), "Retry-After "+tc.want.String()) {
				t.Errorf("error %q does not report the delay", err)
			}
		})
	}
	if _, ok := RetryAfter(errors.New("other")); ok {
		t.Error("RetryAfter of a foreign error reported a delay")
	}
}

// An error body longer than the limit is cut; a password straddling the
// cut must not survive as a fragment.
func TestClient_ErrorBodyCutInsideASecret(t *testing.T) {
	const userPassword = "USER-PASSWORD-456"
	f := newFakeNexus(t)
	f.override = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, strings.Repeat("x", maxErrorBody-5)+userPassword+strings.Repeat("y", 100))
		return true
	}
	srv := f.server("")
	c := newTestClient(t, f, srv, "")
	_, err := c.CreateUser(context.Background(), testUser(), userPassword)
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("CreateUser = %v, want ErrBadRequest", err)
	}
	if strings.Contains(err.Error(), userPassword[:5]) || !strings.Contains(err.Error(), messageWithheld) {
		t.Errorf("error = %.200q…, want the message withheld", err)
	}
}
