// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"testing"
	"time"
)

func sampleKey() CacheKey {
	return CacheKey{
		HarborAccessNamespace: "harbor-bridge-system",
		HarborAccessName:      "flux-access",
		Generation:            1,
		Subject:               "system:serviceaccount:flux-system:source-controller",
	}
}

func sampleToken(suffix string) *DockerToken {
	return &DockerToken{
		Token:     "fake.jwt." + suffix,
		Issued:    time.Now(),
		ExpiresIn: time.Hour,
	}
}

func TestCache_HitMiss(t *testing.T) {
	c := NewDockerTokenCache(0)
	t.Cleanup(c.Stop)

	key := sampleKey()
	if _, ok := c.Get(key); ok {
		t.Errorf("expected miss on empty cache")
	}
	c.Set(key, sampleToken("a"), time.Hour)
	got, ok := c.Get(key)
	if !ok {
		t.Fatal("expected hit after Set")
	}
	if got.Token != "fake.jwt.a" {
		t.Errorf("returned token = %q", got.Token)
	}
}

func TestCache_GenerationKeyedInvalidation(t *testing.T) {
	// ADR-0007 lazy invalidation: a permissions edit bumps the CR
	// generation, the next Get uses a new cache key, and the older
	// entry is no longer returned (it ages out on its own TTL).
	c := NewDockerTokenCache(0)
	t.Cleanup(c.Stop)

	g1 := sampleKey()
	c.Set(g1, sampleToken("gen1"), time.Hour)

	g2 := g1
	g2.Generation = 2
	if _, ok := c.Get(g2); ok {
		t.Errorf("Get with bumped generation should miss")
	}

	// Original key still hits — older entry is preserved until TTL.
	if got, ok := c.Get(g1); !ok || got.Token != "fake.jwt.gen1" {
		t.Errorf("old-generation entry should still be cached: ok=%v token=%v", ok, got)
	}
}

func TestCache_SubjectIsPartOfKey(t *testing.T) {
	// Same CR + generation, different requesting SAs → separate cache
	// entries. Pods on the same SA share, distinct SAs do not.
	c := NewDockerTokenCache(0)
	t.Cleanup(c.Stop)

	k1 := sampleKey()
	k2 := k1
	k2.Subject = "system:serviceaccount:flux-system:other-sa"

	c.Set(k1, sampleToken("sa1"), time.Hour)
	if _, ok := c.Get(k2); ok {
		t.Errorf("different subject should miss")
	}
}

func TestCache_TTL_IsNotExtendedByGet(t *testing.T) {
	// ttlcache's default behaviour is to extend an entry's expiration on
	// every Get ("touch on hit"). For our use case this would be a
	// correctness bug: cache TTL is derived from the docker JWT's exp
	// claim, and touching it on Get would let the cache hold a token
	// past its actual JWT exp. We explicitly disable touch-on-hit in
	// NewDockerTokenCache; this test pins that behaviour so a future
	// library upgrade can't silently re-enable it.
	c := NewDockerTokenCache(0)
	t.Cleanup(c.Stop)

	key := sampleKey()
	c.Set(key, sampleToken("stable"), 80*time.Millisecond)

	// Hit at T+40ms (well within TTL).
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.Get(key); !ok {
		t.Fatal("expected hit at T+40ms")
	}

	// Past the original Set TTL of 80ms. If touch-on-hit were active,
	// the preceding Get would have reset the expiration; here we must
	// see a miss.
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.Get(key); ok {
		t.Errorf("entry must be expired at T+100ms; touch-on-hit appears to be active")
	}
}

func TestCache_TTLEviction(t *testing.T) {
	c := NewDockerTokenCache(0)
	t.Cleanup(c.Stop)

	key := sampleKey()
	c.Set(key, sampleToken("ttl"), 30*time.Millisecond)
	if _, ok := c.Get(key); !ok {
		t.Fatal("expected hit immediately after Set")
	}
	time.Sleep(60 * time.Millisecond)
	if _, ok := c.Get(key); ok {
		t.Errorf("expected miss after TTL elapses")
	}
}

func TestCache_CapacityEviction(t *testing.T) {
	// ttlcache evicts the LRU entry when capacity is exceeded. We don't
	// assert which entry is evicted — only that the cache stays bounded.
	c := NewDockerTokenCache(2)
	t.Cleanup(c.Stop)

	for i := 0; i < 5; i++ {
		k := sampleKey()
		k.Generation = int64(i)
		c.Set(k, sampleToken("g"), time.Hour)
	}
	if got := c.Len(); got > 2 {
		t.Errorf("cache exceeded capacity: Len() = %d, want <= 2", got)
	}
}

func TestCacheKey_String_Stable(t *testing.T) {
	// The string form is the actual map key inside ttlcache; changing it
	// silently is a cache-poisoning class of bug. Pin the format.
	k := CacheKey{
		HarborAccessNamespace: "harbor-bridge-system",
		HarborAccessName:      "flux-access",
		Generation:            7,
		Subject:               "system:serviceaccount:flux-system:source-controller",
	}
	want := "harbor-bridge-system/flux-access@7/system:serviceaccount:flux-system:source-controller"
	if got := k.String(); got != want {
		t.Errorf("CacheKey.String() = %q, want %q", got, want)
	}
}
