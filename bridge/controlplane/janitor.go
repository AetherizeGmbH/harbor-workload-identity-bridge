// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
	// HarborAccess lookups. Each sweep lists robots and robot Secrets
	// first and then lists the owners live, so it never judges a robot or
	// a Secret against a spec older than the one it was created from: an
	// informer cache lagging behind a serviceAccountRef edit would
	// otherwise make the janitor delete the robot the reconciler just
	// created for the new SA. Before each deletion the owner is read live
	// once more. Falls back to Client when nil (unit tests).
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
// separately from Start for testability.
//
// Robots and robot Secrets are listed first, the owners after them, in
// one uncached List: every robot and Secret is judged against an owner
// state no older than itself, at the cost of one List instead of a read
// per object. A deletion re-reads its owner live right before it, so a
// change after the List that makes the object current again still saves
// it. Per-object failures are logged and skipped so one bad object
// cannot stall the sweep. The Secret sweep needs no Harbor and runs while
// Harbor is unreachable; releasing finalizers does not, since it must
// know that no robot is left. A failure to list robots or owners is
// returned.
func (j *Janitor) Sweep(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("janitor")
	robots, robotsErr := j.Harbor.List(ctx)
	secrets, secretsErr := j.listRobotSecrets(ctx)
	owners, err := j.listOwners(ctx)
	if err != nil {
		return fmt.Errorf("list HarborAccess objects: %w", err)
	}
	if robotsErr == nil {
		j.sweepRobots(ctx, robots, owners)
	}
	if secretsErr == nil {
		j.sweepSecrets(ctx, secrets, owners)
	} else {
		logger.Error(secretsErr, "failed to list robot Secrets")
	}
	if robotsErr != nil {
		return fmt.Errorf("list robots: %w", robotsErr)
	}
	j.releaseUnselected(ctx, owners)
	return nil
}

// listOwners returns every HarborAccess, read uncached, by key.
func (j *Janitor) listOwners(ctx context.Context) (map[types.NamespacedName]*harborv1alpha1.HarborAccess, error) {
	var list harborv1alpha1.HarborAccessList
	if err := j.reader().List(ctx, &list); err != nil {
		return nil, err
	}
	owners := make(map[types.NamespacedName]*harborv1alpha1.HarborAccess, len(list.Items))
	for i := range list.Items {
		ha := &list.Items[i]
		owners[types.NamespacedName{Namespace: ha.Namespace, Name: ha.Name}] = ha
	}
	return owners, nil
}

// liveOwner reads one HarborAccess uncached; nil when it does not exist.
func (j *Janitor) liveOwner(ctx context.Context, key types.NamespacedName) (*harborv1alpha1.HarborAccess, error) {
	ha := &harborv1alpha1.HarborAccess{}
	err := j.reader().Get(ctx, key, ha)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return ha, nil
}

