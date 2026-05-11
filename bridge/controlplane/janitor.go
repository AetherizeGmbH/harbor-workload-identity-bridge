// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
)

// DefaultSweepInterval is how often the Janitor walks Harbor for orphan
// robots. Five minutes is short enough to clean up after a misconfigured
// HarborAccess delete (which would otherwise wait until next reconcile,
// but reconciliation never fires on a deleted CR), and long enough not to
// spam Harbor's audit log.
const DefaultSweepInterval = 5 * time.Minute

// Janitor periodically scans Harbor for robots managed by this bridge whose
// owning HarborAccess CR no longer exists, and deletes them. It enforces
// the ADR-0009 ownership-prefix invariant at every Harbor write site, plus
// the description-tag defense-in-depth check against prefix collisions.
//
// Janitor implements sigs.k8s.io/controller-runtime/pkg/manager.Runnable
// so it can be added to the same Manager as the reconciler.
type Janitor struct {
	Client client.Client
	Harbor harbor.Client
	Config *Config

	// SweepInterval is how often Start runs a sweep. Defaults to
	// DefaultSweepInterval when zero.
	SweepInterval time.Duration
}

// Start runs the periodic sweep loop until ctx is cancelled.
func (j *Janitor) Start(ctx context.Context) error {
	interval := j.SweepInterval
	if interval == 0 {
		interval = DefaultSweepInterval
	}
	logger := log.FromContext(ctx).WithName("janitor").WithValues("interval", interval)
	logger.Info("starting orphan-robot sweep")

	// Run once immediately so a freshly-started bridge does not wait an
	// entire interval before its first cleanup.
	if err := j.Sweep(ctx); err != nil {
		logger.Error(err, "initial sweep failed")
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("janitor stopped")
			return nil
		case <-ticker.C:
			if err := j.Sweep(ctx); err != nil {
				logger.Error(err, "sweep failed")
			}
		}
	}
}

// Sweep performs one pass: list all Harbor robots, filter to those owned by
// this bridge in this cluster, and delete any whose owning HarborAccess CR
// no longer exists. Exposed separately from Start for testability.
func (j *Janitor) Sweep(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("janitor")

	robots, err := j.Harbor.List(ctx)
	if err != nil {
		return fmt.Errorf("list robots: %w", err)
	}

	for i := range robots {
		robot := &robots[i]

		// Layer 1: ownership prefix (ADR-0009 invariant). A bridge MUST
		// NOT examine any robot outside its prefix beyond noticing it.
		if !harbor.OwnsRobot(j.Config.ClusterName, robot.Name) {
			continue
		}

		// Layer 2: description-tag check (ADR-0009 defense-in-depth).
		// Catches the prefix-collision class where a robot's name happens
		// to be in our prefix but its description marks it as another
		// cluster's. We do NOT touch such robots.
		if !RobotBelongsToCluster(robot.Description, j.Config.ClusterName) {
			logger.V(1).Info("skipping prefix-match robot whose description belongs to another cluster",
				"robot", robot.Name, "description", robot.Description)
			continue
		}

		haNS, haName, ok := ParseRobotDescription(robot.Description)
		if !ok {
			// Our prefix + our cluster tag, but no harboraccess= token.
			// Probably an older-format robot from before this bridge
			// version, or human-edited. Skip rather than guess.
			logger.Info("owned-prefix robot has unparseable description; skipping",
				"robot", robot.Name, "description", robot.Description)
			continue
		}

		ha := &harborv1alpha1.HarborAccess{}
		err := j.Client.Get(ctx, types.NamespacedName{Namespace: haNS, Name: haName}, ha)
		switch {
		case apierrors.IsNotFound(err):
			logger.Info("deleting orphan robot (owning HarborAccess gone)",
				"robot", robot.Name, "id", robot.ID, "harboraccess", haNS+"/"+haName)
			if delErr := j.Harbor.Delete(ctx, robot.ID); delErr != nil {
				logger.Error(delErr, "failed to delete orphan robot", "robot", robot.Name)
			}
		case err != nil:
			logger.Error(err, "failed to check HarborAccess existence",
				"harboraccess", haNS+"/"+haName, "robot", robot.Name)
		}
	}

	return nil
}
