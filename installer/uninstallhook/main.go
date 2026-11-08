// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Binary harbor-bridge-uninstall-hook is the chart's pre-delete hook
// (ADR-0039). Before `helm uninstall` deletes anything, it switches the
// release's plugin DaemonSet to the removal action (INSTALL_ACTION=remove),
// so that the installer on every node takes this install's entry and files
// off the node, and waits until every node reports. It fails as soon as one
// node reports a failed removal, and at its deadline; the release then
// stays installed and `helm uninstall` can run again.
//
// It talks to the API server with the standard library only (ADR-0021):
// the Job's ServiceAccount token goes only to the in-cluster API server
// (KUBERNETES_SERVICE_HOST), read again for every request, over TLS
// verified against the ServiceAccount CA, never through a proxy and never
// after a redirect.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harbor-bridge-uninstall-hook: config error: %v\n", err)
		os.Exit(1)
	}
	api, err := newInClusterClient(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harbor-bridge-uninstall-hook: %v\n", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, api, cfg, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "harbor-bridge-uninstall-hook: %v\n", err)
		os.Exit(1)
	}
}

// config is what the hook reads from its environment; the chart renders it.
type config struct {
	Namespace string // NAMESPACE: the plugin namespace.
	DaemonSet string // DAEMONSET: the plugin DaemonSet's name.
	Provider  string // PROVIDER_NAME: the provider name the nodes report for.
	// ReleaseName and ReleaseNamespace name the Helm release the hook was
	// rendered for; the DaemonSet must carry them in Helm's annotations.
	ReleaseName      string // RELEASE_NAME
	ReleaseNamespace string // RELEASE_NAMESPACE
	// MaxUnavailable is plugin.uninstall.maxUnavailable: a number, or a
	// percentage of the nodes ("25%").
	MaxUnavailable string        // MAX_UNAVAILABLE
	Timeout        time.Duration // TIMEOUT_SECONDS
	PollInterval   time.Duration
}

func loadConfig(getenv func(string) string) (*config, error) {
	c := &config{
		Namespace:        getenv("NAMESPACE"),
		DaemonSet:        getenv("DAEMONSET"),
		Provider:         getenv("PROVIDER_NAME"),
		ReleaseName:      getenv("RELEASE_NAME"),
		ReleaseNamespace: getenv("RELEASE_NAMESPACE"),
		MaxUnavailable:   getenv("MAX_UNAVAILABLE"),
		PollInterval:     5 * time.Second,
	}
	for name, v := range map[string]string{
		"NAMESPACE": c.Namespace, "DAEMONSET": c.DaemonSet, "PROVIDER_NAME": c.Provider,
		"RELEASE_NAME": c.ReleaseName, "RELEASE_NAMESPACE": c.ReleaseNamespace,
	} {
		if !dnsName(v) {
			return nil, fmt.Errorf("%s must be a DNS name (got %q)", name, v)
		}
	}
	if c.MaxUnavailable == "" {
		c.MaxUnavailable = "25%"
	}
	if _, err := budget(c.MaxUnavailable, 1, 0); err != nil {
		return nil, fmt.Errorf("MAX_UNAVAILABLE: %w", err)
	}
	secs := getenv("TIMEOUT_SECONDS")
	if secs == "" {
		secs = "270"
	}
	n, err := strconv.Atoi(secs)
	if err != nil || n <= 0 {
		return nil, fmt.Errorf("TIMEOUT_SECONDS must be a positive number of seconds (got %q)", secs)
	}
	c.Timeout = time.Duration(n) * time.Second
	return c, nil
}

// dnsName reports whether s is a lower-case DNS subdomain: the names of
// Kubernetes objects and provider names (a DNS label is one too).
func dnsName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}

// budget returns how many pods the removal rollout may take down at once:
// maxUnavailable (a number, or a percentage of desired rounded up, as the
// DaemonSet rounds it) plus the pods that are unavailable already, so that
// nodes whose plugin pod is down anyway do not use up the budget, at least
// one and at most desired (ADR-0039, decision 5.2).
func budget(maxUnavailable string, desired, unavailable int) (int, error) {
	var n int
	if pct, ok := strings.CutSuffix(maxUnavailable, "%"); ok {
		p, err := strconv.Atoi(pct)
		if err != nil || p < 1 || p > 100 {
			return 0, fmt.Errorf("%q is not a percentage from 1%% to 100%%", maxUnavailable)
		}
		n = (p*desired + 99) / 100
	} else {
		v, err := strconv.Atoi(maxUnavailable)
		if err != nil || v < 1 {
			return 0, fmt.Errorf("%q is not a number of at least 1 or a percentage", maxUnavailable)
		}
		n = v
	}
	n += max(unavailable, 0)
	if desired > 0 {
		n = min(n, desired)
	}
	return max(n, 1), nil
}
