// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Package nexus is a hand-written client for the part of the Sonatype
// Nexus Repository REST API (/service/rest) the bridge needs to give each
// workload identity one Nexus role and one local user (ADR-0033): local
// users, roles, the repository-view privileges Nexus creates with every
// repository, and the status endpoints. It also holds the injective
// identity-to-name mapping and the password generator.
//
// It follows the Harbor client's hardening (bridge/controlplane/harbor):
// every call has a deadline, redirects are refused, https is required
// unless explicitly allowed, response bodies are read up to a limit, and
// no credential or password reaches a log line or an error. There is no
// SDK: about a dozen endpoints do not justify a generated client, and
// hand-written requests leave no debug dump to switch off.
//
// The package is not wired into the bridge yet; ADR-0033 describes the
// control plane that will use it.
package nexus

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// apiBasePath is the root of Nexus's REST API below the server's
	// base URL (which may carry a context path, e.g. /nexus).
	apiBasePath = "/service/rest"

	// DefaultCallTimeout bounds one Nexus API call. The reconcile and
	// janitor contexts carry no deadline; without it one Nexus that
	// accepts the connection and never answers would block the reconcile
	// worker, and with it rotation, revocation and deletion for every
	// NexusAccess.
	DefaultCallTimeout = 30 * time.Second

	// DefaultMaxResponseBytes bounds the body of a successful response the
	// client decodes. The largest is a role listing, which Nexus cannot
	// filter (GET /v1/security/roles returns every role of the source).
	DefaultMaxResponseBytes int64 = 16 << 20

	// DefaultSource is the user and role source of Nexus's own local
	// users and roles. The client reads and writes only this source.
	DefaultSource = "default"

	// LocalRealm is the realm DELETE /v1/security/users/{id} must name to
	// address a local user. Without it Nexus resolves the id across every
	// user source: an LDAP user of the same id makes the request fail with
	// 400, a SAML or OAuth2 user of the same id may be deleted instead
	// (UserApiResource.deleteUser, nexus-public release-3.96.3).
	LocalRealm = "NexusAuthenticatingRealm"

	// drainLimit bounds how much of an unread body the client discards to
	// keep the connection reusable.
	drainLimit = 64 << 10

	userAgent = "harbor-workload-identity-bridge"
)

// idPattern restricts the ids the client puts into a URL path (users,
// roles, privileges) and the prefixes of listings. Every name the bridge
// builds (IdentityName, UserID, RepositoryPrivileges) matches it; so do
// Nexus's repository and privilege names. It rules out '/', "..", '%',
// ';' and every other character a proxy or Jetty could read as a path
// separator, a parameter or an escape.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// maxIDLen is the width of Nexus's user and role id columns.
const maxIDLen = 200

func validateID(what, id string) error {
	if len(id) == 0 || len(id) > maxIDLen || !idPattern.MatchString(id) {
		return fmt.Errorf("%s %q is not an id this client sends to Nexus (letters, digits, '.', '_', '-'; first a letter or digit; at most %d characters)", what, id, maxIDLen)
	}
	return nil
}

// CredentialSource returns the Basic Auth username and password for one
// Nexus API request. Its errors must not carry the credentials themselves.
type CredentialSource func() (username, password string, err error)

