// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	ctrl "sigs.k8s.io/controller-runtime"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
)

// SetupNexus adds the Nexus backend's control plane to mgr when cfg.Nexus
// is set, and does nothing otherwise (ADR-0036): a Nexus client that reads
// the admin credentials on every call, the rate-limit backoff with its
// gauge registered on reg (nil registers nothing), the NexusAccess
// reconciler and the Nexus janitor, both leader-only.
//
// The manager must have been built with nexusv1alpha1 in its scheme and
// with cfg.CacheOptions(); the data plane's readiness should wait for
// cfg.CachedObjects() (AddAfterCacheSync). A missing or empty admin
// credential file fails here, at startup.
func SetupNexus(mgr ctrl.Manager, cfg *Config, log logr.Logger, reg prometheus.Registerer) error {
	if cfg.Nexus == nil {
		return nil
	}
	gvk := nexusv1alpha1.GroupVersion.WithKind("NexusAccess")
	if !mgr.GetScheme().Recognizes(gvk) {
		return fmt.Errorf("the manager's scheme does not know %s: add nexusv1alpha1.AddToScheme before building the manager", gvk)
	}
	c, err := NewNexusClient(cfg.Nexus, log.WithName("nexus-credentials"))
	if err != nil {
		return err
	}
	backoff := &NexusBackoff{Window: cfg.Nexus.RateLimitBackoff}
	if reg != nil {
		if err := reg.Register(backoff.RateLimitedGauge()); err != nil {
			return fmt.Errorf("register the Nexus rate-limit gauge: %w", err)
		}
	}
	gated := backoff.Wrap(c)
	rec := &NexusReconciler{
		Client:  mgr.GetClient(),
		Scheme:  mgr.GetScheme(),
		Nexus:   gated,
		Backoff: backoff,
		Config:  cfg,
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup NexusAccess reconciler: %w", err)
	}
	if err := mgr.Add(&NexusJanitor{
		Client: mgr.GetClient(),
		// Uncached; see NexusJanitor.Reader.
		Reader: mgr.GetAPIReader(),
		Nexus:  gated,
		Config: cfg,
	}); err != nil {
		return fmt.Errorf("add Nexus janitor: %w", err)
	}
	return nil
}

// NewNexusClient builds the Nexus client for n. It reads the admin
// credentials from n.AdminDir on every call, so a rotated Secret takes
// effect without a restart; reading them once here fails startup on a
// missing or empty file.
func NewNexusClient(n *NexusConfig, log logr.Logger) (nexus.Client, error) {
	if n == nil || n.URL == nil {
		return nil, errors.New("the Nexus backend is not configured")
	}
	creds := NewNexusAdminCredsReader(n, log)
	username, password, err := creds.Read()
	if err != nil {
		return nil, fmt.Errorf("load Nexus admin creds: %w", err)
	}
	caPEM, err := n.LoadCA()
	if err != nil {
		return nil, err
	}
	transport, err := nexus.NewTransport(caPEM)
	if err != nil {
		return nil, fmt.Errorf("build Nexus transport: %w", err)
	}
	opts := []nexus.Option{nexus.WithCredentialSource(creds.Read)}
	if n.AllowHTTP {
		opts = append(opts, nexus.WithAllowInsecureHTTP())
	}
	c, err := nexus.NewClient(n.URL, username, password, transport, opts...)
	if err != nil {
		return nil, fmt.Errorf("build Nexus client: %w", err)
	}
	return c, nil
}
