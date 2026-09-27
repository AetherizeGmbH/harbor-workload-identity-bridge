// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a clock the tests advance by hand. The key set reads it
// from its fetch goroutine too, hence the lock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// jwksServer serves a JWKS through a handler the test can swap, counts
// the requests, and can hang a request until the test ends.
type jwksServer struct {
	*httptest.Server
	hits    atomic.Int32
	release chan struct{}

	mu      sync.Mutex
	handler http.HandlerFunc
}

func newJWKSServer(t *testing.T, h http.HandlerFunc) *jwksServer {
	t.Helper()
	s := &jwksServer{release: make(chan struct{}), handler: h}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		h := s.handler
		s.mu.Unlock()
		h(w, r)
	}))
	// Release hanging requests first: Close waits for them.
	t.Cleanup(func() {
		close(s.release)
		s.Close()
	})
	return s
}

func (s *jwksServer) set(h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler = h
}

// hang blocks like a blackholed apiserver: until the client gives up or
// the test ends.
func (s *jwksServer) hang(_ http.ResponseWriter, r *http.Request) {
	select {
	case <-s.release:
	case <-r.Context().Done():
	}
}

func jwksBody(t *testing.T, key *rsa.PrivateKey, kid string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func serveBody(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

func serveStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// keySetFixture is a key set on a fake clock against a jwksServer that
// initially serves the fixture issuer's key. fetchTimeout bounds a
// hanging fetch (the client timeout). Tests that assert a caller did not
// wait for a hanging fetch keep their bound well below it, so a loaded CI
// runner does not turn them flaky.
func keySetFixture(t *testing.T, fetchTimeout time.Duration) (*fixtureIssuer, *jwksServer, *cachedKeySet, *fakeClock) {
	t.Helper()
	fi := newFixtureIssuer(t)
	srv := newJWKSServer(t, serveBody(jwksBody(t, fi.key, fi.kid)))
	clock := newFakeClock()
	ks := newCachedKeySet(srv.URL, &http.Client{Timeout: fetchTimeout})
	ks.now = clock.now
	return fi, srv, ks, clock
}

func forgedToken(t *testing.T, fi *fixtureIssuer, kid string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return signWith(t, key, kid, fi.standardClaims())
}

// While a refresh for an unknown key hangs, a token signed by a cached,
// fresh key verifies at once. The key set used to hold its mutex across
// the fetch, so every verification waited for it.
func TestCachedKeySet_KnownKeyNeverWaitsForARefresh(t *testing.T) {
	fi, srv, ks, clock := keySetFixture(t, 5*time.Second)
	valid := fi.signToken(t, fi.standardClaims())
	if _, err := ks.VerifySignature(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	srv.set(srv.hang)
	clock.advance(jwksMinRefresh)
	forged := forgedToken(t, fi, "unknown-kid")
	go func() { _, _ = ks.VerifySignature(context.Background(), forged) }()
	eventually(t, "the unknown key's refresh to start", func() bool { return srv.hits.Load() == 2 })

	start := time.Now()
	if _, err := ks.VerifySignature(context.Background(), valid); err != nil {
		t.Fatalf("known key during a hanging refresh: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("known-key verification took %s: it waited for the hanging refresh", d)
	}
}

// Keys past jwksMaxAge are refreshed in the background while the known
// ones keep verifying. A hanging apiserver used to make every caller run
// its own serial fetch under the mutex, so the Nth caller waited N fetch
// timeouts although valid keys were cached.
func TestCachedKeySet_StaleKeysServeKnownKeysWhileTheRefreshHangs(t *testing.T) {
	fi, srv, ks, clock := keySetFixture(t, 5*time.Second)
	valid := fi.signToken(t, fi.standardClaims())
	if _, err := ks.VerifySignature(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	srv.set(srv.hang)
	clock.advance(jwksMaxAge + time.Second)

	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ks.VerifySignature(context.Background(), valid); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("known key with stale keys: %v", err)
	}
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("20 known-key verifications took %s with a hanging refresh", d)
	}
	eventually(t, "the background refresh", func() bool { return srv.hits.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)
	if n := srv.hits.Load(); n != 2 {
		t.Fatalf("%d JWKS requests, want 2: one initial fetch and one background refresh", n)
	}
}

// A caller that must wait for a fetch (no keys yet, or an unknown key)
// waits only as long as its own request allows; the fetch goes on for
// the next caller.
func TestCachedKeySet_UnknownKeyWaitIsBoundedByTheRequest(t *testing.T) {
	fi, srv, ks, _ := keySetFixture(t, 5*time.Second)
	srv.set(srv.hang)
	valid := fi.signToken(t, fi.standardClaims())
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		start := time.Now()
		_, err := ks.VerifySignature(ctx, valid)
		cancel()
		if err == nil {
			t.Fatal("verified without keys")
		}
		if d := time.Since(start); d > 2500*time.Millisecond {
			t.Fatalf("call %d waited %s past its 200ms deadline", i+1, d)
		}
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("%d JWKS requests, want 1: the second caller joins the running fetch", n)
	}
}

// The rate limit runs from the end of a fetch. A fetch that took longer
// than jwksMinRefresh (a hanging apiserver) used to let the next forged
// token start another one at once.
func TestCachedKeySet_RateLimitCountsFromTheEndOfAFetch(t *testing.T) {
	fi, srv, ks, clock := keySetFixture(t, 3*time.Second)
	valid := fi.signToken(t, fi.standardClaims())
	if _, err := ks.VerifySignature(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	srv.set(func(w http.ResponseWriter, _ *http.Request) {
		clock.advance(jwksMinRefresh + time.Second) // the fetch takes longer than the rate limit
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	clock.advance(jwksMinRefresh)
	if _, err := ks.VerifySignature(context.Background(), forgedToken(t, fi, "forged-1")); err == nil {
		t.Fatal("forged token verified")
	}
	if n := srv.hits.Load(); n != 2 {
		t.Fatalf("%d JWKS requests after the first forged token, want 2", n)
	}
	if _, err := ks.VerifySignature(context.Background(), forgedToken(t, fi, "forged-2")); err == nil {
		t.Fatal("forged token verified")
	}
	if n := srv.hits.Load(); n != 2 {
		t.Fatalf("%d JWKS requests: a fetch started right after one that ended less than jwksMinRefresh ago", n)
	}
	if _, err := ks.VerifySignature(context.Background(), valid); err != nil {
		t.Fatalf("known key after a failed refresh: %v", err)
	}
}

// A known key ID is looked up again once the keys are older than
// jwksMaxAge, and not before; a key the issuer replaced is then dropped.
func TestCachedKeySet_RefreshesAfterMaxAge(t *testing.T) {
	fi, srv, ks, clock := keySetFixture(t, 3*time.Second)
	old := fi.signToken(t, fi.standardClaims())
	if _, err := ks.VerifySignature(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	clock.advance(jwksMaxAge - time.Second)
	if _, err := ks.VerifySignature(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if n := srv.hits.Load(); n != 1 {
		t.Fatalf("%d JWKS requests before jwksMaxAge, want 1", n)
	}

	// Same key ID, new key: only a refresh can tell.
	newKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	srv.set(serveBody(jwksBody(t, newKey, fi.kid)))
	clock.advance(2 * time.Second)
	if _, err := ks.VerifySignature(context.Background(), old); err != nil {
		t.Fatalf("known key past jwksMaxAge while the refresh runs: %v", err)
	}
	// The known key alone triggers the refresh.
	eventually(t, "the refresh by age", func() bool {
		ks.mu.Lock()
		defer ks.mu.Unlock()
		return srv.hits.Load() == 2 && ks.inflight == nil
	})
	if _, err := ks.VerifySignature(context.Background(), old); err == nil {
		t.Fatal("the replaced key still verifies after the refresh")
	}
	if _, err := ks.VerifySignature(context.Background(), signWith(t, newKey, fi.kid, fi.standardClaims())); err != nil {
		t.Fatalf("new key after the refresh: %v", err)
	}
	if n := srv.hits.Load(); n != 2 {
		t.Fatalf("%d JWKS requests, want 2", n)
	}
}

// A failed refresh keeps the last good keys, so an apiserver blip does
// not turn into a failed verification for every pull.
func TestCachedKeySet_KeepsTheLastGoodKeysWhenARefreshFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"error status", serveStatus(http.StatusInternalServerError)},
		{"not a JWKS", serveBody([]byte("<html>"))},
		{"oversized", serveBody(bytes.Repeat([]byte(" "), maxJWKSSize+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fi, srv, ks, clock := keySetFixture(t, 3*time.Second)
			valid := fi.signToken(t, fi.standardClaims())
			if _, err := ks.VerifySignature(context.Background(), valid); err != nil {
				t.Fatal(err)
			}
			srv.set(tc.handler)

			// Refresh by age, in the background.
			clock.advance(jwksMaxAge + time.Second)
			if _, err := ks.VerifySignature(context.Background(), valid); err != nil {
				t.Fatalf("during the refresh: %v", err)
			}
			eventually(t, "the background refresh", func() bool { return srv.hits.Load() == 2 })
			eventually(t, "the failed refresh to finish", func() bool {
				ks.mu.Lock()
				defer ks.mu.Unlock()
				return ks.inflight == nil
			})
			if _, err := ks.VerifySignature(context.Background(), valid); err != nil {
				t.Fatalf("after the failed refresh: %v", err)
			}

			// Refresh for an unknown key, waited for.
			clock.advance(jwksMinRefresh)
			if _, err := ks.VerifySignature(context.Background(), forgedToken(t, fi, "forged")); err == nil {
				t.Fatal("forged token verified")
			}
			if n := srv.hits.Load(); n != 3 {
				t.Fatalf("%d JWKS requests, want 3", n)
			}
			if _, err := ks.VerifySignature(context.Background(), valid); err != nil {
				t.Fatalf("after the failed unknown-key refresh: %v", err)
			}
		})
	}
}

// The JWKS response is capped at maxJWKSSize.
func TestCachedKeySet_BoundsTheJWKSSize(t *testing.T) {
	fi := newFixtureIssuer(t)
	body := jwksBody(t, fi.key, fi.kid)
	padded := append(append([]byte{}, body...), bytes.Repeat([]byte(" "), maxJWKSSize-len(body))...)
	for _, tc := range []struct {
		name string
		body []byte
		ok   bool
	}{
		{"exactly the cap", padded, true},
		{"one byte over", append(append([]byte{}, padded...), ' '), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newJWKSServer(t, serveBody(tc.body))
			ks := newCachedKeySet(srv.URL, &http.Client{Timeout: 3 * time.Second})
			_, err := ks.VerifySignature(context.Background(), fi.signToken(t, fi.standardClaims()))
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "larger than") {
				t.Fatalf("err = %v, want the size refusal", err)
			}
		})
	}
}
