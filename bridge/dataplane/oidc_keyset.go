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
	// jwksMinRefresh is the shortest interval between the end of one JWKS
	// fetch and the start of the next. go-oidc's RemoteKeySet fetches
	// again for every token whose key ID it does not know, so every forged
	// token sent to the NodePort cost one authenticated request to the
	// apiserver. A real key rotation is picked up at most this late.
	// Measured from the end of a fetch: a fetch that hangs until its
	// timeout must not be followed by the next one at once.
	jwksMinRefresh = 30 * time.Second

	// jwksMaxAge is how long a fetched key set is used before a
	// verification starts a refresh, even for a known key ID.
	jwksMaxAge = 10 * time.Minute

	// jwksFetchTimeout bounds one JWKS fetch. Only a caller whose token
	// names a key the bridge does not hold waits for a fetch, and it has
	// to be answered within the plugin's 15s request timeout.
	jwksFetchTimeout = 10 * time.Second

	// maxJWKSSize bounds the JWKS response.
	maxJWKSSize = 1 << 20
)

// jwtSigningAlgs are the asymmetric algorithms the key set parses. The
// verifier enforces its configured list before it calls the key set: the
// algorithms discovery advertises, or all of these with a JWKS URL.
var jwtSigningAlgs = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.EdDSA,
}

// cachedKeySet implements oidc.KeySet with a rate-limited refresh that
// never makes a caller with a known key wait.
//
// At most one fetch runs at a time, outside the mutex. A token signed by
// a cached key verifies at once, also while a fetch runs, after one
// failed, or when the keys are older than jwksMaxAge (that starts a
// background refresh instead). Only a token whose key is not cached waits
// for a fetch, and only as long as its own request context allows; the
// fetch itself is detached from every request. After a failed fetch the
// last good keys stay in use: a key the issuer rotated out is still
// accepted until a fetch succeeds.
type cachedKeySet struct {
	url          string
	client       *http.Client
	now          func() time.Time
	fetchTimeout time.Duration

	mu        sync.Mutex
	keys      []jose.JSONWebKey
	fetchedAt time.Time // zero until the first successful fetch
	doneAt    time.Time // end of the last fetch, successful or not
	lastErr   error     // error of the last fetch, nil after a success
	inflight  *keyFetch // the running fetch, nil when none runs
}

// keyFetch is one JWKS fetch any number of callers can wait for.
type keyFetch struct {
	done chan struct{} // closed once keys and err are set
	keys []jose.JSONWebKey
	err  error
}

func newCachedKeySet(url string, client *http.Client) *cachedKeySet {
	return &cachedKeySet{url: url, client: client, now: time.Now, fetchTimeout: jwksFetchTimeout}
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

	cached := k.cachedKeys()
	if payload, ok := verifyWith(jws, cached, keyID); ok {
		return payload, nil
	}
	// Unknown key: fetch again, unless the last fetch ended too recently.
	keys, refreshErr := k.refreshedKeys(ctx)
	if payload, ok := verifyWith(jws, keys, keyID); ok {
		return payload, nil
	}
	// A key the bridge holds that does not verify the signature is a bad
	// signature whatever the refresh did. Its error must not carry the
	// refresh error: failures are categorised by their text, and a JWKS
	// fetch error such as an expired TLS certificate would relabel a
	// forged token as expired.
	if refreshErr != nil && !holdsKey(cached, keyID) && !holdsKey(keys, keyID) {
		// The bridge holds no current key set to judge a key it does not
		// know.
		markKeysUnavailable(ctx)
		return nil, fmt.Errorf("failed to verify token signature: %w", refreshErr)
	}
	return nil, errors.New("failed to verify token signature")
}

// prime fetches the keys once and waits for the result, so a bridge that
// cannot reach its JWKS fails at startup rather than on every request.
func (k *cachedKeySet) prime(ctx context.Context) error {
	_, err := k.refreshedKeys(ctx)
	return err
}

