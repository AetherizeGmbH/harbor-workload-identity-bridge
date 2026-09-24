// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// Condition reasons used by the reconciler. Constants so status assertions
// are stable across versions.
const (
	ReasonReconcileSucceeded = "ReconcileSucceeded"
	ReasonIssuerMismatch     = "IssuerMismatch"
	ReasonRobotConflict      = "RobotConflict"
	ReasonRobotDisabled      = "RobotDisabled"
	ReasonInvalidSpec        = "InvalidSpec"
	ReasonAudienceMismatch   = "AudienceMismatch"
	ReasonHarborError        = "HarborError"
	ReasonDeletionBlocked    = "DeletionBlocked"
	ReasonEnforcedByBridge   = "EnforcedByBridge"
)

// Reconciler reconciles HarborAccess CRs into persistent Harbor robots
// scoped to this bridge's cluster (ADRs 0003, 0009, 0023). The robot's
// password is consumed by the data plane as Basic Auth (ADR-0013); the
// reconciler keeps the per-CR Secret holding it in sync.
//
// The reconcile is level-triggered: every pass reads the robot back from
// Harbor and converges it to the spec (permissions, description, expiry),
// instead of acting only when metadata.generation changes. That is what
// makes permission revocations reliable, repairs out-of-band drift, and
// lets a failed update be retried without ever reporting Ready early.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Harbor is the Harbor API client wrapper. The interface form lets tests
	// inject a mock without standing up an httptest server.
	Harbor harbor.Client

	// Config is the bridge runtime config. ClusterName, OIDCIssuer, and
	// Namespace are read on every reconcile; mutation between restarts is
	// undefined (Config is loaded once at startup, see config.go).
	Config *Config

	// Clock is the time source. Defaults to RealClock if unset.
	Clock Clock
}

// SetupWithManager registers the reconciler with the given manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = RealClock{}
	}
	return builder.ControllerManagedBy(mgr).
		Named("harboraccess").
		For(&harborv1alpha1.HarborAccess{}).
		// The robot Secret lives in the bridge namespace while its
		// HarborAccess may live anywhere, and cross-namespace owner
		// references are invalid — so Owns() would never map an event.
		// The Secret's ownership labels carry the mapping instead: a
		// deleted or tampered Secret is rebuilt on its own event rather
		// than at the next resync.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToHarborAccess)).
		Complete(r)
}

// secretToHarborAccess maps a bridge-managed robot Secret of this cluster
// to the HarborAccess it belongs to.
func (r *Reconciler) secretToHarborAccess(_ context.Context, obj client.Object) []reconcile.Request {
	s, ok := obj.(*corev1.Secret)
	if !ok || s.Namespace != r.Config.Namespace || s.Labels[robotsecret.LabelCluster] != r.Config.ClusterName {
		return nil
	}
	ns, name, ok := robotsecret.Owner(s)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}}
}

// +kubebuilder:rbac:groups=harbor.aetherize.io,resources=harboraccesses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=harbor.aetherize.io,resources=harboraccesses/status,verbs=update;patch
// +kubebuilder:rbac:groups=harbor.aetherize.io,resources=harboraccesses/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is the entry point controller-runtime calls per HarborAccess event.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("harboraccess", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	ha := &harborv1alpha1.HarborAccess{}
	if err := r.Get(ctx, req.NamespacedName, ha); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The cache holds only selected objects (ADR-0026); this guards a
	// stale cache entry right after a label change. The janitor releases
	// objects that stopped matching.
	if !r.Config.Selects(ha) {
		return ctrl.Result{}, nil
	}

	if !ha.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, ha)
	}

	finalizer := r.Config.Finalizer()
	if !controllerutil.ContainsFinalizer(ha, finalizer) {
		// Optimistic-lock merge patch: a JSON merge patch replaces the
		// whole finalizers list, so without the resourceVersion guard a
		// finalizer another controller added concurrently could be lost.
		patch := client.MergeFromWithOptions(ha.DeepCopy(), client.MergeFromWithOptimisticLock{})
		controllerutil.AddFinalizer(ha, finalizer)
		if err := r.Patch(ctx, ha, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
	}

	return r.reconcileNormal(ctx, ha)
}

