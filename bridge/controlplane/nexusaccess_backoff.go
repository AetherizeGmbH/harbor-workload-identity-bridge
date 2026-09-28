// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
)

// NexusBackoff stops every Nexus call made with the admin credential for
// Window after Nexus answered one with 429 (ADR-0036 decision j).
//
// From Nexus 3.94 on a username past the failed-login limit is refused
// with 429 before its password is checked, and every request during the
// block keeps it alive; Retry-After is not when requests succeed again.
// The block ends only after nexus.auth.ratelimit.max-delay-seconds
// (default 900 s) without any request for that username. So the bridge
// does not probe: it stops calling for Window (BRIDGE_NEXUS_RATE_LIMIT_BACKOFF,
// default 15 minutes), counted from the last 429. The reconciler and the
// janitor share one NexusBackoff through the client Wrap returns.
type NexusBackoff struct {
	// Window is how long calls stop after a 429. Positive.
	Window time.Duration

	// Clock defaults to RealClock.
	Clock Clock

	mu    sync.Mutex
	until time.Time
}

// NexusBackoffError is returned, instead of a call, while the backoff is
// active. It matches nexus.ErrRateLimited.
type NexusBackoffError struct {
	// Until is when calls resume.
	Until time.Time
}

func (e *NexusBackoffError) Error() string {
	return fmt.Sprintf("Nexus answered 429 to the bridge's admin credential: every Nexus call with it is suspended until %s (%s), "+
		"so rotation and revocation are paused; the block ends early when an administrator updates the admin user or Nexus restarts, "+
		"and a restart of the bridge ends the wait", e.Until.UTC().Format(time.RFC3339), EnvNexusRateLimitBackoff)
}

// Is matches nexus.ErrRateLimited.
func (e *NexusBackoffError) Is(target error) bool {
	return target == nexus.ErrRateLimited
}

func (b *NexusBackoff) now() time.Time {
	if b.Clock == nil {
		return time.Now()
	}
	return b.Clock.Now()
}

// Until returns when calls resume, and whether they are stopped now.
func (b *NexusBackoff) Until() (time.Time, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.until, b.now().Before(b.until)
}

// check returns an *NexusBackoffError while calls are stopped.
func (b *NexusBackoff) check() error {
	if until, blocked := b.Until(); blocked {
		return &NexusBackoffError{Until: until}
	}
	return nil
}

// observe starts the window when err is a 429 from Nexus, and explains the
// consequence in the returned error.
func (b *NexusBackoff) observe(err error) error {
	var apiErr *nexus.APIError
	if !errors.As(err, &apiErr) || !errors.Is(apiErr, nexus.ErrRateLimited) {
		return err
	}
	b.mu.Lock()
	b.until = b.now().Add(b.Window)
	until := b.until
	b.mu.Unlock()
	return fmt.Errorf("%w; %w", err, &NexusBackoffError{Until: until})
}

// Wrap returns a client that refuses every authenticated call while the
// backoff is active and starts it on a 429. The status endpoints send no
// credentials and pass through.
func (b *NexusBackoff) Wrap(c nexus.Client) nexus.Client {
	return &gatedNexusClient{inner: c, backoff: b}
}

// RateLimitedGauge returns a gauge that reports 1 while calls are stopped,
// for alerting (ADR-0036 decision j).
func (b *NexusBackoff) RateLimitedGauge() prometheus.GaugeFunc {
	return prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "bridge_nexus_rate_limited",
		Help: "1 while the bridge has stopped calling Nexus because Nexus answered 429 to its admin credential " +
			"(BRIDGE_NEXUS_RATE_LIMIT_BACKOFF); rotation and revocation of Nexus users are paused meanwhile.",
	}, func() float64 {
		if _, blocked := b.Until(); blocked {
			return 1
		}
		return 0
	})
}

// gatedNexusClient is NexusBackoff.Wrap's client.
type gatedNexusClient struct {
	inner   nexus.Client
	backoff *NexusBackoff
}

func gate[T any](b *NexusBackoff, call func() (T, error)) (T, error) {
	if err := b.check(); err != nil {
		var zero T
		return zero, err
	}
	v, err := call()
	return v, b.observe(err)
}

func (g *gatedNexusClient) GetUser(ctx context.Context, userID string) (*nexus.User, error) {
	return gate(g.backoff, func() (*nexus.User, error) { return g.inner.GetUser(ctx, userID) })
}

func (g *gatedNexusClient) ListUsers(ctx context.Context, prefix string) ([]nexus.User, error) {
	return gate(g.backoff, func() ([]nexus.User, error) { return g.inner.ListUsers(ctx, prefix) })
}

func (g *gatedNexusClient) CreateUser(ctx context.Context, user nexus.User, password string) (*nexus.User, error) {
	return gate(g.backoff, func() (*nexus.User, error) { return g.inner.CreateUser(ctx, user, password) })
}

func (g *gatedNexusClient) UpdateUser(ctx context.Context, user nexus.User) error {
	_, err := gate(g.backoff, func() (struct{}, error) { return struct{}{}, g.inner.UpdateUser(ctx, user) })
	return err
}

func (g *gatedNexusClient) DeleteUser(ctx context.Context, userID string) error {
	_, err := gate(g.backoff, func() (struct{}, error) { return struct{}{}, g.inner.DeleteUser(ctx, userID) })
	return err
}

func (g *gatedNexusClient) GetRole(ctx context.Context, id string) (*nexus.Role, error) {
	return gate(g.backoff, func() (*nexus.Role, error) { return g.inner.GetRole(ctx, id) })
}

func (g *gatedNexusClient) ListRoles(ctx context.Context, prefix string) ([]nexus.Role, error) {
	return gate(g.backoff, func() ([]nexus.Role, error) { return g.inner.ListRoles(ctx, prefix) })
}

func (g *gatedNexusClient) CreateRole(ctx context.Context, role nexus.Role) (*nexus.Role, error) {
	return gate(g.backoff, func() (*nexus.Role, error) { return g.inner.CreateRole(ctx, role) })
}

func (g *gatedNexusClient) UpdateRole(ctx context.Context, role nexus.Role) error {
	_, err := gate(g.backoff, func() (struct{}, error) { return struct{}{}, g.inner.UpdateRole(ctx, role) })
	return err
}

func (g *gatedNexusClient) DeleteRole(ctx context.Context, id string) error {
	_, err := gate(g.backoff, func() (struct{}, error) { return struct{}{}, g.inner.DeleteRole(ctx, id) })
	return err
}

func (g *gatedNexusClient) GetPrivilege(ctx context.Context, name string) (*nexus.Privilege, error) {
	return gate(g.backoff, func() (*nexus.Privilege, error) { return g.inner.GetPrivilege(ctx, name) })
}

func (g *gatedNexusClient) Status(ctx context.Context) error {
	return g.inner.Status(ctx)
}

func (g *gatedNexusClient) Writable(ctx context.Context) (bool, error) {
	return g.inner.Writable(ctx)
}