// Client is the surface the control plane needs against Nexus. Like
// harbor.Client it is cluster-agnostic: the ownership rules (OwnsName and
// the markers of ADR-0033) are the caller's, enforced before every write.
type Client interface {
	// GetUser returns the local user with exactly this id. Nexus can only
	// search by a case-insensitive id prefix, so the client lists and
	// matches the id case-sensitively. ErrNotFound means the listing
	// succeeded without a match; a 404 from the listing endpoint itself
	// (a URL that does not point at Nexus) is ErrUnexpectedStatus.
	GetUser(ctx context.Context, userID string) (*User, error)

	// ListUsers returns every local user whose id begins with prefix,
	// compared case-sensitively.
	ListUsers(ctx context.Context, prefix string) ([]User, error)

	// CreateUser creates a local user with the given password and returns
	// Nexus's view of it. Nexus drops roles that do not exist instead of
	// refusing them, so the caller checks the returned roles.
	CreateUser(ctx context.Context, user User, password string) (*User, error)

	// UpdateUser rewrites a local user's names, e-mail address, status and
	// roles. user must come from GetUser or ListUsers with the fields to
	// change edited: its Status is sent as it is, so an administrator's
	// disable is not undone. The password is left unchanged.
	UpdateUser(ctx context.Context, user User) error

	// DeleteUser deletes the local user. A user that does not exist is no
	// error once the listing confirms its absence.
	DeleteUser(ctx context.Context, userID string) error

	// GetRole returns the local role; ErrNotFound when there is none.
	GetRole(ctx context.Context, id string) (*Role, error)

	// ListRoles returns every local role whose id begins with prefix,
	// compared case-sensitively.
	ListRoles(ctx context.Context, prefix string) ([]Role, error)

	// CreateRole creates a local role and returns Nexus's view of it.
	CreateRole(ctx context.Context, role Role) (*Role, error)

	// UpdateRole rewrites a local role's name, description, privileges
	// and contained roles.
	UpdateRole(ctx context.Context, role Role) error

	// DeleteRole deletes the local role; Nexus also removes it from every
	// user. A role that does not exist is no error once the listing
	// confirms its absence.
	DeleteRole(ctx context.Context, id string) error

	// GetPrivilege returns the privilege; ErrNotFound when there is none.
	// For a built-in repository-view privilege (RepositoryPrivileges) that
	// means the repository does not exist in that format.
	GetPrivilege(ctx context.Context, name string) (*Privilege, error)

	// Status reports whether Nexus can serve read requests
	// (GET /v1/status). It sends no credentials.
	Status(ctx context.Context) error

	// Writable reports whether Nexus can serve write requests
	// (GET /v1/status/writable): true on 200, false on 503. It sends no
	// credentials.
	Writable(ctx context.Context) (bool, error)
}

// Option configures NewClient.
type Option func(*httpClient)

// WithCallTimeout overrides DefaultCallTimeout.
func WithCallTimeout(d time.Duration) Option {
	return func(c *httpClient) { c.callTimeout = d }
}

// WithCredentialSource makes the client ask source for the credentials on
// every request, in place of the username and password passed to
// NewClient, so rotated credentials take effect without a new client. A
// request fails before it is sent when source returns an error.
func WithCredentialSource(source CredentialSource) Option {
	return func(c *httpClient) { c.credentials = source }
}

// WithAllowInsecureHTTP permits an http:// Nexus URL. The admin
// credentials and every new user's password then travel unencrypted; only
// for a Nexus reached over a test cluster's pod network.
func WithAllowInsecureHTTP() Option {
	return func(c *httpClient) { c.allowHTTP = true }
}

// WithMaxResponseBytes overrides DefaultMaxResponseBytes.
func WithMaxResponseBytes(n int64) Option {
	return func(c *httpClient) { c.maxResponseBytes = n }
}

// withClock replaces time.Now (tests of Retry-After dates).
func withClock(now func() time.Time) Option {
	return func(c *httpClient) { c.now = now }
}

type httpClient struct {
	base             *url.URL // scheme, host and path up to /service/rest
	http             *http.Client
	credentials      CredentialSource
	callTimeout      time.Duration
	maxResponseBytes int64
	allowHTTP        bool
	now              func() time.Time
}

// defaultTransport is http.DefaultTransport with the timeouts it lacks (a
// response that never starts would otherwise hold the call until the
// per-call deadline) and TLS 1.2 as the floor.
func defaultTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	t := base.Clone()
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = DefaultCallTimeout
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return t
}

