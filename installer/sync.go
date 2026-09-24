// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"time"
)

// runSync is the DaemonSet's long-running container: it keeps the
// on-host CA/mTLS files in step with the mounted Secret volumes, which
// kubelet updates in place when cert-manager rotates the certificates.
// It never touches kubelet — the plugin re-reads these files on every
// exec (ADR-0021). It also keeps the pod alive so `kubectl logs`
// surfaces the install output per node. Returns when ctx is cancelled
// (SIGTERM/SIGINT in production).
func runSync(ctx context.Context, cfg *config) error {
	if cfg.SyncInterval <= 0 {
		return fmt.Errorf("sync interval must be positive (got %s)", cfg.SyncInterval)
	}
	logf("sync loop started (interval %s)", cfg.SyncInterval)
	ticker := time.NewTicker(cfg.SyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := syncAuxFiles(cfg); err != nil {
				// Transient volume states (mid-rotation) must not
				// crash-loop the DaemonSet; log and retry next tick.
				logf("sync pass failed (will retry): %v", err)
			}
		case <-ctx.Done():
			logf("sync loop stopping: %v", context.Cause(ctx))
			return nil
		}
	}
}
