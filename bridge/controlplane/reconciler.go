// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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

	// APIReader reads the robot Secret uncached. Rotating a password in
	// Harbor cannot be undone, so the decision to rotate, and the write
	// that stores the new password, must see the Secret as it is: an
	// informer cache that has not yet caught up with the previous pass's
	// write would make the pass rotate again and fail to store the result,
	// leaving a password Harbor rejects in a Secret that looks current.
	// SetupWithManager sets mgr.GetAPIReader() when nil; tests that call
	// Reconcile directly may leave it nil, which falls back to Client.
	APIReader client.Reader

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
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return builder.ControllerManagedBy(mgr).
		Named("harboraccess").
		For(&harborv1alpha1.HarborAccess{}).
		// The robot Secret lives in the bridge namespace while its
		// HarborAccess may live anywhere, and cross-namespace owner
		// references are invalid — so Owns() would never map an event.
		// The Secret's ownership labels and its name carry the mapping
		// instead: a deleted or tampered Secret is rebuilt on its own
		// event rather than at the next resync.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToHarborAccess)).
		// A HarborAccess that shares its serviceAccountRef with a deleted
		// one reported RobotConflict while the deleted one owned the
		// robot. The deletion released the robot (the finalizer revokes
		// it before the object goes), so re-check the survivors now.
		Watches(&harborv1alpha1.HarborAccess{},
			handler.EnqueueRequestsFromMapFunc(r.sameServiceAccount),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(event.CreateEvent) bool { return false },
				UpdateFunc:  func(event.UpdateEvent) bool { return false },
				DeleteFunc:  func(event.DeleteEvent) bool { return true },
				GenericFunc: func(event.GenericEvent) bool { return false },
			})).
		Complete(r)
}

// secretToHarborAccess maps a Secret in the bridge namespace to the
// HarborAccess objects it concerns: the one its bridge labels name (this
// cluster's Secrets only), and the one whose robot-Secret name it
// occupies. The second mapping is what lets a HarborAccess in
// RobotConflict notice at once that the Secret blocking it was renamed or
// deleted, which is what its condition asks for; such a Secret carries no
// bridge labels, or labels naming another HarborAccess.
func (r *Reconciler) secretToHarborAccess(_ context.Context, obj client.Object) []reconcile.Request {
	s, ok := obj.(*corev1.Secret)
	if !ok || s.Namespace != r.Config.Namespace {
		return nil
	}
	var reqs []reconcile.Request
	if s.Labels[robotsecret.LabelCluster] == r.Config.ClusterName {
		if ns, name, ok := robotsecret.Owner(s); ok {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		}
	}
	if ns, name, ok := robotsecret.ParseName(s.Name); ok {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if len(reqs) == 0 || reqs[0].NamespacedName != key {
			reqs = append(reqs, reconcile.Request{NamespacedName: key})
		}
	}
	return reqs
}

// sameServiceAccount maps a HarborAccess to the other HarborAccess objects
// with the same serviceAccountRef: they map to the same robot name, so at
// most one of them owns the robot and the others report RobotConflict.
func (r *Reconciler) sameServiceAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	gone, ok := obj.(*harborv1alpha1.HarborAccess)
	if !ok {
		return nil
	}
	var list harborv1alpha1.HarborAccessList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "list HarborAccess objects sharing a serviceAccountRef",
			"harboraccess", gone.Namespace+"/"+gone.Name)
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		ha := &list.Items[i]
		if (ha.Namespace == gone.Namespace && ha.Name == gone.Name) ||
			ha.Spec.ServiceAccountRef != gone.Spec.ServiceAccountRef {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ha.Namespace, Name: ha.Name}})
	}
	return reqs
}

// The bridge's RBAC is hand-maintained in
// charts/harbor-bridge/templates/bridge-rbac.yaml: Secrets and the
// leader-election Lease in the bridge namespace only (ADR-0011),
// HarborAccess cluster-wide. Nothing generates RBAC from markers here.

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
	return r.reconcileNormal(ctx, ha)
}