// NewTransport returns the default transport. With caPEM set it trusts
// only the certificates in that PEM bundle (a Nexus behind a private CA);
// the caller reads the bundle from the operator's configured file.
func NewTransport(caPEM []byte) (http.RoundTripper, error) {
	t := defaultTransport()
	if len(caPEM) == 0 {
		return t, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("nexus CA bundle contains no PEM certificates")
	}
	t.TLSClientConfig.RootCAs = pool
	return t, nil
}

// NewClient builds a Client for the Nexus at nexusURL (scheme, host and
// any context path; "/service/rest" is appended unless present) using
// HTTP Basic Auth with username and password, or with the credentials
// WithCredentialSource supplies. transport is optional; nil selects the
// default transport (see NewTransport for a private CA).
//
// The URL must use https unless WithAllowInsecureHTTP is given, and must
// carry no user:password@ part, query or fragment.
//
// The client follows no redirects. Every request carries the Nexus admin
// credentials, and net/http re-sends the Authorization header (and on
// 307/308 the body, which may hold a new user's password) to a redirect
// target on the same host or a subdomain of it whatever its scheme. A
// redirect fails the call with an error naming the target instead.
func NewClient(nexusURL *url.URL, username, password string, transport http.RoundTripper, opts ...Option) (Client, error) {
	c := &httpClient{
		callTimeout:      DefaultCallTimeout,
		maxResponseBytes: DefaultMaxResponseBytes,
		now:              time.Now,
	}
	for _, o := range opts {
		o(c)
	}
	if nexusURL == nil {
		return nil, errors.New("nexus URL is nil")
	}
	switch {
	case nexusURL.User != nil:
		return nil, errors.New("nexus URL must not contain credentials (user:password@): the client authenticates with the configured admin credentials")
	case nexusURL.Scheme == "http" && !c.allowHTTP:
		return nil, fmt.Errorf("nexus URL %q uses plain http: the admin credentials and user passwords would travel unencrypted; use https or allow http explicitly", nexusURL.Redacted())
	case nexusURL.Scheme != "https" && nexusURL.Scheme != "http":
		return nil, fmt.Errorf("nexus URL %q must use https", nexusURL.Redacted())
	case nexusURL.Host == "":
		return nil, errors.New("nexus URL must include a host")
	case nexusURL.RawQuery != "" || nexusURL.ForceQuery || nexusURL.Fragment != "":
		return nil, fmt.Errorf("nexus URL %q must not carry a query or fragment", nexusURL.Redacted())
	case c.callTimeout <= 0:
		return nil, errors.New("nexus call timeout must be positive")
	case c.maxResponseBytes <= 0:
		return nil, errors.New("nexus response limit must be positive")
	}
	if c.credentials == nil {
		if username == "" {
			return nil, errors.New("nexus credentials are required: a username or a credential source")
		}
		// A closure, so no field of the client holds the password where a
		// %+v of the client would print it.
		c.credentials = func() (string, string, error) { return username, password, nil }
	}

	base := &url.URL{Scheme: nexusURL.Scheme, Host: nexusURL.Host}
	p := strings.TrimRight(nexusURL.EscapedPath(), "/")
	if !strings.HasSuffix(p, apiBasePath) {
		p += apiBasePath
	}
	if err := setEscapedPath(base, p); err != nil {
		return nil, fmt.Errorf("nexus URL path: %w", err)
	}
	c.base = base

	if transport == nil {
		transport = defaultTransport()
	}
	c.http = &http.Client{Transport: transport, CheckRedirect: refuseRedirect}
	return c, nil
}

// refuseRedirect is the client's http.Client.CheckRedirect: Nexus's REST
// endpoints do not redirect, so a redirect means a proxy or a URL that
// points somewhere other than the API. net/http closes the redirect
// response and returns this error from the call.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("refusing to follow a redirect to %s: the Nexus client follows no redirects "+
		"(it would re-send the Nexus admin credentials); set the Nexus URL to the https scheme and host, "+
		"plus any context path in front of %s, at which Nexus's API answers without a redirect",
		req.URL.Redacted(), apiBasePath)
}

