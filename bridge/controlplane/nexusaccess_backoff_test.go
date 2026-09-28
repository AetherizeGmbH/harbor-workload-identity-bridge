// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus/nexustest"
)

// gaugeValue reads the single gauge registered on reg.
func gaugeValue(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil || len(mfs) != 1 || len(mfs[0].GetMetric()) != 1 {
		t.Fatalf("gather: %v, %d families", err, len(mfs))
	}
	return mfs[0].GetMetric()[0].GetGauge().GetValue()
}

func TestNexusBackoff(t *testing.T) {
	nx := nexustest.New(t)
	clock := newStepClock()
	b := &NexusBackoff{Window: 10 * time.Minute, Clock: clock}
	c := b.Wrap(nx.Client(t))
	ctx := context.Background()
	gauge := b.RateLimitedGauge()
	reg := prometheus.NewRegistry()
	reg.MustRegister(gauge)

	if _, err := c.ListRoles(ctx, "bridge-"); err != nil {
		t.Fatal(err)
	}
	if gaugeValue(t, reg) != 0 {
		t.Error("gauge reports a block before any 429")
	}

	nx.SetRateLimited(true)
	_, err := c.ListRoles(ctx, "bridge-")
	var backoffErr *NexusBackoffError
	if !errors.Is(err, nexus.ErrRateLimited) || !errors.As(err, &backoffErr) || !backoffErr.Until.Equal(clock.Now().Add(10*time.Minute)) {
		t.Fatalf("429: %v, want ErrRateLimited with the backoff's end", err)
	}
	if gaugeValue(t, reg) != 1 {
		t.Error("gauge does not report the block")
	}

	// No call goes out during the window, authenticated or not, except the
	// status endpoints, which send no credentials.
	nx.SetRateLimited(false)
	calls := len(nx.Requests())
	clock.Advance(10*time.Minute - time.Second)
	for name, call := range map[string]func() error{
		"GetUser":      func() error { _, err := c.GetUser(ctx, "u"); return err },
		"ListUsers":    func() error { _, err := c.ListUsers(ctx, "u"); return err },
		"CreateUser":   func() error { _, err := c.CreateUser(ctx, nexus.User{UserID: "u"}, "p"); return err },
		"UpdateUser":   func() error { return c.UpdateUser(ctx, nexus.User{UserID: "u"}) },
		"DeleteUser":   func() error { return c.DeleteUser(ctx, "u") },
		"GetRole":      func() error { _, err := c.GetRole(ctx, "r"); return err },
		"ListRoles":    func() error { _, err := c.ListRoles(ctx, "r"); return err },
		"CreateRole":   func() error { _, err := c.CreateRole(ctx, nexus.Role{ID: "r"}); return err },
		"UpdateRole":   func() error { return c.UpdateRole(ctx, nexus.Role{ID: "r"}) },
		"DeleteRole":   func() error { return c.DeleteRole(ctx, "r") },
		"GetPrivilege": func() error { _, err := c.GetPrivilege(ctx, "p"); return err },
	} {
		if err := call(); !errors.As(err, &backoffErr) {
			t.Errorf("%s during the backoff: %v", name, err)
		}
	}
	if n := len(nx.Requests()); n != calls {
		t.Errorf("%d requests during the backoff", n-calls)
	}
	if err := c.Status(ctx); err != nil {
		t.Errorf("Status during the backoff: %v", err)
	}
	if _, err := c.Writable(ctx); err != nil {
		t.Errorf("Writable during the backoff: %v", err)
	}

	clock.Advance(time.Second)
	if _, err := c.ListRoles(ctx, "bridge-"); err != nil {
		t.Errorf("after the window: %v", err)
	}
	if gaugeValue(t, reg) != 0 {
		t.Error("gauge still reports a block after the window")
	}
	if msg := backoffErr.Error(); !strings.Contains(msg, EnvNexusRateLimitBackoff) {
		t.Errorf("message %q does not name the setting", msg)
	}
}

func TestNexusErrorReason(t *testing.T) {
	for err, want := range map[error]string{
		&nexus.APIError{Op: "list users", StatusCode: 429}:                                             ReasonNexusRateLimited,
		&NexusBackoffError{Until: time.Now()}:                                                          ReasonNexusRateLimited,
		&nexus.APIError{Op: "list users", StatusCode: 401}:                                             ReasonNexusAuthFailed,
		&nexus.APIError{Op: "list users", StatusCode: 403}:                                             ReasonNexusAuthFailed,
		&nexus.APIError{Op: `create user "u"`, StatusCode: 400, Message: "PARAMETER password: policy"}: ReasonPasswordRejected,
		&nexus.APIError{Op: `create user "u"`, StatusCode: 400, Message: "emailAddress: bad"}:          ReasonNexusError,
		&nexus.APIError{Op: `update role "r"`, StatusCode: 400, Message: "password"}:                   ReasonNexusError,
		&nexus.APIError{Op: "list users", StatusCode: 503}:                                             ReasonNexusError,
		errors.New("dial tcp: connection refused"):                                                     ReasonNexusError,
	} {
		wrapped := fmt.Errorf("pass: %w", err)
		if got := nexusErrorReason(wrapped); got != want {
			t.Errorf("nexusErrorReason(%v) = %s, want %s", err, got, want)
		}
	}
}
