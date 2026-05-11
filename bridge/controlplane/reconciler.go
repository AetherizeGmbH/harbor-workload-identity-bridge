// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
)

// Condition reasons used by the reconciler. Constants so status assertions
// are stable across versions.
const (
	ReasonReconcileSucceeded = "ReconcileSucceeded"
	ReasonIssuerMismatch     = "IssuerMismatch"
	ReasonRobotConflict      = "RobotConflict"
	ReasonInvalidSpec        = "InvalidSpec"
	ReasonHarborError        = "HarborError"
	ReasonEnforcedByBridge   = "EnforcedByBridge"
)

// Reconciler reconciles HarborAccess CRs into persistent Harbor robots
// scoped to this bridge's cluster (ADRs 0003, 0005, 0009).
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
		Owns(&corev1.Secret{}).
		Complete(r)
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

	if !ha.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, ha)
	}

	if !controllerutil.ContainsFinalizer(ha, FinalizerName) {
		controllerutil.AddFinalizer(ha, FinalizerName)
		if err := r.Update(ctx, ha); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return r.reconcileNormal(ctx, ha)
}

func (r *Reconciler) reconcileNormal(ctx context.Context, ha *harborv1alpha1.HarborAccess) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Issuer match — refuse early if the CR was applied to the wrong cluster.
	if ha.Spec.TrustPolicy.Issuer != r.Config.OIDCIssuer.String() {
		return r.markNotReady(ctx, ha, ReasonIssuerMismatch,
			fmt.Sprintf("CR trustPolicy.issuer %q does not match cluster issuer %q",
				ha.Spec.TrustPolicy.Issuer, r.Config.OIDCIssuer.String()))
	}

	// 2. Compute desired robot identity.
	robotName, err := harbor.RobotName(
		r.Config.ClusterName,
		ha.Spec.ServiceAccountRef.Namespace,
		ha.Spec.ServiceAccountRef.Name,
	)
	if err != nil {
		return r.markNotReady(ctx, ha, ReasonInvalidSpec, err.Error())
	}

	// 3. Defensive invariant (ADR-0009): name must be in our ownership prefix.
	// Always true by construction, but a programming error here would let us
	// reach into another cluster's robots.
	if !harbor.OwnsRobot(r.Config.ClusterName, robotName) {
		return ctrl.Result{}, fmt.Errorf(
			"ADR-0009 invariant violation: robot name %q is not in cluster %q's ownership prefix",
			robotName, r.Config.ClusterName,
		)
	}

	desiredDescription := RobotDescription(r.Config.ClusterName, ha.Namespace, ha.Name)
	desiredPerms := toHarborPerms(ha.Spec.Permissions)

	// 4. Look up existing robot.
	existing, err := r.Harbor.GetByName(ctx, robotName)
	switch {
	case errors.Is(err, harbor.ErrRobotNotFound):
		return r.createAndStatus(ctx, ha, robotName, desiredDescription, desiredPerms)
	case err != nil:
		return r.markTransientError(ctx, ha, ReasonHarborError, fmt.Errorf("lookup robot: %w", err))
	}

	// 5. Adoption discipline: refuse to manage a robot whose description does
	// not include cluster=<our cluster>. Catches the prefix-collision class
	// from ADR-0009 even when the name prefix matches.
	if !RobotBelongsToCluster(existing.Description, r.Config.ClusterName) {
		return r.markNotReady(ctx, ha, ReasonRobotConflict, fmt.Sprintf(
			"Harbor robot %q exists but its description does not mark it as belonging to cluster %q; refusing to adopt",
			robotName, r.Config.ClusterName,
		))
	}

	// 6. Check whether the password Secret is missing in k8s. If so we must
	// rotate to obtain a fresh password; the in-Harbor password is opaque
	// and cannot be recovered. This covers two real scenarios:
	//   (a) the bridge crashed between Harbor.Create and writeRobotSecret on
	//       a previous run, leaving the robot but no Secret;
	//   (b) an operator (or a misbehaving controller) deleted the Secret.
	// Without this check, the reconciler would happily mark Ready=True while
	// the data plane has no credentials to authenticate with.
	secretMissing, err := r.secretMissing(ctx, ha)
	if err != nil {
		return r.markTransientError(ctx, ha, ReasonHarborError, fmt.Errorf("check robot Secret: %w", err))
	}

	// 7. Permission update on generation change.
	generationChanged := ha.Status.ObservedGeneration != ha.Generation
	if generationChanged {
		logger.Info("updating Harbor robot permissions", "robot", robotName, "generation", ha.Generation)
		if err := r.Harbor.UpdatePermissions(ctx, existing.ID, desiredDescription, desiredPerms); err != nil {
			return r.markTransientError(ctx, ha, ReasonHarborError, fmt.Errorf("update permissions: %w", err))
		}
	}

	// 8. Password rotation: on generation change, when the stored password
	// exceeds the rotation interval, or when the k8s Secret is missing
	// (force-rebuild path).
	if generationChanged || r.passwordIsStale(ha) || secretMissing {
		if secretMissing {
			logger.Info("password Secret missing; forcing rotation to rebuild it", "robot", robotName)
		} else {
			logger.Info("rotating Harbor robot secret", "robot", robotName)
		}
		newSecret, err := r.Harbor.RefreshSecret(ctx, existing.ID)
		if err != nil {
			return r.markTransientError(ctx, ha, ReasonHarborError, fmt.Errorf("refresh secret: %w", err))
		}
		existing.Secret = newSecret
		if err := r.writeRobotSecret(ctx, ha, existing); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.markReady(ctx, ha, existing)
}