func (r *Reconciler) reconcileNormal(ctx context.Context, ha *harborv1alpha1.HarborAccess) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	cluster := r.Config.ClusterName

	// 1. Issuer match — refuse early if the CR was applied to the wrong cluster.
	if ha.Spec.TrustPolicy.Issuer != r.Config.OIDCIssuer.String() {
		return r.markNotReady(ctx, ha, ReasonIssuerMismatch,
			fmt.Sprintf("CR trustPolicy.issuer %q does not match cluster issuer %q",
				ha.Spec.TrustPolicy.Issuer, r.Config.OIDCIssuer.String()))
	}

	// The bridge serves one audience (ADR-0026). Another audience would
	// make every token of that ServiceAccount minted for it, e.g. the
	// apiserver's default audience, redeemable for Harbor credentials.
	if ha.Spec.TrustPolicy.Audience != r.Config.Audience {
		return r.markNotReady(ctx, ha, ReasonAudienceMismatch,
			fmt.Sprintf("CR trustPolicy.audience %q is not the audience this bridge serves (%q)",
				ha.Spec.TrustPolicy.Audience, r.Config.Audience))
	}

	// 2. The HarborAccess name becomes a label value on the robot Secret.
	// The CRD rejects longer names; this guards objects admitted before
	// that rule existed, which could otherwise never create their Secret.
	if len(ha.Name) > HarborAccessNameMaxLen {
		return r.markNotReady(ctx, ha, ReasonInvalidSpec, fmt.Sprintf(
			"metadata.name is %d characters; HarborAccess names must be at most %d (the name is a label value on the robot Secret) — recreate the CR under a shorter name",
			len(ha.Name), HarborAccessNameMaxLen))
	}

	// The CRD enforces Harbor's project-name rule; this guards objects
	// admitted before it existed. "*" would grant every project.
	for _, p := range ha.Spec.Permissions {
		if err := harbor.ValidateProjectName(p.Project); err != nil {
			return r.markNotReady(ctx, ha, ReasonInvalidSpec, "spec.permissions: "+err.Error())
		}
	}

	// 3. Compute desired robot identity.
	robotName, err := harbor.RobotName(cluster, ha.Spec.ServiceAccountRef.Namespace, ha.Spec.ServiceAccountRef.Name)
	if err != nil {
		return r.markNotReady(ctx, ha, ReasonInvalidSpec, err.Error())
	}

	// 4. Defensive invariant (ADR-0009): name must be in our ownership prefix.
	// Always true by construction, but a programming error here would let us
	// reach into another cluster's robots.
	if !harbor.OwnsRobot(cluster, robotName) {
		return ctrl.Result{}, fmt.Errorf(
			"ADR-0009 invariant violation: robot name %q is not in cluster %q's ownership prefix",
			robotName, cluster)
	}

	// 5. Secret-name collision guard (audit F2). The Secret name is
	// dot-joined ("robot-<haNs>.<haName>", ADR-0018) and therefore
	// injective; this stays as defense-in-depth: if two CRs ever collapsed
	// to one Secret name, overwriting it would let one workload's SA read
	// the other's robot password. Refuse rather than overwrite; first owner
	// wins.
	secret, err := r.getRobotSecret(ctx, ha)
	if err != nil {
		return r.markTransientError(ctx, ha, fmt.Errorf("read robot Secret: %w", err))
	}
	if secret != nil && robotsecret.StampedForOther(secret, ha.Namespace, ha.Name) {
		return r.markNotReady(ctx, ha, ReasonRobotConflict, fmt.Sprintf(
			"robot-password Secret %q is already owned by a different HarborAccess (naming collision); refusing to overwrite",
			robotsecret.Name(ha.Namespace, ha.Name)))
	}

	desiredDescription := RobotDescription(cluster, ha.Namespace, ha.Name)
	desiredPerms := toHarborPerms(ha.Spec.Permissions)

	// 6. Find or create the robot.
	robot, created, res, err := r.ensureRobot(ctx, ha, robotName, desiredDescription, desiredPerms)
	if robot == nil {
		return res, err
	}

	// 7. Converge the robot to the spec. Level-triggered: this compares
	// against what Harbor actually holds, so a permission change is
	// applied (and retried) until it sticks, and drift made in the Harbor
	// UI is reverted. The password is NOT rotated for a spec change —
	// Harbor applies the new grants to the existing robot, and a rotation
	// would invalidate every password kubelet still has cached.
	if !created && (!harbor.PermissionsMatch(robot, desiredPerms) ||
		robot.Description != desiredDescription ||
		robot.ExpiresAt != harborNeverExpires) {
		logger.Info("updating Harbor robot to match spec", "robot", robotName)
		if err := r.Harbor.Update(ctx, robot, desiredDescription, desiredPerms); err != nil {
			return r.markTransientError(ctx, ha, fmt.Errorf("update robot: %w", err))
		}
	}

	// 8. Password. A freshly created robot comes with one. Otherwise rotate
	// when the schedule says so, or when the stored password cannot be
	// valid for this robot (Secret missing/incomplete, or the robot was
	// re-created under the same name).
	now := r.Clock.Now()
	password, rotatedAt := "", (*time.Time)(nil)
	if created {
		password, rotatedAt = robot.Secret, &now
	} else if why := rotationReason(ha, secret, robot, now); why != "" {
		logger.Info("rotating Harbor robot password", "robot", robotName, "reason", why)
		newSecret, err := r.Harbor.RefreshSecret(ctx, robot.ID)
		if err != nil {
			return r.markTransientError(ctx, ha, fmt.Errorf("refresh secret: %w", err))
		}
		password, rotatedAt = newSecret, &now
	}
	notBefore := rotationNotBefore(ha, secret, now, rotatedAt)
	if err := r.writeRobotSecret(ctx, ha, secret, robot, password, notBefore); err != nil {
		return r.markTransientError(ctx, ha, err)
	}

	// 9. Revoke robots this HarborAccess no longer uses: the robot of a
	// previous serviceAccountRef, or a pre-ADR-0018 dash-named one. Runs
	// only when something changed (spec edit, new robot, replaced robot)
	// because it pages through every robot in Harbor; the janitor sweeps
	// the same class periodically as a backstop.
	if created || ha.Status.ObservedGeneration != ha.Generation ||
		(ha.Status.Robot != nil && ha.Status.Robot.ID != 0 && ha.Status.Robot.ID != robot.ID) {
		if err := r.deleteStaleRobots(ctx, ha, robot.ID); err != nil {
			return r.markTransientError(ctx, ha, err)
		}
	}

	// 10. A robot an administrator disabled in Harbor authenticates
	// nothing; report it instead of claiming Ready. The flag is left alone
	// (Update echoes it) — re-enabling is the administrator's decision.
	if robot.Disabled {
		return r.markNotReadyWithRequeue(ctx, ha, ReasonRobotDisabled, fmt.Sprintf(
			"Harbor robot %q is disabled in Harbor; image pulls through this HarborAccess fail until an administrator re-enables it", robot.WireName))
	}

	return r.markReady(ctx, ha, robot, rotatedAt, r.requeueAfter(ha, notBefore, now))
}

