// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// sourceIdleTTL is how long a source's bucket is kept without traffic.
	sourceIdleTTL = 10 * time.Minute
	// pruneInterval bounds how often idle buckets are dropped.
	pruneInterval = time.Minute
)

// SourceLimiter is a token bucket per source IP. The credential endpoint
// is reachable on every node's NodePort and from every pod; without a
// limit one source can keep the bridge busy validating tokens and reading
// Secrets for everyone else. Kubelet on a node calls through its node's
// address and caches credentials per registry, so a generous per-source
// budget never limits it in practice.
type SourceLimiter struct {
	limit rate.Limit
	burst int
	now   func() time.Time

	mu        sync.Mutex
	sources   map[string]*sourceBucket
	lastPrune time.Time
}

type sourceBucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

// NewSourceLimiter returns a limiter allowing perSecond requests per source
// with the given burst, or nil (no limit) when perSecond <= 0.
func NewSourceLimiter(perSecond float64, burst int) *SourceLimiter {
	if perSecond <= 0 {
		return nil
	}
	if burst < 1 {
		burst = 1
	}
	return &SourceLimiter{limit: rate.Limit(perSecond), burst: burst, now: time.Now, sources: map[string]*sourceBucket{}}
}

// Allow reports whether source may make one more request now.
func (s *SourceLimiter) Allow(source string) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Sub(s.lastPrune) > pruneInterval {
		for k, b := range s.sources {
			if now.Sub(b.seen) > sourceIdleTTL {
				delete(s.sources, k)
			}
		}
		s.lastPrune = now
	}
	b, ok := s.sources[source]
	if !ok {
		b = &sourceBucket{limiter: rate.NewLimiter(s.limit, s.burst)}
		s.sources[source] = b
	}
	b.seen = now
	return b.limiter.AllowN(now, 1)
}

// sourceIP is the connection's remote address without the port. It is
// the TCP peer, not a forwarded header: the bridge sits behind a NodePort,
// not an HTTP proxy, and a header is whatever the caller wants it to be.
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