// sweepRobots deletes robots nobody should hold anymore.
func (j *Janitor) sweepRobots(ctx context.Context, robots []harbor.Robot, owners map[types.NamespacedName]*harborv1alpha1.HarborAccess) {
	logger := log.FromContext(ctx).WithName("janitor")
	cluster := j.Config.ClusterName

	for i := range robots {
		robot := &robots[i]

		// Layer 1: ownership prefix (ADR-0009 invariant), current or
		// pre-ADR-0018. A bridge MUST NOT touch any robot outside it.
		legacy := harbor.OwnsLegacyRobot(cluster, robot.Name)
		if !harbor.OwnsRobot(cluster, robot.Name) && !legacy {
			if _, _, err := misnamedRobot(cluster, robot); err != nil {
				// Ours by description, not by name: never touched, and its
				// owner keeps its finalizer (releaseUnselected).
				logger.Error(err, "skipping robot the bridge cannot recognise by name", "robot", robot.WireName)
			}
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

		owner := types.NamespacedName{Namespace: haNS, Name: haName}
		if j.robotVerdict(robot, legacy, owners[owner]) == "" {
			continue
		}
		live, err := j.liveOwner(ctx, owner)
		if err != nil {
			logger.Error(err, "failed to check HarborAccess existence",
				"harboraccess", owner.String(), "robot", robot.WireName)
			continue
		}
		if why := j.robotVerdict(robot, legacy, live); why != "" {
			j.deleteRobot(ctx, robot, why, owner.String())
		}
	}
}

// robotVerdict says why robot must go given its owner (nil = gone), or ""
// when the owner still uses it.
func (j *Janitor) robotVerdict(robot *harbor.Robot, legacy bool, owner *harborv1alpha1.HarborAccess) string {
	switch {
	case owner == nil:
		return "owning HarborAccess is gone"
	case !j.Config.Selects(owner):
		// ADR-0026: the owner moved to another bridge or out of every
		// bridge's scope; this bridge's robot for it must go.
		return "owning HarborAccess is no longer selected by this bridge"
	case legacy:
		// The owner exists but a legacy name is never what the current
		// bridge uses for it: the reconciler created (or will create) the
		// dot-named robot. The legacy robot's password is still valid, so
		// it must go.
		return "pre-ADR-0018 robot superseded by the dot-named robot"
	}
	want, err := harbor.RobotName(j.Config.ClusterName, owner.Spec.ServiceAccountRef.Namespace, owner.Spec.ServiceAccountRef.Name)
	switch {
	case errors.Is(err, harbor.ErrInvalidRobotName):
		// The owner's serviceAccountRef maps to no name Harbor accepts
		// (the reconciler reports InvalidSpec), so none of its robots is
		// current: this one belongs to a previous identity.
		return "owning HarborAccess's serviceAccountRef maps to no robot name Harbor accepts"
	case err != nil || want == robot.Name:
		return ""
	}
	// The owner's serviceAccountRef now maps to a different robot: this
	// one belongs to the previous identity. Normally the reconciler
	// already revoked it; this covers a failed cleanup.
	return "owning HarborAccess now uses robot " + want
}

func (j *Janitor) deleteRobot(ctx context.Context, robot *harbor.Robot, why, owner string) {
	logger := log.FromContext(ctx).WithName("janitor")
	logger.Info("deleting robot", "robot", robot.WireName, "id", robot.ID, "harboraccess", owner, "reason", why)
	if err := j.Harbor.Delete(ctx, robot.ID); err != nil {
		logger.Error(err, "failed to delete robot", "robot", robot.WireName)
	}
}

// releaseUnselected removes this bridge's finalizers from HarborAccess
// objects it no longer selects (ADR-0026) once no robot of theirs is left
// in Harbor: the per-instance finalizer, and the shared one when the
// object was served by this bridge (see servedHere), which covers objects
// that stopped matching when this bridge gained its selector. Without a
// selector every object is selected and there is nothing to release.
//
// "No robot left" is read from Harbor again, after the owner List: a
// robot the robot sweep kept (its owner was still selected in an earlier
// read), or one the reconciler created after the first robot listing,
// would otherwise survive the finalizer that promises its revocation. The
// finalizer is released with an optimistic lock on the listed version, so
// an object relabelled back since is left alone.
func (j *Janitor) releaseUnselected(ctx context.Context, owners map[types.NamespacedName]*harborv1alpha1.HarborAccess) {
	if j.Config.Finalizer() == FinalizerName {
		return
	}
	logger := log.FromContext(ctx).WithName("janitor")
	type candidate struct {
		ha      *harborv1alpha1.HarborAccess
		release []string
	}
	var candidates []candidate
	for _, ha := range owners {
		if j.Config.Selects(ha) {
			continue
		}
		var release []string
		for _, f := range j.Config.ReleasedFinalizers() {
			if controllerutil.ContainsFinalizer(ha, f) && (f != FinalizerName || j.servedHere(ha)) {
				release = append(release, f)
			}
		}
		if len(release) > 0 {
			candidates = append(candidates, candidate{ha, release})
		}
	}
	if len(candidates) == 0 {
		return
	}
	robots, err := j.Harbor.List(ctx)
	if err != nil {
		logger.Error(err, "failed to list robots before releasing HarborAccess objects no longer selected")
		return
	}
	for _, c := range candidates {
		ha, key := c.ha, types.NamespacedName{Namespace: c.ha.Namespace, Name: c.ha.Name}
		if slices.ContainsFunc(robots, func(r harbor.Robot) bool {
			return robotOwnedBy(j.Config.ClusterName, &r, ha.Namespace, ha.Name)
		}) {
			logger.Info("keeping the finalizer of a HarborAccess no longer selected until its robot is revoked", "harboraccess", key.String())
			continue
		}
		// A robot ours by description but not by name (see misnamedRobot)
		// is never deleted, so its owner keeps the finalizer that promises
		// its revocation; sweepRobots logs the robot.
		if slices.ContainsFunc(robots, func(r harbor.Robot) bool {
			ns, name, err := misnamedRobot(j.Config.ClusterName, &r)
			return err != nil && ns == ha.Namespace && name == ha.Name
		}) {
			logger.Info("keeping the finalizer of a HarborAccess no longer selected: a robot of it has a name the bridge cannot recognise",
				"harboraccess", key.String())
			continue
		}
		patch := client.MergeFromWithOptions(ha.DeepCopy(), client.MergeFromWithOptimisticLock{})
		for _, f := range c.release {
			controllerutil.RemoveFinalizer(ha, f)
		}
		if err := j.Client.Patch(ctx, ha, patch); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to release HarborAccess no longer selected", "harboraccess", key.String())
			continue
		}
		logger.Info("released HarborAccess no longer selected by this bridge", "harboraccess", key.String(), "finalizers", c.release)
	}
}