// harborNeverExpires is Harbor's expires_at for a robot created with
// duration -1 (the only duration the bridge uses).
const harborNeverExpires = -1

// ensureRobot returns the robot for robotName, creating it when absent.
// created reports whether it was created on this pass (and therefore
// carries a password). A nil robot means status was already written and
// (res, err) must be returned as-is.
func (r *Reconciler) ensureRobot(
	ctx context.Context, ha *harborv1alpha1.HarborAccess,
	robotName, description string, perms []harbor.ProjectPermission,
) (robot *harbor.Robot, created bool, res ctrl.Result, err error) {
	logger := log.FromContext(ctx)

	existing, err := r.Harbor.GetByName(ctx, robotName)
	switch {
	case errors.Is(err, harbor.ErrRobotNotFound):
		logger.Info("creating Harbor robot", "name", robotName)
		robot, err := r.Harbor.Create(ctx, robotName, description, perms)
		if err == nil {
			return robot, true, ctrl.Result{}, nil
		}
		if !errors.Is(err, harbor.ErrRobotAlreadyExists) {
			res, err := r.markTransientError(ctx, ha, fmt.Errorf("create robot: %w", err))
			return nil, false, res, err
		}
		// Harbor returned 409: the robot exists although the lookup just
		// said it did not. Causes: (a) a previous pass created it and
		// crashed before observing success; (b) an operator hand-created a
		// robot with our deterministic name; (c) Harbor's listing was
		// briefly inconsistent. The recovery is the same for all three:
		// re-fetch and run the adoption checks. The rest of the pass then
		// converges permissions and rotates the password (the stored
		// Secret, if any, cannot be trusted for this robot).
		logger.Info("Harbor returned 409 on create; recovering existing robot", "name", robotName)
		existing, err = r.Harbor.GetByName(ctx, robotName)
		if err != nil {
			res, err := r.markTransientError(ctx, ha, fmt.Errorf("recover after create 409: lookup robot: %w", err))
			return nil, false, res, err
		}
	case err != nil:
		res, err := r.markTransientError(ctx, ha, fmt.Errorf("lookup robot: %w", err))
		return nil, false, res, err
	}

	// Adoption discipline (ADR-0009): refuse to manage a robot whose
	// description does not mark it as this cluster's. Catches the
	// prefix-collision class even when the name prefix matches.
	if !RobotBelongsToCluster(existing.Description, r.Config.ClusterName) {
		res, err := r.markNotReady(ctx, ha, ReasonRobotConflict, fmt.Sprintf(
			"Harbor robot %q exists but its description does not mark it as belonging to cluster %q; refusing to adopt",
			robotName, r.Config.ClusterName))
		return nil, false, res, err
	}
	// Robot-name collision guard (audit F2). The dot-joined name is
	// injective except on the hash-truncation overflow path; if the robot
	// names a different HarborAccess, adopting it would let two CRs fight
	// over one robot (permission overwrite, and a rotation that breaks the
	// other's stored password). Refuse.
	if descNS, descName, ok := ParseRobotDescription(existing.Description); ok && (descNS != ha.Namespace || descName != ha.Name) {
		res, err := r.markNotReady(ctx, ha, ReasonRobotConflict, fmt.Sprintf(
			"Harbor robot %q belongs to HarborAccess %s/%s, not %s/%s (robot-name collision); refusing to adopt",
			robotName, descNS, descName, ha.Namespace, ha.Name))
		return nil, false, res, err
	}
	return existing, false, ctrl.Result{}, nil
}

