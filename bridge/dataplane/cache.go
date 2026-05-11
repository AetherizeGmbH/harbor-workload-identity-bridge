// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"fmt"
	"time"

	"github.com/jellydator/ttlcache/v3"
)

// CacheKey identifies a cached docker token. Generation is the HarborAccess
// CR's metadata.generation at the time the token was minted — permission
// changes bump generation and naturally evict older entries (lazy
// invalidation, see docs/adr/0007-cache-invalidation-on-cr-change.md).
//
// Subject is the sub claim from the validated SA token; the same robot
// can serve many subjects (e.g. multiple pods running the same SA), but
// caching per-subject lets us observe and log per-workload behaviour.
type CacheKey struct {
	HarborAccessNamespace string
	HarborAccessName      string
	Generation            int64
	Subject               string
}

// String returns a stable string form used as the underlying ttlcache key.
func (k CacheKey) String() string {
	return fmt.Sprintf("%s/%s@%d/%s",
		k.HarborAccessNamespace, k.HarborAccessName, k.Generation, k.Subject)
}

// DockerTokenCache holds short-lived bearer JWTs minted via Harbor's
// /service/token endpoint. It is the bridge's defense against
// /service/token being a single Harbor process (ADR-0005): on every cache
// hit we skip the Harbor round-trip entirely.
type DockerTokenCache interface {
	// Get returns the cached token and true on hit, nil and false on miss
	// or expiry.
	Get(key CacheKey) (*DockerToken, bool)

	// Set stores token under key with the given TTL. The TTL should be
	// min(token.ExpiresIn, spec.tokenTTL) so the cache never serves a
	// token past either limit.
	Set(key CacheKey, token *DockerToken, ttl time.Duration)

	// Len returns the current number of unexpired entries. Used by
	// metrics.
	Len() int

	// Stop releases the cache's internal eviction goroutine. Call before
	// the bridge shuts down.
	Stop()
}

// NewDockerTokenCache constructs a cache with the given LRU capacity.
// When capacity is reached, ttlcache evicts the least-recently-used
// entry. capacity == 0 disables the cap (library default).
//
// We disable ttlcache's default touch-on-hit behaviour: an entry's
// expiration is set once at Set time and never extended on Get. This is
// load-bearing for correctness — the cache TTL is derived from the
// docker JWT's own exp claim (`token.ExpiresIn`), and extending it on
// hit would let us serve JWTs past their actual exp, after which
// containerd would receive expired tokens from the registry handshake.
func NewDockerTokenCache(capacity uint64) DockerTokenCache {
	opts := []ttlcache.Option[string, *DockerToken]{
		ttlcache.WithDisableTouchOnHit[string, *DockerToken](),
	}
	if capacity > 0 {
		opts = append(opts, ttlcache.WithCapacity[string, *DockerToken](capacity))
	}
	inner := ttlcache.New[string, *DockerToken](opts...)
	go inner.Start()
	return &ttlCache{inner: inner}
}

type ttlCache struct {
	inner *ttlcache.Cache[string, *DockerToken]
}

func (c *ttlCache) Get(key CacheKey) (*DockerToken, bool) {
	// ttlcache.Get already evicts expired items and returns nil for them;
	// no IsExpired() guard needed.
	item := c.inner.Get(key.String())
	if item == nil {
		return nil, false
	}
	return item.Value(), true
}

func (c *ttlCache) Set(key CacheKey, token *DockerToken, ttl time.Duration) {
	c.inner.Set(key.String(), token, ttl)
}

func (c *ttlCache) Len() int {
	return c.inner.Len()
}

func (c *ttlCache) Stop() {
	c.inner.Stop()
}