// servedHere reports whether the robot recorded in ha's status is one of
// this cluster's: this bridge served ha last, so a shared finalizer on it
// is this bridge's, set before it had a selector (ADR-0026 point 3). A
// bridge that served ha since records its own robot there instead.
func (j *Janitor) servedHere(ha *harborv1alpha1.HarborAccess) bool {
	return ha.Status.Robot != nil && j.Config.statusRobotIsOurs(ha)
}

// listRobotSecrets lists this cluster's robot Secrets (cached).
func (j *Janitor) listRobotSecrets(ctx context.Context) ([]corev1.Secret, error) {
	var secrets corev1.SecretList
	if err := j.Client.List(ctx, &secrets,
		client.InNamespace(j.Config.Namespace),
		client.MatchingLabels{
			robotsecret.LabelManagedBy: robotsecret.LabelManagedByValue,
			robotsecret.LabelCluster:   j.Config.ClusterName,
		}); err != nil {
		return nil, err
	}
	return secrets.Items, nil
}

// sweepSecrets deletes robot Secrets of this cluster that no HarborAccess
// of this bridge reads: its HarborAccess is gone (e.g. its finalizer was
// removed by hand) or no longer selected, or the Secret is not at the name
// its HarborAccess uses (robotsecret.Name; e.g. a dash-named Secret written
// by 0.2.x, whose robot the robot sweep revokes). The robot they point at
// is deleted by sweepRobots; the Secret would otherwise linger with a dead
// password forever. Deleting a Secret revokes nothing: the data plane
// only ever reads robotsecret.Name of a HarborAccess it matched.
func (j *Janitor) sweepSecrets(ctx context.Context, secrets []corev1.Secret, owners map[types.NamespacedName]*harborv1alpha1.HarborAccess) {
	logger := log.FromContext(ctx).WithName("janitor")
	for i := range secrets {
		s := &secrets[i]
		haNS, haName, ok := robotsecret.Owner(s)
		if !ok {
			continue
		}
		owner := types.NamespacedName{Namespace: haNS, Name: haName}
		if j.secretVerdict(s, owners[owner]) == "" {
			continue
		}
		live, err := j.liveOwner(ctx, owner)
		if err != nil {
			logger.Error(err, "failed to check HarborAccess existence", "harboraccess", owner.String(), "secret", s.Name)
			continue
		}
		why := j.secretVerdict(s, live)
		if why == "" {
			continue
		}
		logger.Info("deleting orphan robot Secret", "reason", why, "secret", s.Name, "harboraccess", owner.String())
		if err := j.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID}); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to delete orphan robot Secret", "secret", s.Name)
		}
	}
}

// secretVerdict says why the robot Secret s must go given the HarborAccess
// its labels name (nil = gone), or "" when that HarborAccess reads it.
func (j *Janitor) secretVerdict(s *corev1.Secret, owner *harborv1alpha1.HarborAccess) string {
	switch {
	case owner == nil:
		return "owning HarborAccess gone"
	case !j.Config.Selects(owner):
		return "owning HarborAccess no longer selected by this bridge"
	case s.Name != robotsecret.Name(owner.Namespace, owner.Name):
		return "not at the name its HarborAccess uses (" + robotsecret.Name(owner.Namespace, owner.Name) + ")"
	}
	return ""
}
