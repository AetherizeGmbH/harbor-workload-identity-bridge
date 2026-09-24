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
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	// jwksMinRefresh is the shortest interval between two JWKS fetches.
	// go-oidc's RemoteKeySet fetches again for every token whose key ID
	// it does not know, so every forged token sent to the NodePort cost
	// one authenticated request to the apiserver. A real key rotation is
	// picked up at most this late.
	jwksMinRefresh = 30 * time.Second

	// jwksMaxAge is how long a fetched key set is used before the next
	// verification fetches it again, even for a known key ID.
	jwksMaxAge = 10 * time.Minute

	// maxJWKSSize bounds the JWKS response.
	maxJWKSSize = 1 << 20
)

// jwtSigningAlgs are the algorithms the key set parses. The verifier
// enforces the narrower configured list before it calls the key set.
var jwtSigningAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.EdDSA,
}

// cachedKeySet implements oidc.KeySet with a rate-limited refresh.
type cachedKeySet struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	keys      []jose.JSONWebKey
	fetchedAt time.Time // zero until the first successful fetch
	triedAt   time.Time // last fetch attempt, successful or not
}

func newCachedKeySet(url string, client *http.Client) *cachedKeySet {
	return &cachedKeySet{url: url, client: client, now: time.Now}
}

// VerifySignature implements oidc.KeySet.
func (k *cachedKeySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	jws, err := jose.ParseSigned(raw, jwtSigningAlgs)
	if err != nil {
		return nil, fmt.Errorf("malformed jwt: %w", err)
	}
	if len(jws.Signatures) != 1 {
		return nil, errors.New("jwt must carry exactly one signature")
	}
	keyID := jws.Signatures[0].Header.KeyID

	keys, err := k.keysFor(ctx, false)
	if err != nil {
		return nil, err
	}
	if payload, ok := verifyWith(jws, keys, keyID); ok {
		return payload, nil
	}
	// Unknown key: fetch again, unless the last attempt was too recent.
	keys, err = k.keysFor(ctx, true)
	if err != nil {
		return nil, err
	}
	if payload, ok := verifyWith(jws, keys, keyID); ok {
		return payload, nil
	}
	return nil, errors.New("failed to verify token signature")
}

func verifyWith(jws *jose.JSONWebSignature, keys []jose.JSONWebKey, keyID string) ([]byte, bool) {
	for i := range keys {
		if keyID != "" && keys[i].KeyID != keyID {
			continue
		}
		if payload, err := jws.Verify(&keys[i]); err == nil {
			return payload, true
		}
	}
	return nil, false
}

// keysFor returns the cached keys, fetching when there are none, when they
// are older than jwksMaxAge, or when refresh is set; any fetch waits at
// least jwksMinRefresh after the previous attempt. The lock is held across
// the fetch, so concurrent callers share one request.
func (k *cachedKeySet) keysFor(ctx context.Context, refresh bool) ([]jose.JSONWebKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	stale := k.fetchedAt.IsZero() || now.Sub(k.fetchedAt) > jwksMaxAge
	if !refresh && !stale {
		return k.keys, nil
	}
	if !k.triedAt.IsZero() && now.Sub(k.triedAt) < jwksMinRefresh {
		if k.fetchedAt.IsZero() {
			return nil, errors.New("no token signing keys available yet (JWKS fetch failed recently)")
		}
		return k.keys, nil
	}
	k.triedAt = now
	// A caller that gives up must not fail the shared fetch for everyone
	// else; the client's own timeout still bounds it.
	keys, err := k.fetch(context.WithoutCancel(ctx))
	if err != nil {
		if k.fetchedAt.IsZero() {
			return nil, err
		}
		// Keep serving the last good keys; the next attempt comes after
		// jwksMinRefresh.
		return k.keys, nil
	}
	k.keys, k.fetchedAt = keys, now
	return keys, nil
}

func (k *cachedKeySet) fetch(ctx context.Context) ([]jose.JSONWebKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, fmt.Errorf("build JWKS request: %w", err)
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch JWKS: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSSize+1))
	if err != nil {
		return nil, fmt.Errorf("read JWKS: %w", err)
	}
	if len(body) > maxJWKSSize {
		return nil, fmt.Errorf("JWKS larger than %d bytes", maxJWKSSize)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	return set.Keys, nil
}