// rotationReason returns why the stored password must be replaced now, or
// "" when it may stay. secret is the current robot Secret (nil = absent).
func rotationReason(ha *harborv1alpha1.HarborAccess, secret *corev1.Secret, robot *harbor.Robot, now time.Time) string {
	if secret == nil {
		// The in-Harbor password is opaque and cannot be recovered, so a
		// missing Secret (crash between create and write, or an operator
		// deleting it) can only be rebuilt by rotating.
		return "robot Secret missing"
	}
	username, _, err := robotsecret.Credentials(secret)
	if err != nil {
		return "robot Secret incomplete"
	}
	if username != robot.WireName {
		return "robot Secret holds the credentials of another robot"
	}
	if id, ok := robotsecret.RobotID(secret); ok {
		if id != robot.ID {
			return "robot was re-created in Harbor"
		}
	} else if ha.Status.Robot != nil && ha.Status.Robot.ID != 0 && ha.Status.Robot.ID != robot.ID {
		return "robot was re-created in Harbor"
	}
	if nb, ok := robotsecret.RotationNotBefore(secret); ok {
		if !now.Before(nb.Add(RotationSafetyMargin)) {
			return "scheduled rotation"
		}
		return ""
	}
	// Secret written by a bridge that predates the annotation: fall back
	// to the status record of the last rotation.
	if ha.Status.Robot == nil || ha.Status.Robot.LastRotated == nil {
		return "no record of the last rotation"
	}
	if !now.Before(ha.Status.Robot.LastRotated.Add(PasswordRotationInterval + RotationSafetyMargin)) {
		return "scheduled rotation"
	}
	return ""
}

// rotationNotBefore is the instant before which the current password will
// not be rotated (ADR-0023). After a rotation it is a full interval out;
// otherwise it is the existing promise, backfilled from the status record
// for Secrets written by older bridges.
func rotationNotBefore(ha *harborv1alpha1.HarborAccess, secret *corev1.Secret, now time.Time, rotatedAt *time.Time) time.Time {
	if rotatedAt != nil {
		return rotatedAt.Add(PasswordRotationInterval)
	}
	if secret != nil {
		if nb, ok := robotsecret.RotationNotBefore(secret); ok {
			return nb
		}
	}
	if ha.Status.Robot != nil && ha.Status.Robot.LastRotated != nil {
		return ha.Status.Robot.LastRotated.Add(PasswordRotationInterval)
	}
	// Unreachable in practice (rotationReason forces a rotation when there
	// is no record); a zero promise makes the data plane disable caching.
	return now
}