// holdsKey reports whether keys include one with the ID keyID. A token
// without a key ID names no key.
func holdsKey(keys []jose.JSONWebKey, keyID string) bool {
	if keyID == "" {
		return false
	}
	for i := range keys {
		if keys[i].KeyID == keyID {
			return true
		}
	}
	return false
}

// verifyOutcome records, for one Validate call, whether the signature
// check failed because no current key set was available. go-oidc wraps
// every key set error with %v, so the classification travels in the
// context instead of the error chain.
type verifyOutcome struct {
	keysUnavailable bool
}

type verifyOutcomeKey struct{}

func withVerifyOutcome(ctx context.Context) (context.Context, *verifyOutcome) {
	o := &verifyOutcome{}
	return context.WithValue(ctx, verifyOutcomeKey{}, o), o
}

func markKeysUnavailable(ctx context.Context) {
	if o, ok := ctx.Value(verifyOutcomeKey{}).(*verifyOutcome); ok {
		o.keysUnavailable = true
	}
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

// cachedKeys returns the cached keys without waiting. When there are none
// or they are older than jwksMaxAge, it starts a fetch in the background
// (if no fetch runs and the rate limit allows one).
func (k *cachedKeySet) cachedKeys() []jose.JSONWebKey {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	stale := k.fetchedAt.IsZero() || now.Sub(k.fetchedAt) > jwksMaxAge
	if stale && k.mayFetchLocked(now) {
		k.startFetchLocked()
	}
	return k.keys
}

// refreshedKeys waits for the running fetch, or starts one when the rate
// limit allows it, and returns the keys after it. Rate-limited, it returns
// the cached keys at once. The error, when set, says why the returned
// keys may be out of date: the fetch failed (the keys are then the last
// good ones, possibly none) or ctx ended while waiting.
func (k *cachedKeySet) refreshedKeys(ctx context.Context) ([]jose.JSONWebKey, error) {
	k.mu.Lock()
	f := k.inflight
	if f == nil {
		if !k.mayFetchLocked(k.now()) {
			keys, lastErr := k.keys, k.lastErr
			k.mu.Unlock()
			if lastErr != nil {
				return keys, fmt.Errorf("last JWKS fetch failed: %w", lastErr)
			}
			return keys, nil
		}
		f = k.startFetchLocked()
	}
	k.mu.Unlock()
	select {
	case <-f.done:
		return f.keys, f.err
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the JWKS fetch: %w", context.Cause(ctx))
	}
}

// mayFetchLocked reports whether a fetch may start now: none runs, and the
// last one ended at least jwksMinRefresh ago.
func (k *cachedKeySet) mayFetchLocked(now time.Time) bool {
	return k.inflight == nil && (k.doneAt.IsZero() || now.Sub(k.doneAt) >= jwksMinRefresh)
}

func (k *cachedKeySet) startFetchLocked() *keyFetch {
	f := &keyFetch{done: make(chan struct{})}
	k.inflight = f
	go k.runFetch(f)
	return f
}

// runFetch performs f. It is detached from every request: a caller that
// gives up must not fail the fetch for the others waiting for it. The
// fetch timeout bounds it.
func (k *cachedKeySet) runFetch(f *keyFetch) {
	ctx, cancel := context.WithTimeout(context.Background(), k.fetchTimeout)
	defer cancel()
	keys, err := k.fetch(ctx)

	k.mu.Lock()
	now := k.now()
	k.doneAt, k.lastErr = now, err
	if err == nil {
		k.keys, k.fetchedAt = keys, now
	}
	f.keys, f.err = k.keys, err
	k.inflight = nil
	k.mu.Unlock()
	close(f.done)
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
	// Any JSON object decodes: a URL that names the discovery document or
	// another JSON endpoint yields no keys, and replacing the last good
	// keys with none would fail every token.
	for i := range set.Keys {
		if set.Keys[i].IsPublic() {
			return set.Keys, nil
		}
	}
	return nil, fmt.Errorf("JWKS contains no public key (%d keys)", len(set.Keys))
}
