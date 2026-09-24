// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// DefaultSweepInterval is how often the Janitor walks Harbor for orphan
// robots. Five minutes is short enough to clean up after a HarborAccess
// whose finalizer was removed by hand (reconciliation never fires on a
// deleted CR), and long enough not to spam Harbor.
const DefaultSweepInterval = 5 * time.Minute

// Janitor periodically revokes robots this bridge created that no
// HarborAccess uses any more, and removes robot Secrets whose HarborAccess
// is gone. It is the backstop for everything the reconciler cannot see:
// finalizers removed by hand, a bridge that was down while a CR was
// deleted, a serviceAccountRef change whose cleanup failed, and robots
// left behind by pre-ADR-0018 bridges (ADR-0023).
//
// Janitor implements sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
// It does not implement LeaderElectionRunnable, so it runs on the leader
// only — exactly one replica deletes.
type Janitor struct {
	// Client reads robot Secrets (cached, bridge namespace only) and
	// deletes orphaned ones.
	Client client.Client

	// Reader must be an UNCACHED reader (mgr.GetAPIReader()) for
	// HarborAccess lookups. The janitor lists robots first and then reads
	// each owner live, so it can never act on a spec older than the one
	// the robot was created from: an informer cache lagging behind a
	// serviceAccountRef edit would otherwise make the janitor delete the
	// robot the reconciler just created for the new SA. Falls back to
	// Client when nil (unit tests).
	Reader client.Reader

	Harbor harbor.Client
	Config *Config

	// SweepInterval is how often Start runs a sweep. Defaults to
	// DefaultSweepInterval when zero; tests set it directly.
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

func (j *Janitor) reader() client.Reader {
	if j.Reader != nil {
		return j.Reader
	}
	return j.Client
}

// Sweep performs one pass over Harbor robots and bridge Secrets. Exposed
// separately from Start for testability. Per-object failures are logged
// and skipped so one bad robot cannot stall the sweep; only a failure to
// list robots at all is returned.
func (j *Janitor) Sweep(ctx context.Context) error {
	if err := j.sweepRobots(ctx); err != nil {
		return err
	}
	j.sweepSecrets(ctx)
	return nil
}

func (j *Janitor) sweepRobots(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("janitor")
	cluster := j.Config.ClusterName

	robots, err := j.Harbor.List(ctx)
	if err != nil {
		return fmt.Errorf("list robots: %w", err)
	}

	for i := range robots {
		robot := &robots[i]

		// Layer 1: ownership prefix (ADR-0009 invariant), current or
		// pre-ADR-0018. A bridge MUST NOT touch any robot outside it.
		legacy := harbor.OwnsLegacyRobot(cluster, robot.Name)
		if !harbor.OwnsRobot(cluster, robot.Name) && !legacy {
			continue
		}

		// Layer 2: description-tag check (ADR-0009 defense-in-depth).
		// Catches the prefix-collision class — and it is the ONLY check
		// separating this cluster's legacy robots from those of a cluster
		// whose name extends ours with "-…".
		if !RobotBelongsToCluster(robot.Description, cluster) {
			logger.V(1).Info("skipping prefix-match robot whose description belongs to another cluster",
				"robot", robot.WireName, "description", robot.Description)
			continue
		}

		haNS, haName, ok := ParseRobotDescription(robot.Description)
		if !ok {
			// Our prefix + our cluster tag, but no harboraccess= token:
			// human-edited or of unknown provenance. Skip rather than guess.
			logger.Info("owned-prefix robot has unparseable description; skipping",
				"robot", robot.WireName, "description", robot.Description)
			continue
		}

		ha := &harborv1alpha1.HarborAccess{}
		err := j.reader().Get(ctx, types.NamespacedName{Namespace: haNS, Name: haName}, ha)
		switch {
		case apierrors.IsNotFound(err):
			j.deleteRobot(ctx, robot, "owning HarborAccess is gone", haNS+"/"+haName)
		case err != nil:
			logger.Error(err, "failed to check HarborAccess existence",
				"harboraccess", haNS+"/"+haName, "robot", robot.WireName)
		case legacy:
			// The owner exists but a legacy name is never what the current
			// bridge uses for it: the reconciler created (or will create)
			// the dot-named robot. The legacy robot's password is still
			// valid, so it must go.
			j.deleteRobot(ctx, robot, "pre-ADR-0018 robot superseded by the dot-named robot", haNS+"/"+haName)
		default:
			want, err := harbor.RobotName(cluster, ha.Spec.ServiceAccountRef.Namespace, ha.Spec.ServiceAccountRef.Name)
			if err != nil || want == robot.Name {
				continue
			}
			// The owner's serviceAccountRef now maps to a different robot:
			// this one belongs to the previous identity. Normally the
			// reconciler already revoked it; this covers a failed cleanup.
			j.deleteRobot(ctx, robot, "owning HarborAccess now uses robot "+want, haNS+"/"+haName)
		}
	}
	return nil
}

func (j *Janitor) deleteRobot(ctx context.Context, robot *harbor.Robot, why, owner string) {
	logger := log.FromContext(ctx).WithName("janitor")
	logger.Info("deleting robot", "robot", robot.WireName, "id", robot.ID, "harboraccess", owner, "reason", why)
	if err := j.Harbor.Delete(ctx, robot.ID); err != nil {
		logger.Error(err, "failed to delete robot", "robot", robot.WireName)
	}
}

// sweepSecrets deletes robot Secrets of this cluster whose HarborAccess no
// longer exists (e.g. its finalizer was removed by hand). The robot they
// point at is deleted by sweepRobots; the Secret would otherwise linger
// with a dead password forever.
func (j *Janitor) sweepSecrets(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("janitor")

	var secrets corev1.SecretList
	if err := j.Client.List(ctx, &secrets,
		client.InNamespace(j.Config.Namespace),
		client.MatchingLabels{
			robotsecret.LabelManagedBy: robotsecret.LabelManagedByValue,
			robotsecret.LabelCluster:   j.Config.ClusterName,
		}); err != nil {
		logger.Error(err, "failed to list robot Secrets")
		return
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		haNS, haName, ok := robotsecret.Owner(s)
		if !ok {
			continue
		}
		err := j.reader().Get(ctx, types.NamespacedName{Namespace: haNS, Name: haName}, &harborv1alpha1.HarborAccess{})
		switch {
		case apierrors.IsNotFound(err):
			logger.Info("deleting orphan robot Secret (owning HarborAccess gone)",
				"secret", s.Name, "harboraccess", haNS+"/"+haName)
			if err := j.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID}); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "failed to delete orphan robot Secret", "secret", s.Name)
			}
		case err != nil:
			logger.Error(err, "failed to check HarborAccess existence", "harboraccess", haNS+"/"+haName, "secret", s.Name)
		}
	}
}
