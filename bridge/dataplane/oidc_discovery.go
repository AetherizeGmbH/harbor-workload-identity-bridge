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

	jose "github.com/go-jose/go-jose/v4"
)

const (
	// InClusterAPIServerURL is the in-cluster apiserver, where the bridge
	// reads its issuer and keys when BRIDGE_OIDC_ISSUER is empty
	// (ADR-0044). kubeadm writes this name into the apiserver's serving
	// certificate; the cluster CA (BRIDGE_OIDC_CA_FILE) verifies it.
	InClusterAPIServerURL = "https://kubernetes.default.svc"

	// discoveryPath and apiServerJWKSPath are where the apiserver serves its
	// discovery document and its keys (ServiceAccountIssuerDiscovery). The
	// keys are at apiServerJWKSPath whatever jwks_uri the document names.
	discoveryPath     = "/.well-known/openid-configuration"
	apiServerJWKSPath = "/openid/v1/jwks"

	// maxDiscoverySize bounds the discovery document. The apiserver's is a
	// few hundred bytes.
	maxDiscoverySize = 64 << 10
)

// IssuerDiscovery is what the bridge takes from the apiserver's discovery
// document (ADR-0044).
type IssuerDiscovery struct {
	// DocumentURL is where the document was read.
	DocumentURL string

	// Issuer is the document's issuer: the iss claim of the tokens the
	// apiserver issues (its first --service-account-issuer).
	Issuer string

	// JWKSURL is the apiserver's own key endpoint. The bridge fetches the
	// keys there unless BRIDGE_OIDC_JWKS_URL names another endpoint.
	JWKSURL string

	// SigningAlgs are the algorithms of the document's
	// id_token_signing_alg_values_supported that the key set can verify,
	// RS256 when it names none of them (the verifier's default, as with
	// discovery at a configured issuer).
	SigningAlgs []string
}

// DiscoverIssuer reads the ServiceAccount token issuer from the discovery
// document of the apiserver at apiServer (InClusterAPIServerURL in the
// bridge, ADR-0044). Unlike go-oidc's NewProvider it accepts an issuer that
// differs from the URL the document came from: on managed clusters the
// issuer is a cloud URL, while the document is read from the in-cluster
// apiserver.
//
// The client should come from NewOIDCHTTPClient. The bridge's token goes
// with the request only when apiServer is the in-cluster apiserver, and
// only over https; redirects are not followed. nil means
// NewOIDCHTTPClient("", ""). The request is bounded by oidcFetchTimeout
// and the document by maxDiscoverySize.
//
// The issuer must be an https URL with a host and without a user:password@
// part, an '@', a query or a fragment, which the apiserver requires of an
// issuer it serves discovery for, and must read back unchanged after
// parsing: the bridge compares tokens with exactly this string.
func DiscoverIssuer(ctx context.Context, client *http.Client, apiServer string) (*IssuerDiscovery, error) {
	base, err := url.Parse(apiServer)
	if err != nil {
		return nil, fmt.Errorf("apiserver URL %q: %w", apiServer, err)
	}
	if err := requireTLSUnlessLoopback(base); err != nil {
		return nil, fmt.Errorf("apiserver URL %q: %w", apiServer, err)
	}
	if client == nil {
		if client, err = NewOIDCHTTPClient("", ""); err != nil {
			return nil, err
		}
	}
	// The bridge's token goes only to the in-cluster apiserver (see
	// NewOIDCHTTPClient).
	if tt, ok := client.Transport.(*tokenTransport); ok && isInClusterAPIServer(base) {
		tt.allow(base.Host)
	}
	root := strings.TrimSuffix(apiServer, "/")
	d := &IssuerDiscovery{DocumentURL: root + discoveryPath, JWKSURL: root + apiServerJWKSPath}

	ctx, cancel := context.WithTimeout(ctx, oidcFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.DocumentURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", d.DocumentURL, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", d.DocumentURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// A redirect ends here too: the client follows none.
		return nil, fmt.Errorf("read %s: %s", d.DocumentURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoverySize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", d.DocumentURL, err)
	}
	if len(body) > maxDiscoverySize {
		return nil, fmt.Errorf("%s is larger than %d bytes", d.DocumentURL, maxDiscoverySize)
	}
	var doc struct {
		Issuer string   `json:"issuer"`
		Algs   []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode %s: %w", d.DocumentURL, err)
	}
	if err := checkDiscoveredIssuer(doc.Issuer); err != nil {
		return nil, fmt.Errorf("%s: %w", d.DocumentURL, err)
	}
	d.Issuer = doc.Issuer
	if d.SigningAlgs = supportedAlgs(doc.Algs); len(d.SigningAlgs) == 0 {
		d.SigningAlgs = []string{string(jose.RS256)}
	}
	return d, nil
}

// checkDiscoveredIssuer applies DiscoverIssuer's rules for the issuer. Its
// errors repeat the issuer only when it has no '@', so that a
// user:password@ part never reaches a log.
func checkDiscoveredIssuer(raw string) error {
	if raw == "" {
		return errors.New("the document names no issuer")
	}
	if strings.Contains(raw, "@") {
		return errors.New("the issuer contains an '@' (a user:password@ part, or one after the host); " +
			"no token's iss claim carries one (the value is left out: it could hold a credential)")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("the issuer is not a URL: %w", err)
	case u.Scheme != "https":
		return fmt.Errorf("the issuer %q is not an https URL", raw)
	case u.Host == "":
		return fmt.Errorf("the issuer %q has no host", raw)
	case strings.ContainsAny(raw, "?#"):
		return fmt.Errorf("the issuer %q has a query or a fragment", raw)
	case u.String() != raw:
		return fmt.Errorf("the issuer %q does not read back unchanged after parsing (%q)", raw, u.String())
	}
	return nil
}
