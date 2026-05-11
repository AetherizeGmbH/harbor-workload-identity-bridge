// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Default timeout for /service/token calls. Harbor responses are normally
// sub-second; 10s is a generous ceiling that lets us fail before the
// kubelet plugin's own 15s total budget runs out.
const defaultTokenClientTimeout = 10 * time.Second

// DockerToken is the response from Harbor's /service/token endpoint —
// a Docker Registry v2 bearer JWT plus TTL information used by the cache.
type DockerToken struct {
	// Token is the JWT to return to the kubelet plugin and ultimately
	// containerd.
	Token string

	// Issued is the issued_at timestamp from Harbor when present;
	// time.Now() at receipt otherwise.
	Issued time.Time

	// ExpiresIn is the token lifetime. Used as the cache TTL.
	ExpiresIn time.Duration
}

// Scope is one entry in the Docker token "scope" query parameter:
//
//	repository:<resource>:<actions>
//
// Resource is "<project>/<image>" or just "<project>" for project-scoped
// access. Actions are e.g. ["pull"] or ["pull","push"].
type Scope struct {
	Type     string
	Resource string
	Actions  []string
}

// Encode returns the wire form of a single scope per the Docker Registry
// v2 token spec.
func (s Scope) Encode() string {
	return fmt.Sprintf("%s:%s:%s", s.Type, s.Resource, strings.Join(s.Actions, ","))
}

// HarborTokenClient mints a Docker bearer JWT from Harbor's /service/token
// endpoint, authenticating with the per-CR robot password. See
// docs/adr/0005-docker-token-via-service-token.md for why we mint short-
// lived JWTs rather than handing the robot password to the kubelet.
type HarborTokenClient interface {
	// Mint requests a docker bearer for the given service and scopes,
	// authenticating with the supplied robot credentials. service is the
	// registry hostname Harbor expects in the docker token spec — the
	// canonical Harbor value is "harbor-registry", but operators may
	// override via the chart for non-default deployments.
	Mint(ctx context.Context, robotUsername, robotPassword, service string, scopes []Scope) (*DockerToken, error)
}

// ErrTokenAuth signals a 401 from /service/token. The reconciler owns the
// robot password; if /service/token says 401, the password Secret in the
// bridge namespace is stale relative to Harbor (probably because the
// reconciler rotated since we last loaded the Secret). ADR-0007 calls
// this out: the data plane treats 401 as the signal to re-read the
// Secret and retry once.
var ErrTokenAuth = errors.New("harbor /service/token returned 401")

// NewHarborTokenClient builds a token client targeting harborURL.
// transport is optional; pass non-nil to plug in httptest, custom TLS,
// or an instrumented round-tripper.
func NewHarborTokenClient(harborURL *url.URL, transport http.RoundTripper) (HarborTokenClient, error) {
	if harborURL == nil {
		return nil, errors.New("harbor token client: nil URL")
	}
	c := &http.Client{Timeout: defaultTokenClientTimeout}
	if transport != nil {
		c.Transport = transport
	}
	return &httpTokenClient{baseURL: harborURL, client: c}, nil
}

type httpTokenClient struct {
	baseURL *url.URL
	client  *http.Client
}

func (c *httpTokenClient) Mint(ctx context.Context, user, pass, service string, scopes []Scope) (*DockerToken, error) {
	switch {
	case user == "" || pass == "":
		return nil, errors.New("harbor token client: empty robot credentials")
	case service == "":
		return nil, errors.New("harbor token client: empty service")
	}

	u := *c.baseURL
	// /service/token lives at the registry-token endpoint, not under the
	// /api/v2.0 root. Strip the API suffix if the caller passed a URL
	// already augmented for the v2 API client (harbor/client.go does this).
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api/v2.0")
	u.Path += "/service/token"

	q := url.Values{}
	q.Set("service", service)
	for _, s := range scopes {
		q.Add("scope", s.Encode())
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.SetBasicAuth(user, pass)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call /service/token: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through
	case http.StatusUnauthorized:
		return nil, ErrTokenAuth
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("/service/token returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Docker Registry v2 token response. Harbor uses "token"; some
	// registries (and OAuth2-style implementations) use "access_token".
	// Accept either so the bridge does not break on registry-version skew.
	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		IssuedAt    string `json:"issued_at"` // RFC3339; optional
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode /service/token response: %w", err)
	}

	token := payload.Token
	if token == "" {
		token = payload.AccessToken
	}
	if token == "" {
		return nil, errors.New("/service/token returned empty token")
	}

	issued := time.Now()
	if payload.IssuedAt != "" {
		if t, err := time.Parse(time.RFC3339, payload.IssuedAt); err == nil {
			issued = t
		}
	}

	expiresIn := time.Duration(payload.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		// Harbor's default token TTL is documented as 30m. If the server
		// omits expires_in (older Harbor versions sometimes do) we use
		// that default so the cache does not insert zero-TTL entries
		// it would evict immediately on read.
		expiresIn = 30 * time.Minute
	}

	return &DockerToken{
		Token:     token,
		Issued:    issued,
		ExpiresIn: expiresIn,
	}, nil
}