// requeueAfter schedules the next pass: at the rotation instant (plus the
// safety margin), but no later than ResyncInterval so out-of-band drift is
// noticed. A per-object offset derived from the UID spreads the resyncs of
// many HarborAccess objects instead of hitting Harbor in lockstep.
func (r *Reconciler) requeueAfter(ha *harborv1alpha1.HarborAccess, notBefore, now time.Time) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(ha.UID))
	jitter := time.Duration(h.Sum32()%uint32(ResyncInterval/10/time.Second)) * time.Second
	next := ResyncInterval - jitter
	if untilRotation := notBefore.Add(RotationSafetyMargin).Sub(now); untilRotation < next {
		next = untilRotation
	}
	if next < time.Second {
		next = time.Second
	}
	return next
}

// deleteStaleRobots deletes every robot owned by this HarborAccess except
// keepID. Ownership requires all three robotOwnedBy layers, so a foreign
// or another HarborAccess's robot is never touched.
func (r *Reconciler) deleteStaleRobots(ctx context.Context, ha *harborv1alpha1.HarborAccess, keepID int64) error {
	robots, err := r.Harbor.List(ctx)
	if err != nil {
		return fmt.Errorf("list robots for stale-robot cleanup: %w", err)
	}
	for i := range robots {
		robot := &robots[i]
		if robot.ID == keepID || !robotOwnedBy(r.Config.ClusterName, robot, ha.Namespace, ha.Name) {
			continue
		}
		if err := r.Harbor.Delete(ctx, robot.ID); err != nil {
			return fmt.Errorf("delete stale robot %q: %w", robot.WireName, err)
		}
		log.FromContext(ctx).Info("deleted stale Harbor robot no longer used by this HarborAccess",
			"robot", robot.WireName, "id", robot.ID)
	}
	return nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, ha *harborv1alpha1.HarborAccess) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// With a selector the bridge owns its per-instance finalizer, and also
	// the shared one, which only this bridge's pre-selector installation
	// can have set (a CR can be deleted before the instance finalizer was
	// added).
	finalizers := []string{r.Config.Finalizer()}
	if r.Config.Finalizer() != FinalizerName {
		finalizers = append(finalizers, FinalizerName)
	}
	held := false
	for _, f := range finalizers {
		held = held || controllerutil.ContainsFinalizer(ha, f)
	}
	if !held {
		return ctrl.Result{}, nil
	}

	// Delete EVERY robot this HarborAccess owns — the current one and any
	// left over from an earlier serviceAccountRef or the pre-ADR-0018
	// naming — not just the one the current spec maps to. Deletion is the
	// revocation; a robot that survives it keeps a valid password.
	robots, err := r.Harbor.List(ctx)
	if err != nil {
		return r.blockDeletion(ctx, ha, fmt.Errorf("list robots: %w", err))
	}
	for i := range robots {
		robot := &robots[i]
		if !robotOwnedBy(r.Config.ClusterName, robot, ha.Namespace, ha.Name) {
			continue
		}
		if err := r.Harbor.Delete(ctx, robot.ID); err != nil {
			return r.blockDeletion(ctx, ha, fmt.Errorf("delete robot %q: %w", robot.WireName, err))
		}
		logger.Info("deleted Harbor robot", "robot", robot.WireName, "id", robot.ID)
	}

	// Delete the password Secret, unless it is stamped for a different
	// HarborAccess (collision backstop — never delete another CR's Secret).
	secret, err := r.getRobotSecret(ctx, ha)
	if err != nil {
		return r.blockDeletion(ctx, ha, fmt.Errorf("read robot Secret: %w", err))
	}
	if secret != nil && !robotsecret.StampedForOther(secret, ha.Namespace, ha.Name) {
		if err := r.Delete(ctx, secret, client.Preconditions{UID: &secret.UID}); err != nil && !apierrors.IsNotFound(err) {
			return r.blockDeletion(ctx, ha, fmt.Errorf("delete robot Secret: %w", err))
		}
	}

	patch := client.MergeFromWithOptions(ha.DeepCopy(), client.MergeFromWithOptimisticLock{})
	for _, f := range finalizers {
		controllerutil.RemoveFinalizer(ha, f)
	}
	if err := r.Patch(ctx, ha, patch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(fmt.Errorf("remove finalizer: %w", err))
	}
	return ctrl.Result{}, nil
}

