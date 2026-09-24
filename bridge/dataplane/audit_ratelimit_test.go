// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"golang.org/x/time/rate"
)

type captured struct {
	mu    sync.Mutex
	lines []string
}

func (c *captured) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.lines = append(c.lines, args)
	}, funcr.Options{})
}

func (c *captured) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.lines, "\n")
}

func TestSourceLimiter_PerSourceBuckets(t *testing.T) {
	l := NewSourceLimiter(1, 3)
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if !l.Allow("10.0.0.1") {
			t.Fatalf("request %d within the burst refused", i+1)
		}
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("request beyond the burst allowed")
	}
	if !l.Allow("10.0.0.2") {
		t.Fatal("another source shares the first source's bucket")
	}
	now = now.Add(time.Second)
	if !l.Allow("10.0.0.1") {
		t.Fatal("bucket did not refill")
	}
	var nilLimiter *SourceLimiter
	if !nilLimiter.Allow("x") || NewSourceLimiter(0, 10) != nil {
		t.Fatal("a zero rate must disable the limit")
	}
}

func TestSourceLimiter_PrunesIdleSources(t *testing.T) {
	l := NewSourceLimiter(1, 1)
	now := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return now }
	l.Allow("10.0.0.1")
	now = now.Add(sourceIdleTTL + pruneInterval + time.Second)
	l.Allow("10.0.0.2")
	if _, ok := l.sources["10.0.0.1"]; ok {
		t.Fatal("idle source not pruned")
	}
}

func TestHandler_RateLimitedBeforeTokenValidation(t *testing.T) {
	f, _, reg := metricsFixture(t)
	f.Handler.Limiter = NewSourceLimiter(0.001, 1)
	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		w := httptest.NewRecorder()
		f.Handler.ServeHTTP(w, bearerReq(t, "img"))
		if w.Code != want {
			t.Fatalf("request %d: status %d, want %d", i+1, w.Code, want)
		}
		if want == http.StatusTooManyRequests && w.Header().Get("Retry-After") == "" {
			t.Error("429 without Retry-After")
		}
	}
	if got := counter(t, reg, "bridge_credential_issuances_total", map[string]string{"result": ResultRateLimited}); got != 1 {
		t.Errorf("rate_limited counter = %v, want 1", got)
	}
}

// The audit trail must attribute every decision, and BRIDGE_LOG_LEVEL must
// not be able to silence it (main wires Audit at info).
func TestHandler_AuditLinesAttributeTheCaller(t *testing.T) {
	var audit captured
	f := newHandlerFixture(t)
	claims := newTestClaims()
	claims.Pod, claims.PodUID, claims.Node = "puller-7d9", "3f2c", "node-a"
	f.Validator.claims = claims
	f.Handler.Audit = audit.logger()

	w := httptest.NewRecorder()
	r := bearerReq(t, "harbor.example.com/production/app:"+strings.Repeat("x", 2*maxAuditImageLen))
	r.RemoteAddr = "10.1.2.3:51000"
	f.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	out := audit.joined()
	for _, want := range []string{`"credential issued"`, `"source"="10.1.2.3"`, `"pod"="puller-7d9"`, `"pod_uid"="3f2c"`, `"node"="node-a"`, `"requested_image"=`} {
		if !strings.Contains(out, want) {
			t.Errorf("audit line lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, strings.Repeat("x", maxAuditImageLen+1)) {
		t.Error("requested_image not truncated")
	}
	if strings.Contains(out, hTestRobotPass) {
		t.Fatal("audit line contains the robot password")
	}
}

func TestHandler_DenialsAreAudited(t *testing.T) {
	var audit captured
	f := newHandlerFixture(t)
	f.Validator.err = errors.New("oidc: token is expired")
	f.Handler.Audit = audit.logger()
	w := httptest.NewRecorder()
	r := bearerReq(t, "img")
	r.RemoteAddr = "10.9.9.9:4000"
	f.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
	out := audit.joined()
	for _, want := range []string{`"credential denied"`, `"reason"="invalid_token"`, `"source"="10.9.9.9"`} {
		if !strings.Contains(out, want) {
			t.Errorf("denial line lacks %s:\n%s", want, out)
		}
	}
}

func TestRateLimitedWriter_DropsBeyondTheRate(t *testing.T) {
	var buf bytes.Buffer
	w := &rateLimitedWriter{w: &buf, limiter: rate.NewLimiter(rate.Every(time.Hour), 2)}
	for i := 0; i < 5; i++ {
		if _, err := w.Write([]byte("line\n")); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(buf.String(), "line"); got != 2 {
		t.Fatalf("%d lines written, want 2", got)
	}
}