// ensureFinalizer adds this bridge's finalizer. reconcileNormal calls it
// right before the robot can be created, so a robot never exists without
// the finalizer that revokes it, and a HarborAccess the bridge refuses
// never depends on this bridge (and its Harbor) to be deleted. That
// matters with several bridges whose selectors overlap (ADR-0026): each
// serves only the objects that name its audience.
func (r *Reconciler) ensureFinalizer(ctx context.Context, ha *harborv1alpha1.HarborAccess) error {
	finalizer := r.Config.Finalizer()
	if controllerutil.ContainsFinalizer(ha, finalizer) {
		return nil
	}
	// Optimistic-lock merge patch: a JSON merge patch replaces the whole
	// finalizers list, so without the resourceVersion guard a finalizer
	// another controller added concurrently could be lost.
	patch := client.MergeFromWithOptions(ha.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.AddFinalizer(ha, finalizer)
	if err := r.Patch(ctx, ha, patch); err != nil {
		return fmt.Errorf("add finalizer: %w", err)
	}
	return nil
}

func (r *Reconciler) reconcileNormal(ctx context.Context, ha *harborv1alpha1.HarborAccess) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	cluster := r.Config.ClusterName

	// Every refusal below (steps 0-3) deletes the robot Secret and
	// suspends the robot the serviceAccountRef maps to, if the HarborAccess
	// owns one (refuse, ADR-0030).
	//
	// 0. The CRD requires spec; this guards objects stored before that rule
	// existed, which would otherwise be reported as an issuer mismatch.
	if ha.Spec.ServiceAccountRef == (harborv1alpha1.ServiceAccountRef{}) &&
		ha.Spec.TrustPolicy == (harborv1alpha1.TrustPolicy{}) && len(ha.Spec.Permissions) == 0 {
		return r.refuse(ctx, ha, ReasonInvalidSpec,
			"spec is missing: a HarborAccess needs spec.serviceAccountRef, spec.trustPolicy and spec.permissions")
	}

	// 1. Issuer match — refuse early if the CR was applied to the wrong
	// cluster.
	if ha.Spec.TrustPolicy.Issuer != r.Config.OIDCIssuer.String() {
		return r.refuse(ctx, ha, ReasonIssuerMismatch,
			fmt.Sprintf("CR trustPolicy.issuer %q does not match cluster issuer %q",
				ha.Spec.TrustPolicy.Issuer, r.Config.OIDCIssuer.String()))
	}

	// The bridge serves one audience (ADR-0026). Another audience would
	// make every token of that ServiceAccount minted for it, e.g. the
	// apiserver's default audience, redeemable for Harbor credentials.
	if ha.Spec.TrustPolicy.Audience != r.Config.Audience {
		return r.refuse(ctx, ha, ReasonAudienceMismatch,
			fmt.Sprintf("CR trustPolicy.audience %q is not the audience this bridge serves (%q)",
				ha.Spec.TrustPolicy.Audience, r.Config.Audience))
	}

	// 2. The HarborAccess name becomes a label value on the robot Secret.
	// The CRD rejects longer names; this guards objects admitted before
	// that rule existed, which could otherwise never create their Secret.
	if len(ha.Name) > HarborAccessNameMaxLen {
		return r.refuse(ctx, ha, ReasonInvalidSpec, fmt.Sprintf(
			"metadata.name is %d characters; HarborAccess names must be at most %d (the name is a label value on the robot Secret) — recreate the CR under a shorter name",
			len(ha.Name), HarborAccessNameMaxLen))
	}

	// The CRD enforces Harbor's project-name rule; this guards objects
	// admitted before it existed. "*" would grant every project.
	for _, p := range ha.Spec.Permissions {
		if err := harbor.ValidateProjectName(p.Project); err != nil {
			return r.refuse(ctx, ha, ReasonInvalidSpec, "spec.permissions: "+err.Error())
		}
	}

	// The CRD admits only Go durations; this guards values such as "1d"
	// admitted before that rule existed, which decode to a zero TTL. The
	// data plane refuses such an object (403); a robot it got before the
	// value was set is suspended like that of any refused object.
	if err := ha.Spec.TokenTTL.Err(); err != nil {
		return r.refuse(ctx, ha, ReasonInvalidSpec, "spec.tokenTTL: "+err.Error())
	}

	// 3. Compute desired robot identity.
	robotName, err := harbor.RobotName(cluster, ha.Spec.ServiceAccountRef.Namespace, ha.Spec.ServiceAccountRef.Name)
	if err != nil {
		return r.refuse(ctx, ha, ReasonInvalidSpec, err.Error())
	}

	// 4. Defensive invariant (ADR-0009): name must be in our ownership prefix.
	// Always true by construction, but a programming error here would let us
	// reach into another cluster's robots.
	if !harbor.OwnsRobot(cluster, robotName) {
		return ctrl.Result{}, fmt.Errorf(
			"ADR-0009 invariant violation: robot name %q is not in cluster %q's ownership prefix",
			robotName, cluster)
	}

	// 5. Secret-name collision guard (audit F2), see secretConflict. The
	// Secret is read uncached (APIReader): the rotation decision below is
	// irreversible and must not rest on a lagging cache.
	secret, err := r.getRobotSecret(ctx, ha)
	if err != nil {
		return r.markTransientError(ctx, ha, fmt.Errorf("read robot Secret: %w", err))
	}
	if msg := r.secretConflict(ha, secret, robotName); msg != "" {
		return r.markNotReadyWithRequeue(ctx, ha, ReasonRobotConflict, msg)
	}

	if err := r.ensureFinalizer(ctx, ha); err != nil {
		return ctrl.Result{}, err
	}

	desiredDescription := RobotDescription(cluster, ha.Namespace, ha.Name)
	desiredPerms := toHarborPerms(ha.Spec.Permissions)

	// 6. Find or create the robot.
	robot, created, res, err := r.ensureRobot(ctx, ha, robotName, desiredDescription, desiredPerms)
	if robot == nil {
		return res, err
	}

	// 6b. Resume a robot the bridge suspended while this HarborAccess was
	// refused (ADR-0030). A password from before the suspension must never
	// work again, so the robot gets a new one while it is still disabled
	// (Harbor rotates a disabled robot's secret and keeps it disabled), and
	// only then is it re-enabled. Whatever step a pass fails at, the old
	// password stays dead: until the enabling update succeeds the robot is
	// disabled and carries the token, and the next pass resumes again. A
	// Secret present at this point (restored, or its deletion during the
	// suspension failed) holds a password that is dead after the rotation;
	// it goes first, so that a pass that stops after the enabling update
	// finds no Secret and rotates again instead of trusting it.
	now := r.Clock.Now()
	password, rotatedAt := "", (*time.Time)(nil)
	resumed := false
	if !created && robot.Disabled && RobotSuspended(robot.Description) {
		if secret != nil {
			if err := r.Delete(ctx, secret, client.Preconditions{UID: &secret.UID}); err != nil && !apierrors.IsNotFound(err) {
				return r.markTransientError(ctx, ha, fmt.Errorf("delete the robot Secret of a suspended robot: %w", err))
			}
			secret = nil
		}
		logger.Info("rotating the password of a suspended Harbor robot before resuming it", "robot", robotName)
		newSecret, err := r.Harbor.RefreshSecret(ctx, robot.ID)
		if err != nil {
			return r.markTransientError(ctx, ha, fmt.Errorf("rotate the password of a suspended robot: %w", err))
		}
		password, rotatedAt = newSecret, &now
		enabled := *robot
		enabled.Disabled = false
		logger.Info("resuming Harbor robot suspended while the HarborAccess was refused", "robot", robotName)
		if err := r.Harbor.Update(ctx, &enabled, desiredDescription, desiredPerms); err != nil {
			return r.markTransientError(ctx, ha, fmt.Errorf("resume suspended robot: %w", err))
		}
		robot.Disabled, robot.Description = false, desiredDescription
		resumed = true
	}

	// 7. Converge the robot to the spec. Level-triggered: this compares
	// against what Harbor actually holds, so a permission change is
	// applied (and retried) until it sticks, and drift made in the Harbor
	// UI is reverted. The password is NOT rotated for a spec change —
	// Harbor applies the new grants to the existing robot, and a rotation
	// would invalidate every password kubelet still has cached. A resumed
	// robot was converged by the update that re-enabled it.
	if !created && !resumed && (!harbor.PermissionsMatch(robot, desiredPerms) ||
		robot.Description != desiredDescription ||
		robot.ExpiresAt != harborNeverExpires) {
		logger.Info("updating Harbor robot to match spec", "robot", robotName)
		if err := r.Harbor.Update(ctx, robot, desiredDescription, desiredPerms); err != nil {
			return r.markTransientError(ctx, ha, fmt.Errorf("update robot: %w", err))
		}
	}

	// 8. Password. A freshly created robot comes with one, a resumed one
	// got one above. Otherwise rotate when the schedule says so, or when
	// the stored password cannot be valid for this robot (Secret
	// missing/incomplete, or the robot was re-created under the same name).
	if created {
		password, rotatedAt = robot.Secret, &now
	} else if why := rotationReason(ha, secret, robot, now); !resumed && why != "" {
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

// refuse reports a HarborAccess the bridge will not serve (issuer or
// audience mismatch, invalid spec) and suspends what it already owns
// (ADR-0030): a robot provisioned before the HarborAccess became refused
// would otherwise keep its grants and a password that is never rotated
// again. Refused objects are re-checked every resyncAfter, so a suspended
// robot an administrator re-enables is disabled again. A failed
// suspension keeps the refusal reason and is retried with backoff. Once
// nothing of this bridge is left, its finalizers are released (ADR-0032).
func (r *Reconciler) refuse(ctx context.Context, ha *harborv1alpha1.HarborAccess, reason, message string) (ctrl.Result, error) {
	outcome, releasable, suspendErr := r.suspend(ctx, ha)
	if suspendErr != nil {
		message += fmt.Sprintf("; suspending its robot failed and is retried: %v", suspendErr)
		outcome = "suspending the robot failed and is retried: " + suspendErr.Error()
	}
	meta.SetStatusCondition(&ha.Status.Conditions, metav1.Condition{
		Type:               harborv1alpha1.ConditionRobotProvisioned,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            "no usable robot while the HarborAccess is refused (ADR-0030): " + outcome,
		ObservedGeneration: ha.Generation,
	})
	if err := r.setNotReady(ctx, ha, reason, message); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}
	if suspendErr != nil {
		return ctrl.Result{}, suspendErr
	}
	if releasable {
		if err := r.releaseRefused(ctx, ha); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: resyncAfter(ha)}, nil
}

// suspend deletes the robot Secret of a refused HarborAccess and disables
// the robot it owns (ADR-0030). It returns what happened to the robot, for
// the RobotProvisioned condition, and whether nothing of this bridge may
// be left, so that its finalizers can go (releaseRefused).
//
// The Secret goes first and whatever Harbor answers: deleting it needs only
// the apiserver, its password is dead or about to be, and the data plane
// still matches an InvalidSpec HarborAccess and would hand the Secret out.
//
// Harbor is asked only while the object holds a finalizer this bridge may
// have set (ADR-0032). A robot never exists without one: ensureFinalizer
// runs right before the robot can be created, and older bridges added it
// before anything else. Refusing an object that never had a robot here
// (another bridge's audience, another cluster's issuer) costs no Harbor
// call.
func (r *Reconciler) suspend(ctx context.Context, ha *harborv1alpha1.HarborAccess) (outcome string, releasable bool, err error) {
	robotName, nameErr := harbor.RobotName(r.Config.ClusterName, ha.Spec.ServiceAccountRef.Namespace, ha.Spec.ServiceAccountRef.Name)
	secretErr := r.deleteOwnRobotSecret(ctx, ha, robotName)
	if !slices.ContainsFunc(r.Config.ReleasedFinalizers(), func(f string) bool { return controllerutil.ContainsFinalizer(ha, f) }) {
		return noRobotOfThisBridge, false, secretErr
	}
	if nameErr != nil {
		robotName = ""
	}
	outcome, kept, err := r.suspendRobot(ctx, ha, robotName)
	if err != nil {
		return "", false, errors.Join(secretErr, err)
	}
	return outcome, !kept && secretErr == nil, secretErr
}

// noRobotOfThisBridge is the RobotProvisioned outcome of a refused
// HarborAccess without a robot of this bridge.
const noRobotOfThisBridge = "it has no robot of this bridge"

// suspendRobot disables the robot at robotName ("" = none) if ha owns it,
// and reports whether a robot of ha is kept there (disabled). Only the
// robot its serviceAccountRef maps to can be enabled and serving; robots
// of an earlier serviceAccountRef and pre-ADR-0018 robots are the
// janitor's (it deletes them). A robot that is already disabled stays as
// it is: suspended before, or disabled by an administrator, whose decision
// the bridge never overrides.
func (r *Reconciler) suspendRobot(ctx context.Context, ha *harborv1alpha1.HarborAccess, robotName string) (outcome string, kept bool, err error) {
	logger := log.FromContext(ctx)
	cluster := r.Config.ClusterName
	if robotName == "" || !harbor.OwnsRobot(cluster, robotName) {
		return "its ServiceAccount maps to no robot name of this bridge", false, nil
	}
	robot, err := r.Harbor.GetByName(ctx, robotName)
	switch {
	case errors.Is(err, harbor.ErrRobotNotFound):
		return noRobotOfThisBridge, false, nil
	case err != nil:
		return "", false, fmt.Errorf("look up robot: %w", err)
	case !robotOwnedBy(cluster, robot, ha.Namespace, ha.Name):
		return fmt.Sprintf("Harbor robot %q is not this HarborAccess's and is left alone", robot.WireName), false, nil
	case robot.Disabled && RobotSuspended(robot.Description):
		return fmt.Sprintf("Harbor robot %q is suspended (disabled) until the HarborAccess is accepted again", robot.WireName), true, nil
	case robot.Disabled:
		return fmt.Sprintf("Harbor robot %q was disabled in Harbor by an administrator and stays disabled", robot.WireName), true, nil
	case canWriteBack(robot.Permissions):
		disabled := *robot
		disabled.Disabled = true
		if err := r.Harbor.Update(ctx, &disabled, SuspendedRobotDescription(cluster, ha.Namespace, ha.Name), robot.Permissions); err != nil {
			return "", false, fmt.Errorf("disable robot %q: %w", robot.WireName, err)
		}
		logger.Info("suspended Harbor robot of a refused HarborAccess", "robot", robot.WireName, "id", robot.ID)
		return fmt.Sprintf("Harbor robot %q is suspended (disabled) until the HarborAccess is accepted again", robot.WireName), true, nil
	default:
		// Harbor's update replaces the grants with the ones sent, and the
		// bridge never sends a project it refuses (such as a pre-0.5.5
		// "*"). A robot it cannot disable is revoked.
		if err := r.Harbor.Delete(ctx, robot.ID); err != nil {
			return "", false, fmt.Errorf("delete robot %q: %w", robot.WireName, err)
		}
		logger.Info("deleted Harbor robot of a refused HarborAccess: its grants cannot be written back to disable it",
			"robot", robot.WireName, "id", robot.ID)
		return fmt.Sprintf("Harbor robot %q was deleted: its grants cannot be written back to disable it", robot.WireName), false, nil
	}
}

// releaseRefused removes this bridge's finalizers from a refused
// HarborAccess once no robot of it is left in Harbor, so that deleting it
// no longer waits for this bridge and its Harbor (ADR-0032). Bridges
// before ADR-0032 added the finalizer to every object they selected, also
// to those they refused. A robot of an earlier serviceAccountRef or a
// pre-ADR-0018 one keeps the finalizers; the janitor deletes it, and a
// later pass releases them. If the object is accepted again,
// ensureFinalizer adds the finalizer back before any robot is created.
//
// The shared finalizer is released only by a bridge without a selector,
// and only while the object's status records no robot of another
// clusterName: a bridge without a selector that serves the object sets it
// too (overlapping selectors, ADR-0026) and would add it back at once.
// Several bridges on one cluster need selectors; the per-instance
// finalizer is this bridge's alone.
func (r *Reconciler) releaseRefused(ctx context.Context, ha *harborv1alpha1.HarborAccess) error {
	sharedIsOurs := r.Config.Finalizer() == FinalizerName && r.Config.statusRobotIsOurs(ha)
	var release []string
	for _, f := range r.Config.ReleasedFinalizers() {
		if controllerutil.ContainsFinalizer(ha, f) && (f != FinalizerName || sharedIsOurs) {
			release = append(release, f)
		}
	}
	if len(release) == 0 {
		return nil
	}
	robots, err := r.Harbor.List(ctx)
	if err != nil {
		return fmt.Errorf("list robots before releasing the finalizer of a refused HarborAccess: %w", err)
	}
	for i := range robots {
		robot := &robots[i]
		if ns, name, err := misnamedRobot(r.Config.ClusterName, robot); err != nil && ns == ha.Namespace && name == ha.Name {
			// Ours by description, not by name: the bridge can neither
			// suspend it nor take it for absent, so the finalizers stay.
			return err
		}
		if robotOwnedBy(r.Config.ClusterName, robot, ha.Namespace, ha.Name) {
			return nil
		}
	}
	patch := client.MergeFromWithOptions(ha.DeepCopy(), client.MergeFromWithOptimisticLock{})
	for _, f := range release {
		controllerutil.RemoveFinalizer(ha, f)
	}
	if err := r.Patch(ctx, ha, patch); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("release the finalizer of a refused HarborAccess: %w", err))
	}
	log.FromContext(ctx).Info("released a refused HarborAccess without a robot of this bridge", "finalizers", release)
	return nil
}