// blockDeletion records why the finalizer cannot be released yet and
// returns cause so controller-runtime retries with backoff. The status
// write is best-effort: the object is being deleted, and surfacing the
// Harbor error matters more than the write succeeding.
func (r *Reconciler) blockDeletion(ctx context.Context, ha *harborv1alpha1.HarborAccess, cause error) (ctrl.Result, error) {
	msg := fmt.Sprintf("cannot revoke the Harbor robot yet, deletion is waiting: %v. "+
		"If Harbor is gone for good, remove the %q finalizer by hand; the janitor deletes the orphaned robot once Harbor is reachable",
		cause, r.Config.Finalizer())
	if err := r.updateReadyCondition(ctx, ha, metav1.ConditionFalse, ReasonDeletionBlocked, msg); err != nil {
		log.FromContext(ctx).V(1).Info("could not record deletion-blocked status", "err", err.Error())
	}
	return ctrl.Result{}, cause
}

// getRobotSecret returns the robot Secret of ha, or nil when it does not
// exist.
func (r *Reconciler) getRobotSecret(ctx context.Context, ha *harborv1alpha1.HarborAccess) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: robotsecret.Name(ha.Namespace, ha.Name)}, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return s, nil
}

// writeRobotSecret converges the robot Secret. password is the new
// password to store, or "" to keep the stored one (then only labels and
// annotations are converged — e.g. backfilling the rotation promise onto
// a Secret written by an older bridge). The Secret lives in the bridge
// namespace so workload SAs cannot read it (ADR-0011).
func (r *Reconciler) writeRobotSecret(
	ctx context.Context, ha *harborv1alpha1.HarborAccess, existing *corev1.Secret,
	robot *harbor.Robot, password string, notBefore time.Time,
) error {
	labels := robotsecret.Labels(r.Config.ClusterName, ha.Namespace, ha.Name)
	annotations := map[string]string{
		robotsecret.AnnotationRobotID:           robotsecret.FormatRobotID(robot.ID),
		robotsecret.AnnotationRotationNotBefore: robotsecret.FormatRotationNotBefore(notBefore),
	}

	if existing == nil {
		if password == "" {
			// Unreachable: a missing Secret always forces a rotation.
			return errors.New("robot Secret is missing and no password is available to rebuild it")
		}
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        robotsecret.Name(ha.Namespace, ha.Name),
				Namespace:   r.Config.Namespace,
				Labels:      labels,
				Annotations: annotations,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				robotsecret.KeyUsername: []byte(robot.WireName),
				robotsecret.KeyPassword: []byte(password),
			},
		}
		if err := r.Create(ctx, s); err != nil {
			return fmt.Errorf("create robot Secret: %w", err)
		}
		return nil
	}

	updated := existing.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	for k, v := range labels {
		updated.Labels[k] = v
	}
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		updated.Annotations[k] = v
	}
	if password != "" {
		updated.Data = map[string][]byte{
			robotsecret.KeyUsername: []byte(robot.WireName),
			robotsecret.KeyPassword: []byte(password),
		}
	}
	if equalStringMaps(updated.Labels, existing.Labels) &&
		equalStringMaps(updated.Annotations, existing.Annotations) && password == "" {
		return nil
	}
	if err := r.Update(ctx, updated); err != nil {
		return fmt.Errorf("update robot Secret: %w", err)
	}
	return nil
}

func equalStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func (r *Reconciler) markReady(
	ctx context.Context, ha *harborv1alpha1.HarborAccess, robot *harbor.Robot,
	rotatedAt *time.Time, requeueAfter time.Duration,
) (ctrl.Result, error) {
	if ha.Status.Robot == nil {
		ha.Status.Robot = &harborv1alpha1.RobotRef{}
	}
	ha.Status.Robot.Name = robot.WireName
	ha.Status.Robot.ID = robot.ID
	ha.Status.Robot.PasswordSecretRef = robotsecret.Name(ha.Namespace, ha.Name)
	if rotatedAt != nil {
		// LastRotated advances only when we actually wrote a new password.
		t := metav1.NewTime(*rotatedAt)
		ha.Status.Robot.LastRotated = &t
	}
	ha.Status.TrustPolicyEnforcedBy = harborv1alpha1.EnforcedByBridge
	// observedGeneration advances ONLY here: it means "this generation is
	// fully applied". Error paths leave it behind so the next pass still
	// knows the spec changed (e.g. to run the stale-robot cleanup).
	ha.Status.ObservedGeneration = ha.Generation

	meta.SetStatusCondition(&ha.Status.Conditions, metav1.Condition{
		Type:               harborv1alpha1.ConditionRobotProvisioned,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonReconcileSucceeded,
		Message:            fmt.Sprintf("Harbor robot %q (id=%d) provisioned", robot.WireName, robot.ID),
		ObservedGeneration: ha.Generation,
	})
	meta.SetStatusCondition(&ha.Status.Conditions, metav1.Condition{
		Type:               harborv1alpha1.ConditionTrustPolicyApplied,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonEnforcedByBridge,
		Message:            "trust policy enforced by bridge data plane until upstream Harbor lands goharbor/harbor#17520",
		ObservedGeneration: ha.Generation,
	})
	meta.SetStatusCondition(&ha.Status.Conditions, metav1.Condition{
		Type:               harborv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonReconcileSucceeded,
		Message:            "Harbor robot exists with desired permissions",
		ObservedGeneration: ha.Generation,
	})
	if err := r.Status().Update(ctx, ha); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// markNotReady writes Ready=False for terminal (operator-error) failures
// that retrying cannot fix: issuer mismatch, invalid spec, foreign robot
// conflict. Returns nil so controller-runtime treats the reconcile as
// successful and waits for the next CR event.
func (r *Reconciler) markNotReady(ctx context.Context, ha *harborv1alpha1.HarborAccess, reason, message string) (ctrl.Result, error) {
	if err := r.updateReadyCondition(ctx, ha, metav1.ConditionFalse, reason, message); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}
	return ctrl.Result{}, nil
}

// markNotReadyWithRequeue is markNotReady for conditions that change
// outside Kubernetes (a robot disabled in Harbor): no CR event announces
// their resolution, so the object is re-checked on the resync interval.
func (r *Reconciler) markNotReadyWithRequeue(ctx context.Context, ha *harborv1alpha1.HarborAccess, reason, message string) (ctrl.Result, error) {
	if _, err := r.markNotReady(ctx, ha, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: ResyncInterval}, nil
}

// markTransientError writes Ready=False AND returns the cause as the
// reconcile error so controller-runtime retries with exponential backoff.
// Use for Harbor API failures, network errors, and any condition where a
// later attempt could plausibly succeed without operator intervention.
func (r *Reconciler) markTransientError(ctx context.Context, ha *harborv1alpha1.HarborAccess, cause error) (ctrl.Result, error) {
	if err := r.updateReadyCondition(ctx, ha, metav1.ConditionFalse, ReasonHarborError, cause.Error()); err != nil {
		// Status update itself failed; surface that error instead of cause
		// so the controller manager logs the real blocker.
		return ctrl.Result{}, fmt.Errorf("update status while reporting transient error %q: %w", cause.Error(), err)
	}
	return ctrl.Result{}, cause
}

// updateReadyCondition sets the Ready condition. It deliberately does not
// touch status.observedGeneration (see markReady).
func (r *Reconciler) updateReadyCondition(
	ctx context.Context,
	ha *harborv1alpha1.HarborAccess,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	meta.SetStatusCondition(&ha.Status.Conditions, metav1.Condition{
		Type:               harborv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ha.Generation,
	})
	return r.Status().Update(ctx, ha)
}

// toHarborPerms converts the CRD permission shape to the Harbor wrapper's
// permission shape. Action is copied verbatim — the wrapper normalizes and
// expands the comma form ("pull,push") into two Access entries.
func toHarborPerms(in []harborv1alpha1.ProjectPermission) []harbor.ProjectPermission {
	out := make([]harbor.ProjectPermission, len(in))
	for i, p := range in {
		out[i] = harbor.ProjectPermission{
			Project: p.Project,
			Action:  string(p.Action),
		}
	}
	return out
}