func (r *Reconciler) createAndStatus(
	ctx context.Context, ha *harborv1alpha1.HarborAccess,
	name, description string, perms []harbor.ProjectPermission,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("creating Harbor robot", "name", name)
	robot, err := r.Harbor.Create(ctx, name, description, perms)
	if err != nil {
		return r.markTransientError(ctx, ha, ReasonHarborError, fmt.Errorf("create robot: %w", err))
	}
	if err := r.writeRobotSecret(ctx, ha, robot); err != nil {
		return ctrl.Result{}, err
	}
	return r.markReady(ctx, ha, robot)
}

// secretMissing reports whether the per-CR robot-password Secret is absent
// from the bridge namespace. Returns an error only on non-NotFound API
// failures so the caller can surface them.
func (r *Reconciler) secretMissing(ctx context.Context, ha *harborv1alpha1.HarborAccess) (bool, error) {
	err := r.Get(ctx,
		client.ObjectKey{Namespace: r.Config.Namespace, Name: r.secretNameFor(ha)},
		&corev1.Secret{},
	)
	switch {
	case apierrors.IsNotFound(err):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

func (r *Reconciler) reconcileDelete(ctx context.Context, ha *harborv1alpha1.HarborAccess) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(ha, FinalizerName) {
		return ctrl.Result{}, nil
	}

	// Best-effort delete of the Harbor robot. If the robot is already gone,
	// or never existed (issuer-mismatch path skipped creation), we still drop
	// the finalizer so the CR can be removed.
	if name, err := harbor.RobotName(
		r.Config.ClusterName,
		ha.Spec.ServiceAccountRef.Namespace,
		ha.Spec.ServiceAccountRef.Name,
	); err == nil && harbor.OwnsRobot(r.Config.ClusterName, name) {
		existing, err := r.Harbor.GetByName(ctx, name)
		switch {
		case errors.Is(err, harbor.ErrRobotNotFound):
			// nothing to do
		case err != nil:
			return ctrl.Result{}, fmt.Errorf("lookup robot for delete: %w", err)
		case !RobotBelongsToCluster(existing.Description, r.Config.ClusterName):
			// Defense-in-depth: refuse to delete a robot whose description
			// does not match our cluster (ADR-0009 prefix-collision class).
			logger.Info("skipping delete of robot whose description does not match this cluster",
				"robot", name, "description", existing.Description)
		default:
			if err := r.Harbor.Delete(ctx, existing.ID); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete robot %d: %w", existing.ID, err)
			}
			logger.Info("deleted Harbor robot", "name", name, "id", existing.ID)
		}
	}

	// Delete the password Secret if present.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.secretNameFor(ha),
			Namespace: r.Config.Namespace,
		},
	}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete password Secret: %w", err)
	}

	controllerutil.RemoveFinalizer(ha, FinalizerName)
	if err := r.Update(ctx, ha); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// writeRobotSecret upserts the per-CR Secret holding the robot username and