func setEscapedPath(u *url.URL, escaped string) error {
	p, err := url.PathUnescape(escaped)
	if err != nil {
		return err
	}
	u.Path, u.RawPath = p, escaped
	return nil
}

// request describes one API call.
type request struct {
	op          string
	method      string
	path        string // escaped, below /service/rest
	query       url.Values
	body        []byte
	contentType string
	noAuth      bool
	secrets     []string // passwords in the body, never rendered in errors
	out         any      // decoded from a successful JSON body; nil discards it
}

// do performs the request. It returns nil for a 2xx status (after
// decoding the body into out) and an *APIError for any other status.
func (c *httpClient) do(ctx context.Context, r request) error {
	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()

	u := *c.base
	if err := setEscapedPath(&u, c.base.EscapedPath()+r.path); err != nil {
		return fmt.Errorf("%s: %w", r.op, err)
	}
	u.RawQuery = r.query.Encode()

	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), body)
	if err != nil {
		return fmt.Errorf("%s: %w", r.op, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if r.body != nil {
		req.Header.Set("Content-Type", r.contentType)
	}
	secrets := r.secrets
	if !r.noAuth {
		username, password, err := c.credentials()
		if err != nil {
			return fmt.Errorf("%s: nexus credentials: %w", r.op, err)
		}
		req.SetBasicAuth(username, password)
		secrets = append(secrets[:len(secrets):len(secrets)], password,
			base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		if resp != nil {
			// A refused redirect returns the redirect response with its
			// body already closed.
			_ = resp.Body.Close()
		}
		// *url.Error names the method and URL, which hold no secrets:
		// NewClient refuses user:password@ and ids are validated.
		return fmt.Errorf("%s: %w", r.op, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		head, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
		// A body cut by the limit or by a read error may end in the start
		// of a secret; renderMessage then checks the tail.
		truncated := len(head) > maxErrorBody || readErr != nil
		if len(head) > maxErrorBody {
			head = head[:maxErrorBody]
		}
		apiErr := &APIError{
			Op:         r.op,
			StatusCode: resp.StatusCode,
			Message:    renderMessage(head, resp.Header.Get("Content-Type"), truncated, secrets),
		}
		if v := resp.Header.Get("Retry-After"); v != "" {
			apiErr.RetryAfter, apiErr.HasRetryAfter = parseRetryAfter(v, c.now())
		}
		return apiErr
	}
	if r.out == nil {
		return nil
	}
	if ct := resp.Header.Get("Content-Type"); !isJSON(ct) {
		// An HTML page with 200 is a login portal or the Nexus UI: the
		// URL does not point at the REST API.
		return fmt.Errorf("%s: Nexus answered %d with Content-Type %q, not JSON; check that the Nexus URL points at the server's base URL", r.op, resp.StatusCode, ct)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%s: read response: %w", r.op, err)
	}
	if int64(len(data)) > c.maxResponseBytes {
		return fmt.Errorf("%s: response larger than %d bytes; refusing to decode it", r.op, c.maxResponseBytes)
	}
	if err := json.Unmarshal(data, r.out); err != nil {
		return fmt.Errorf("%s: decode response: %w", r.op, err)
	}
	return nil
}

// jsonBody encodes v for a request body.
func jsonBody(v any) ([]byte, error) {
	return json.Marshal(v)
}

// pathSegment escapes one validated id for a URL path.
func pathSegment(id string) string {
	return url.PathEscape(id)
}

// Status implements Client.
func (c *httpClient) Status(ctx context.Context) error {
	return c.do(ctx, request{op: "read Nexus status", method: http.MethodGet, path: "/v1/status", noAuth: true})
}

// Writable implements Client.
func (c *httpClient) Writable(ctx context.Context) (bool, error) {
	err := c.do(ctx, request{op: "read Nexus writable status", method: http.MethodGet, path: "/v1/status/writable", noAuth: true})
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusServiceUnavailable {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