// deleteOwnRobotSecret deletes ha's robot Secret if ha owns it
// (secretDeleteConflict).
func (r *Reconciler) deleteOwnRobotSecret(ctx context.Context, ha *harborv1alpha1.HarborAccess, robotName string) error {
	secret, err := r.getRobotSecret(ctx, ha)
	if err != nil {
		return fmt.Errorf("read robot Secret: %w", err)
	}
	if secret == nil {
		return nil
	}
	if msg := r.secretDeleteConflict(ha, secret, robotName); msg != "" {
		log.FromContext(ctx).Info("keeping a Secret at the robot Secret's name that does not belong to this HarborAccess",
			"secret", secret.Name, "reason", msg)
		return nil
	}
	if err := r.Delete(ctx, secret, client.Preconditions{UID: &secret.UID}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete robot Secret: %w", err)
	}
	log.FromContext(ctx).Info("deleted robot Secret", "secret", secret.Name)
	return nil
}

// canWriteBack reports whether perms can be sent back to Harbor unchanged:
// at least one grant, and only project names the bridge writes.
func canWriteBack(perms []harbor.ProjectPermission) bool {
	for _, p := range perms {
		if harbor.ValidateProjectName(p.Project) != nil {
			return false
		}
	}
	return len(perms) > 0
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
		res, err := r.markNotReadyWithRequeue(ctx, ha, ReasonRobotConflict, fmt.Sprintf(
			"Harbor robot %q exists but its description does not mark it as belonging to cluster %q; refusing to adopt",
			robotName, r.Config.ClusterName))
		return nil, false, res, err
	}
	// Robot-name collision guard (audit F2). The robot name is derived
	// from the serviceAccountRef alone, so two HarborAccess objects for the
	// same ServiceAccount map to the same robot (and, on the hash-truncation
	// overflow path, two ServiceAccounts could). If the robot names a
	// different HarborAccess, adopting it would let two CRs fight over one
	// robot (permission overwrite, and a rotation that breaks the other's
	// stored password). Refuse; deleting the other HarborAccess releases
	// the robot and re-checks this one (sameServiceAccount).
	if descNS, descName, ok := ParseRobotDescription(existing.Description); ok && (descNS != ha.Namespace || descName != ha.Name) {
		res, err := r.markNotReadyWithRequeue(ctx, ha, ReasonRobotConflict, fmt.Sprintf(
			"Harbor robot %q (the robot of ServiceAccount %s/%s) belongs to HarborAccess %s/%s, not %s/%s; refusing to adopt. A ServiceAccount is served through one HarborAccess",
			robotName, ha.Spec.ServiceAccountRef.Namespace, ha.Spec.ServiceAccountRef.Name,
			descNS, descName, ha.Namespace, ha.Name))
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
// safety margin), but no later than resyncAfter so out-of-band drift is
// noticed.
func (r *Reconciler) requeueAfter(ha *harborv1alpha1.HarborAccess, notBefore, now time.Time) time.Duration {
	next := resyncAfter(ha)
	if untilRotation := notBefore.Add(RotationSafetyMargin).Sub(now); untilRotation < next {
		next = untilRotation
	}
	if next < time.Second {
		next = time.Second
	}
	return next
}

// resyncAfter is ResyncInterval less a per-object offset of up to a tenth
// of it, derived from the UID. Every object is reconciled when the bridge
// starts; the offset spreads their resyncs instead of hitting Harbor with
// all of them in lockstep an interval later.
func resyncAfter(ha *harborv1alpha1.HarborAccess) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(ha.UID))
	jitter := time.Duration(h.Sum32()%uint32(ResyncInterval/10/time.Second)) * time.Second
	return ResyncInterval - jitter
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
		if ns, name, err := misnamedRobot(r.Config.ClusterName, robot); err != nil && ns == ha.Namespace && name == ha.Name {
			return err
		}
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

	var held []string
	for _, f := range r.Config.ReleasedFinalizers() {
		if controllerutil.ContainsFinalizer(ha, f) {
			held = append(held, f)
		}
	}
	if len(held) == 0 {
		return ctrl.Result{}, nil
	}

	// Delete EVERY robot this HarborAccess owns — the current one and any
	// left over from an earlier serviceAccountRef or the pre-ADR-0018
	// naming — not just the one the current spec maps to. Deletion is the
	// revocation; a robot that survives it keeps a valid password.
	robots, err := r.Harbor.List(ctx)
	if err != nil {
		return r.blockDeletion(ctx, ha, held, fmt.Errorf("list robots: %w", err))
	}
	for i := range robots {
		robot := &robots[i]
		if ns, name, err := misnamedRobot(r.Config.ClusterName, robot); err != nil && ns == ha.Namespace && name == ha.Name {
			// Ours by description, not by name: it can be neither
			// deleted nor left behind.
			return r.blockDeletion(ctx, ha, held, err)
		}
		if !robotOwnedBy(r.Config.ClusterName, robot, ha.Namespace, ha.Name) {
			continue
		}
		if err := r.Harbor.Delete(ctx, robot.ID); err != nil {
			return r.blockDeletion(ctx, ha, held, fmt.Errorf("delete robot %q: %w", robot.WireName, err))
		}
		logger.Info("deleted Harbor robot", "robot", robot.WireName, "id", robot.ID)
	}

	// Delete the password Secret if it belongs to this HarborAccess
	// (secretDeleteConflict): never another CR's Secret (collision
	// backstop), never one the bridge refused to adopt, never one stamped
	// for another cluster. A Secret left behind is never served (the data
	// plane serves only Secrets stamped for the HarborAccess it matched).
	robotName, _ := harbor.RobotName(r.Config.ClusterName, ha.Spec.ServiceAccountRef.Namespace, ha.Spec.ServiceAccountRef.Name)
	if err := r.deleteOwnRobotSecret(ctx, ha, robotName); err != nil {
		return r.blockDeletion(ctx, ha, held, err)
	}

	patch := client.MergeFromWithOptions(ha.DeepCopy(), client.MergeFromWithOptimisticLock{})
	for _, f := range held {
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
// Harbor error matters more than the write succeeding. held lists this
// bridge's finalizers on ha: all of them must be removed by hand to force
// the deletion.
func (r *Reconciler) blockDeletion(ctx context.Context, ha *harborv1alpha1.HarborAccess, held []string, cause error) (ctrl.Result, error) {
	quoted := make([]string, len(held))
	for i, f := range held {
		quoted[i] = strconv.Quote(f)
	}
	what := "the " + strings.Join(quoted, " and ") + " finalizer"
	if len(held) > 1 {
		what += "s"
	}
	msg := fmt.Sprintf("cannot revoke the Harbor robot yet, deletion is waiting: %v. "+
		"If Harbor is gone for good, remove %s by hand; the janitor deletes the orphaned robot once Harbor is reachable",
		cause, what)
	if err := r.setNotReady(ctx, ha, ReasonDeletionBlocked, msg); err != nil {
		log.FromContext(ctx).V(1).Info("could not record deletion-blocked status", "err", err.Error())
	}
	return ctrl.Result{}, cause
}

// secretConflict returns why the Secret at ha's robot-Secret name belongs
// to someone else, or "" when ha may write it. secret nil means absent.
//
// The Secret name is dot-joined ("robot-<haNs>.<haName>", ADR-0018) and
// therefore injective; the stamped-for-other check stays as
// defense-in-depth (audit F2): if two CRs ever collapsed to one Secret
// name, overwriting it would let one workload's SA read the other's robot
// password. Refuse rather than overwrite; first owner wins.
//
// A Secret at that name without the bridge's labels is adopted only when
// it holds this robot's credentials (a Secret from an older bridge).
// Anything else belongs to someone else: overwriting it would destroy it,
// and the data plane would otherwise serve it.
func (r *Reconciler) secretConflict(ha *harborv1alpha1.HarborAccess, secret *corev1.Secret, robotName string) string {
	switch {
	case secret == nil:
		return ""
	case robotsecret.StampedForOther(secret, ha.Namespace, ha.Name):
		return fmt.Sprintf(
			"robot-password Secret %q is already owned by a different HarborAccess (naming collision); refusing to overwrite",
			robotsecret.Name(ha.Namespace, ha.Name))
	case !robotsecret.IsManaged(secret):
		if user, _, err := robotsecret.Credentials(secret); err != nil || user != r.Config.HarborRobotPrefix+robotName {
			return fmt.Sprintf(
				"Secret %q in %s is not managed by the bridge and does not hold this robot's credentials; refusing to adopt it. Rename or delete it",
				robotsecret.Name(ha.Namespace, ha.Name), r.Config.Namespace)
		}
	}
	return ""
}

// secretDeleteConflict returns why ha must not delete the Secret at its
// robot-Secret name, or "" when it may. On top of secretConflict, a
// managed Secret must be stamped for this cluster, the rule the janitor
// deletes by (it lists only this cluster's Secrets): a bridge with another
// clusterName that shares the namespace writes the same Secret names, and
// deleting its Secret would make it rotate and rebuild it, only for this
// bridge to delete it again on the event.
func (r *Reconciler) secretDeleteConflict(ha *harborv1alpha1.HarborAccess, secret *corev1.Secret, robotName string) string {
	if msg := r.secretConflict(ha, secret, robotName); msg != "" {
		return msg
	}
	if secret != nil && robotsecret.IsManaged(secret) && secret.Labels[robotsecret.LabelCluster] != r.Config.ClusterName {
		return fmt.Sprintf("robot-password Secret %q is stamped for cluster %q, not %q",
			secret.Name, secret.Labels[robotsecret.LabelCluster], r.Config.ClusterName)
	}
	return ""
}

// getRobotSecret returns the robot Secret of ha, read uncached, or nil when
// it does not exist.
func (r *Reconciler) getRobotSecret(ctx context.Context, ha *harborv1alpha1.HarborAccess) (*corev1.Secret, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	s := &corev1.Secret{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: robotsecret.Name(ha.Namespace, ha.Name)}, s)
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
//
// A new password is the only one Harbor still accepts. When the Secret
// changed since this pass read it (a Create that finds it created, an
// Update that finds it modified or deleted), the Secret is read again and
// the password stored on top of the current version, as long as the
// Secret still belongs to ha. Dropping the password instead would leave
// the Secret holding one Harbor rejects, and a promise not to rotate it
// for a day. Without a new password a failed write loses nothing and is
// retried on the next pass.
func (r *Reconciler) writeRobotSecret(
	ctx context.Context, ha *harborv1alpha1.HarborAccess, existing *corev1.Secret,
	robot *harbor.Robot, password string, notBefore time.Time,
) error {
	if password == "" {
		return r.putRobotSecret(ctx, ha, existing, robot, "", notBefore)
	}
	current := existing
	return retry.OnError(retry.DefaultRetry, isSecretWriteRace, func() error {
		err := r.putRobotSecret(ctx, ha, current, robot, password, notBefore)
		if !isSecretWriteRace(err) {
			return err
		}
		fresh, readErr := r.getRobotSecret(ctx, ha)
		if readErr != nil {
			return fmt.Errorf("re-read robot Secret to store the new password: %w", readErr)
		}
		if msg := r.secretConflict(ha, fresh, robot.Name); msg != "" {
			return fmt.Errorf("cannot store the new password: %s", msg)
		}
		current = fresh
		return err
	})
}

// isSecretWriteRace reports whether a Secret write failed because the
// Secret changed after it was read.
func isSecretWriteRace(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) || apierrors.IsNotFound(err)
}