// password. Lives in the bridge namespace so workload SAs cannot read it.
// Skipped silently when robot.Secret is empty (i.e. we found an existing
// robot via GetByName, which does not return a secret).
func (r *Reconciler) writeRobotSecret(ctx context.Context, ha *harborv1alpha1.HarborAccess, robot *harbor.Robot) error {
	if robot.Secret == "" {
		return nil
	}
	name := r.secretNameFor(ha)
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Config.Namespace,
			Labels:    r.secretLabels(ha),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"username": []byte(robot.Name),
			"password": []byte(robot.Secret),
		},
	}
	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: r.Config.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create robot Secret: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get robot Secret: %w", err)
	}
	existing.Data = desired.Data
	existing.Labels = desired.Labels
	if err := r.Update(ctx, existing); err != nil {
		return fmt.Errorf("update robot Secret: %w", err)
	}
	return nil
}

func (r *Reconciler) secretNameFor(ha *harborv1alpha1.HarborAccess) string {
	// We do not hash-truncate here yet: the longest reasonable HarborAccess
	// namespace+name combination fits well under Kubernetes' 253-char name
	// limit. If we ever encounter a real CR that overflows, mirror the
	// truncation pattern from harbor/naming.go.
	return SecretNamePrefix + ha.Namespace + "-" + ha.Name
}

func (r *Reconciler) secretLabels(ha *harborv1alpha1.HarborAccess) map[string]string {
	return map[string]string{
		LabelManagedBy:             LabelManagedByValue,
		LabelCluster:               r.Config.ClusterName,
		LabelHarborAccessNamespace: ha.Namespace,
		LabelHarborAccessName:      ha.Name,
	}
}

func (r *Reconciler) passwordIsStale(ha *harborv1alpha1.HarborAccess) bool {
	if ha.Status.Robot == nil || ha.Status.Robot.LastRotated == nil {
		return true
	}
	age := r.Clock.Now().Sub(ha.Status.Robot.LastRotated.Time)
	return age > PasswordRotationInterval
}

func (r *Reconciler) markReady(ctx context.Context, ha *harborv1alpha1.HarborAccess, robot *harbor.Robot) (ctrl.Result, error) {
	now := metav1.NewTime(r.Clock.Now())

	if ha.Status.Robot == nil {
		ha.Status.Robot = &harborv1alpha1.RobotRef{}
	}
	ha.Status.Robot.Name = robot.Name
	ha.Status.Robot.ID = robot.ID
	ha.Status.Robot.PasswordSecretRef = r.secretNameFor(ha)
	if robot.Secret != "" {
		// LastRotated advances only when we actually wrote a new password.
		ha.Status.Robot.LastRotated = &now
	}
	ha.Status.TrustPolicyEnforcedBy = harborv1alpha1.EnforcedByBridge
	ha.Status.ObservedGeneration = ha.Generation

	meta.SetStatusCondition(&ha.Status.Conditions, metav1.Condition{
		Type:               harborv1alpha1.ConditionRobotProvisioned,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonReconcileSucceeded,
		Message:            fmt.Sprintf("Harbor robot %q (id=%d) provisioned", robot.Name, robot.ID),
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
	return ctrl.Result{}, nil
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

// markTransientError writes Ready=False AND returns the cause as the
// reconcile error so controller-runtime retries with exponential backoff.
// Use for Harbor API failures, network errors, and any condition where a
// later attempt could plausibly succeed without operator intervention.
func (r *Reconciler) markTransientError(ctx context.Context, ha *harborv1alpha1.HarborAccess, reason string, cause error) (ctrl.Result, error) {
	if err := r.updateReadyCondition(ctx, ha, metav1.ConditionFalse, reason, cause.Error()); err != nil {
		// Status update itself failed; surface that error instead of cause
		// so the controller manager logs the real blocker.
		return ctrl.Result{}, fmt.Errorf("update status while reporting transient error %q: %w", cause.Error(), err)
	}
	return ctrl.Result{}, cause
}

// updateReadyCondition is the shared helper for setting the Ready condition
// without returning a reconcile result. Used by both markNotReady (terminal)
// and markTransientError (retryable) so the status-update logic stays in
// one place.
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
	ha.Status.ObservedGeneration = ha.Generation
	return r.Status().Update(ctx, ha)
}

// toHarborPerms converts the CRD permission shape to the Harbor wrapper's
// permission shape. Action is copied verbatim — the wrapper expands the
// comma form ("pull,push") into two Access entries internally.
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
