// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Sentinel errors the main loop reacts to. errBridgeRefused triggers the
// empty-auth response (kubelet retries on next pull, no caching);
// errBridgeUnavailable propagates as a non-zero exit so kubelet retries
// via its own backoff.
var (
	errBridgeRefused     = errors.New("bridge refused credentials")
	errBridgeUnavailable = errors.New("bridge unavailable")
)

// Timeouts are tight because the plugin runs synchronously inside the
// kubelet image-pull path. Failing fast is preferable to blocking a pod's
// startup on a slow bridge.
const (
	connectTimeout    = 5 * time.Second
	requestTimeout    = 15 * time.Second
	retryBackoff      = 1 * time.Second
	maxBodySnippetLen = 256
)

// bridgeResponse mirrors bridge/dataplane.Response. Duplicated rather than
// imported per ADR-0015; see that ADR for the binary-size and
// transitive-dep rationale.
type bridgeResponse struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ExpiresInSecs int    `json:"expires_in"`
	CacheKeyType  string `json:"cache_key_type"`
}

// bridgeClient POSTs to the bridge's credentials endpoint. The HTTP client
// is stored so transport-level test fakes (httptest.NewTLSServer's
// pre-trusted *http.Client) can be injected via newBridgeClientWithHTTPClient.
type bridgeClient struct {
	url  string
	http *http.Client
}

func newBridgeClient(cfg *config) (*bridgeClient, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if cfg.CABundle != "" {
		pem, err := loadCAPEM(cfg.CABundle)
		if err != nil {
			return nil, fmt.Errorf("load CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("HARBOR_BRIDGE_CA_BUNDLE contained no PEM blocks")
		}
		tlsCfg.RootCAs = pool
	}

	if cfg.ClientCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("load mTLS client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	transport := &http.Transport{
		TLSClientConfig:       tlsCfg,
		DialContext:           (&net.Dialer{Timeout: connectTimeout}).DialContext,
		TLSHandshakeTimeout:   connectTimeout,
		ResponseHeaderTimeout: requestTimeout,
		IdleConnTimeout:       30 * time.Second,
	}
	hc := &http.Client{Transport: transport, Timeout: requestTimeout}
	return newBridgeClientWithHTTPClient(cfg.Endpoint, hc), nil
}

// newBridgeClientWithHTTPClient is the seam tests use to point the plugin
// at httptest.NewTLSServer (whose Client() already trusts the test cert).
func newBridgeClientWithHTTPClient(endpoint string, hc *http.Client) *bridgeClient {
	return &bridgeClient{
		url:  strings.TrimRight(endpoint, "/") + credentialsPath,
		http: hc,
	}
}

// fetch implements bridgeFetcher.
func (c *bridgeClient) fetch(image, token string) (*bridgeResponse, error) {
	body, err := json.Marshal(struct {
		Image string `json:"image"`
	}{Image: image})
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	resp, status, err := c.do(body, token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBridgeUnavailable, err)
	}

	// One-time retry on 503 — gives the control plane a beat to finish
	// rotating the robot Secret. Any second-attempt outcome is final.
	if status == http.StatusServiceUnavailable {
		fmt.Fprintln(os.Stderr, "harbor-bridge-plugin: bridge returned 503; retrying once after 1s")
		time.Sleep(retryBackoff)
		resp, status, err = c.do(body, token)
		if err != nil {
			return nil, fmt.Errorf("%w: retry failed: %w", errBridgeUnavailable, err)
		}
	}

	switch status {
	case http.StatusOK:
		var out bridgeResponse
		if err := json.Unmarshal(resp, &out); err != nil {
			return nil, fmt.Errorf("decode bridge response: %w", err)
		}
		if out.Username == "" || out.Password == "" {
			return nil, errors.New("bridge returned 200 with empty username and/or password")
		}
		if out.CacheKeyType == "" {
			return nil, errors.New("bridge returned 200 with empty cache_key_type")
		}
		return &out, nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w: %d %s", errBridgeRefused, status, bodySnippet(resp))
	case http.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: 503 after retry: %s", errBridgeUnavailable, bodySnippet(resp))
	default:
		return nil, fmt.Errorf("bridge returned unexpected status %d: %s", status, bodySnippet(resp))
	}
}

// do performs one POST and returns body + status. The body is fully read
// here so the caller can both retry (which requires not depending on a
// half-read response stream) and format error messages without juggling
// Body.Close() across retry branches.
func (c *bridgeClient) do(body []byte, token string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

// loadCAPEM accepts either an inline PEM blob (begins with the standard
// armor line) or a filesystem path. The dual-mode is documented in
// docs/PHASES.md so chart authors can pick whichever fits their deployment.
func loadCAPEM(s string) ([]byte, error) {
	if strings.HasPrefix(s, "-----BEGIN") {
		return []byte(s), nil
	}
	return os.ReadFile(s)
}

// bodySnippet trims a bridge error body so we surface the cause in
// kubelet's event stream without dumping a multi-line HTML page on stderr.
func bodySnippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > maxBodySnippetLen {
		return s[:maxBodySnippetLen] + "…"
	}
	return s
}
