// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// runSync is the DaemonSet's long-running container: it keeps the
// on-host CA/mTLS files in step with the mounted Secret volumes, which
// kubelet updates in place when cert-manager rotates the certificates.
// It never touches kubelet — the plugin re-reads these files on every
// exec (ADR-0021). It also keeps the pod alive so `kubectl logs`
// surfaces the install output per node.
func runSync(cfg *config) error {
	interval, err := time.ParseDuration(cfg.SyncInterval)
	if err != nil {
		return fmt.Errorf("SYNC_INTERVAL is not a duration: %w", err)
	}
	if interval <= 0 {
		return fmt.Errorf("SYNC_INTERVAL must be positive (got %s)", interval)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)

	logf("sync loop started (interval %s)", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := syncAuxFiles(cfg); err != nil {
				// Transient volume states (mid-rotation) must not
				// crash-loop the DaemonSet; log and retry next tick.
				logf("sync pass failed (will retry): %v", err)
			}
		case sig := <-stop:
			logf("received %s; exiting", sig)
			return nil
		}
	}
}