// putRobotSecret creates the robot Secret (existing nil) or updates
// existing, which carries the resourceVersion the update is conditional on.
func (r *Reconciler) putRobotSecret(
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

// markNotReadyWithRequeue writes Ready=False for conditions resolved
// outside the CR: a robot disabled in Harbor, or a robot or Secret that
// belongs to someone else (RobotConflict). No event on this CR announces
// that they are gone, so the object is re-checked on the resync interval
// (resyncAfter). (Refusals go through refuse, which also requeues.)
func (r *Reconciler) markNotReadyWithRequeue(ctx context.Context, ha *harborv1alpha1.HarborAccess, reason, message string) (ctrl.Result, error) {
	if err := r.setNotReady(ctx, ha, reason, message); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}
	return ctrl.Result{RequeueAfter: resyncAfter(ha)}, nil
}

// markTransientError writes Ready=False AND returns the cause as the
// reconcile error so controller-runtime retries with exponential backoff.
// Use for Harbor API failures, network errors, and any condition where a
// later attempt could plausibly succeed without operator intervention.
func (r *Reconciler) markTransientError(ctx context.Context, ha *harborv1alpha1.HarborAccess, cause error) (ctrl.Result, error) {
	if err := r.setNotReady(ctx, ha, ReasonHarborError, cause.Error()); err != nil {
		// Status update itself failed; surface that error instead of cause
		// so the controller manager logs the real blocker.
		return ctrl.Result{}, fmt.Errorf("update status while reporting transient error %q: %w", cause.Error(), err)
	}
	return ctrl.Result{}, cause
}

// setNotReady sets Ready=False. It deliberately does not touch
// status.observedGeneration (see markReady).
func (r *Reconciler) setNotReady(ctx context.Context, ha *harborv1alpha1.HarborAccess, reason, message string) error {
	meta.SetStatusCondition(&ha.Status.Conditions, metav1.Condition{
		Type:               harborv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
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
