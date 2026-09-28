// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/nexussecret"
)

// Condition reasons of the Nexus reconciler (ADR-0036 decision a). The
// refusal reasons (IssuerMismatch, AudienceMismatch, InvalidSpec),
// DeletionBlocked, ReconcileSucceeded and EnforcedByBridge are shared with
// the Harbor reconciler.
const (
	ReasonUserConflict       = "UserConflict"
	ReasonRoleConflict       = "RoleConflict"
	ReasonUserDisabled       = "UserDisabled"
	ReasonRepositoryNotFound = "RepositoryNotFound"
	ReasonPrivilegeConflict  = "PrivilegeConflict"
	ReasonPasswordRejected   = "PasswordRejected"
	ReasonNexusError         = "NexusError"
	ReasonNexusAuthFailed    = "NexusAuthFailed"
	ReasonNexusRateLimited   = "NexusRateLimited"
)

// NexusUserRetireGrace is how long the user a rotation replaced outlives
// the Secret write that replaced it. A scheduled rotation happens only
// after the old password's rotation-not-before, so no kubelet cache holds
// it any more (ADR-0023); the grace covers a pull that fetched the old
// credentials just before and still uses the old user's docker bearer
// token, and a data-plane replica whose cache has not yet seen the new
// Secret. It extends the lifetime of a leaked bearer token by as much.
const NexusUserRetireGrace = 5 * time.Minute

// NexusRepositoryRecheckInterval bounds how long a NexusAccess waits in
// RepositoryNotFound before the reconciler checks the repositories again.
// Nexus sends no event when a repository is created, and ResyncInterval
// would keep an object without credentials for up to an hour after the
// repository appeared. Each check costs a few calls, and, because the
// generation stays unobserved, a listing of the cluster's users and roles.
const NexusRepositoryRecheckInterval = 5 * time.Minute

// NexusReconciler reconciles NexusAccess objects into one Nexus role and
// one Nexus local user per ServiceAccount identity (ADR-0036). Like the
// Harbor reconciler it is level-triggered: every pass reads the role, the
// privileges the spec names and the users back from Nexus and converges
// them, so revocations are reliable and out-of-band drift is repaired.
//
// A password is never changed: Nexus keeps a user's docker bearer token
// valid across a password change and across a delete and re-create under
// the same id (ADR-0036 Context 1). A rotation creates the identity's user
// under a new generation, stores its password, and retires the previous
// user once NexusUserRetireGrace has passed.
type NexusReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads the Secret uncached: the decision to create a user
	// and the write that stores its password must see the Secret as it is
	// (see Reconciler.APIReader). SetupWithManager sets
	// mgr.GetAPIReader() when nil; nil falls back to Client.
	APIReader client.Reader

	// Nexus is the Nexus client, wrapped by Backoff.Wrap in production.
	Nexus nexus.Client

	// Backoff is the rate-limit backoff shared with the janitor; the
	// reconciler asks it how long to wait. nil waits the default window.
	Backoff *NexusBackoff

	Config *Config

	// Clock defaults to RealClock.
	Clock Clock
}

// SetupWithManager registers the reconciler with the given manager.
func (r *NexusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clock == nil {
		r.Clock = RealClock{}
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return builder.ControllerManagedBy(mgr).
		Named("nexusaccess").
		For(&nexusv1alpha1.NexusAccess{}).
		// The Secret lives in the bridge namespace; its labels and its name
		// map it back (see Reconciler.SetupWithManager).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToNexusAccess)).
		// A NexusAccess that shares its serviceAccountRef with a deleted one
		// reported RoleConflict while the deleted one owned the role.
		Watches(&nexusv1alpha1.NexusAccess{},
			handler.EnqueueRequestsFromMapFunc(r.sameServiceAccount),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(event.CreateEvent) bool { return false },
				UpdateFunc:  func(event.UpdateEvent) bool { return false },
				DeleteFunc:  func(event.DeleteEvent) bool { return true },
				GenericFunc: func(event.GenericEvent) bool { return false },
			})).
		Complete(r)
}

// secretToNexusAccess maps a Secret in the bridge namespace to the
// NexusAccess its labels name (this cluster's Secrets only) and to the one
// whose Secret name it occupies.
func (r *NexusReconciler) secretToNexusAccess(_ context.Context, obj client.Object) []reconcile.Request {
	s, ok := obj.(*corev1.Secret)
	if !ok || s.Namespace != r.Config.Namespace {
		return nil
	}
	var reqs []reconcile.Request
	if s.Labels[nexussecret.LabelCluster] == r.Config.ClusterName {
		if ns, name, ok := nexussecret.Owner(s); ok {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		}
	}
	if ns, name, ok := nexussecret.ParseName(s.Name); ok {
		key := types.NamespacedName{Namespace: ns, Name: name}
		if len(reqs) == 0 || reqs[0].NamespacedName != key {
			reqs = append(reqs, reconcile.Request{NamespacedName: key})
		}
	}
	return reqs
}

// sameServiceAccount maps a NexusAccess to the others with the same
// serviceAccountRef: they map to the same role, which at most one owns.
func (r *NexusReconciler) sameServiceAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	gone, ok := obj.(*nexusv1alpha1.NexusAccess)
	if !ok {
		return nil
	}
	var list nexusv1alpha1.NexusAccessList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "list NexusAccess objects sharing a serviceAccountRef",
			"nexusaccess", gone.Namespace+"/"+gone.Name)
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		nxa := &list.Items[i]
		if (nxa.Namespace == gone.Namespace && nxa.Name == gone.Name) ||
			nxa.Spec.ServiceAccountRef != gone.Spec.ServiceAccountRef {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: nxa.Namespace, Name: nxa.Name}})
	}
	return reqs
}

// Reconcile is the entry point controller-runtime calls per NexusAccess
// event.
func (r *NexusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("nexusaccess", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	nxa := &nexusv1alpha1.NexusAccess{}
	if err := r.Get(ctx, req.NamespacedName, nxa); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The cache holds only selected objects (ADR-0026); this guards a
	// stale cache entry right after a label change. The janitor releases
	// objects that stopped matching.
	if !r.Config.Selects(nxa) {
		return ctrl.Result{}, nil
	}
	if !nxa.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, nxa)
	}
	return r.reconcileNormal(ctx, nxa)
}

func (r *NexusReconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}
	return r.Clock.Now()
}

// nexusGrant is one repository of the spec with the privileges its access
// needs.
type nexusGrant struct {
	format     nexus.Format
	repository string
	privileges []string
}

// nexusGrants maps spec.repositories to privileges. An error means the
// spec is invalid.
func nexusGrants(repos []nexusv1alpha1.RepositoryGrant) ([]nexusGrant, error) {
	if len(repos) == 0 {
		return nil, errors.New("spec.repositories is empty")
	}
	seen := map[string]bool{}
	out := make([]nexusGrant, 0, len(repos))
	for _, g := range repos {
		format := g.Format
		if format == "" {
			format = nexusv1alpha1.FormatDocker
		}
		if seen[g.Name] {
			return nil, fmt.Errorf("spec.repositories names %q twice", g.Name)
		}
		seen[g.Name] = true
		privileges, err := nexus.RepositoryPrivileges(nexus.Format(format), g.Name, nexus.Access(g.Access))
		if err != nil {
			return nil, fmt.Errorf("spec.repositories: %w", err)
		}
		out = append(out, nexusGrant{format: nexus.Format(format), repository: g.Name, privileges: privileges})
	}
	return out, nil
}

// ensureFinalizer adds this bridge's finalizer right before the first
// Nexus write, so no user or role exists without the finalizer that
// revokes it, and a refused object never depends on this bridge to be
// deleted (ADR-0032).
func (r *NexusReconciler) ensureFinalizer(ctx context.Context, nxa *nexusv1alpha1.NexusAccess) error {
	finalizer := r.Config.NexusFinalizer()
	if controllerutil.ContainsFinalizer(nxa, finalizer) {
		return nil
	}
	patch := client.MergeFromWithOptions(nxa.DeepCopy(), client.MergeFromWithOptimisticLock{})
	controllerutil.AddFinalizer(nxa, finalizer)
	if err := r.Patch(ctx, nxa, patch); err != nil {
		return fmt.Errorf("add finalizer: %w", err)
	}
	return nil
}

// nexusConflict is a condition resolved outside the NexusAccess: a role or
// user that belongs to someone else. It is reported, not retried with
// backoff.
type nexusConflict struct {
	reason  string
	message string
}

func (c *nexusConflict) Error() string { return c.message }

func (r *NexusReconciler) reconcileNormal(ctx context.Context, nxa *nexusv1alpha1.NexusAccess) (ctrl.Result, error) {
	cluster := r.Config.ClusterName

	// Refusals (ADR-0030 parity): the Secret is deleted and the users of
	// this NexusAccess are deleted (refuse). The CRD enforces most of
	// these; the checks guard objects admitted by an older CRD and the
	// data plane's boundary.
	if nxa.Spec.ServiceAccountRef == (nexusv1alpha1.ServiceAccountRef{}) &&
		nxa.Spec.TrustPolicy == (nexusv1alpha1.TrustPolicy{}) && len(nxa.Spec.Repositories) == 0 {
		return r.refuse(ctx, nxa, ReasonInvalidSpec,
			"spec is missing: a NexusAccess needs spec.serviceAccountRef, spec.trustPolicy and spec.repositories")
	}
	if nxa.Spec.TrustPolicy.Issuer != r.Config.OIDCIssuer.String() {
		return r.refuse(ctx, nxa, ReasonIssuerMismatch,
			fmt.Sprintf("trustPolicy.issuer %q does not match cluster issuer %q", nxa.Spec.TrustPolicy.Issuer, r.Config.OIDCIssuer.String()))
	}
	if nxa.Spec.TrustPolicy.Audience != r.Config.Audience {
		return r.refuse(ctx, nxa, ReasonAudienceMismatch,
			fmt.Sprintf("trustPolicy.audience %q is not the audience this bridge serves (%q)", nxa.Spec.TrustPolicy.Audience, r.Config.Audience))
	}
	if len(nxa.Name) > NexusAccessNameMaxLen {
		return r.refuse(ctx, nxa, ReasonInvalidSpec, fmt.Sprintf(
			"metadata.name is %d characters; NexusAccess names must be at most %d (the name is a label value on the user Secret)",
			len(nxa.Name), NexusAccessNameMaxLen))
	}
	grants, err := nexusGrants(nxa.Spec.Repositories)
	if err != nil {
		return r.refuse(ctx, nxa, ReasonInvalidSpec, err.Error())
	}
	if err := nxa.Spec.TokenTTL.Err(); err != nil {
		return r.refuse(ctx, nxa, ReasonInvalidSpec, "spec.tokenTTL: "+err.Error())
	}
	identity, err := nexus.IdentityName(cluster, nxa.Spec.ServiceAccountRef.Namespace, nxa.Spec.ServiceAccountRef.Name)
	if err != nil {
		return r.refuse(ctx, nxa, ReasonInvalidSpec, err.Error())
	}
	if !nexus.OwnsName(cluster, identity) {
		return ctrl.Result{}, fmt.Errorf("ADR-0009 invariant violation: Nexus name %q is not in cluster %q's ownership prefix", identity, cluster)
	}

	// Secret-name collision guard (audit F2), read uncached.
	secret, err := r.getSecret(ctx, nxa)
	if err != nil {
		return r.markTransientError(ctx, nxa, fmt.Errorf("read user Secret: %w", err))
	}
	if msg := r.secretConflict(nxa, secret); msg != "" {
		return r.markNotReadyWithRequeue(ctx, nxa, ReasonUserConflict, msg)
	}

	role, err := r.lookupRole(ctx, nxa, identity)
	if err != nil {
		return r.markError(ctx, nxa, err)
	}
	if err := r.ensureFinalizer(ctx, nxa); err != nil {
		return ctrl.Result{}, err
	}

	// The role: exactly the spec's privileges that exist as Nexus's own
	// (ADR-0036 decision d). A repository the role cannot grant is marked
	// on the Secret before anything else is written, so the data plane
	// stops issuing credentials at once.
	check, err := r.checkGrants(ctx, grants)
	if err != nil {
		return r.markError(ctx, nxa, err)
	}
	if len(check.missing) > 0 && secret != nil {
		if secret, err = r.putSecret(ctx, nxa, secret, false, func(s *corev1.Secret) { setGrantsIncomplete(s, check.missing) }); err != nil {
			return r.markTransientError(ctx, nxa, err)
		}
	}
	if check, err = r.convergeRole(ctx, nxa, identity, role, grants, check); err != nil {
		return r.markError(ctx, nxa, err)
	}
	missing := check.missing

	// The users of the identity this NexusAccess owns.
	owned, err := r.ownedUsers(ctx, nxa, identity+"_")
	if err != nil {
		return r.markError(ctx, nxa, err)
	}
	curID := secretUserID(secret)
	current := findUser(owned, curID)
	if current == nil && curID != "" {
		if msg, err := r.foreignSecretUser(ctx, nxa, identity, curID); err != nil {
			return r.markError(ctx, nxa, err)
		} else if msg != "" {
			return r.markNotReadyWithRequeue(ctx, nxa, ReasonUserConflict, msg)
		}
	}
	retiringID, _, _ := retiring(secret)
	now := r.now()

	// A user an administrator disabled or locked is neither rotated nor
	// replaced: a new generation would undo the administrator's decision.
	// The bridge never re-enables it (ADR-0036 decision c).
	if disabled := adminDisabled(owned, current, retiringID); disabled != nil {
		if secret, err = r.putSecret(ctx, nxa, secret, false, func(s *corev1.Secret) { convergeSecretMeta(s, missing) }); err != nil {
			return r.markTransientError(ctx, nxa, err)
		}
		if secret, err = r.retireIfDue(ctx, nxa, secret, owned, now); err != nil {
			return r.markError(ctx, nxa, err)
		}
		r.setStatusUser(nxa, disabled.UserID, identity, secret, nil)
		meta.SetStatusCondition(&nxa.Status.Conditions, metav1.Condition{
			Type: nexusv1alpha1.ConditionUserProvisioned, Status: metav1.ConditionFalse, Reason: ReasonUserDisabled,
			Message: fmt.Sprintf("Nexus user %q is %s in Nexus", disabled.UserID, disabled.Status), ObservedGeneration: nxa.Generation,
		})
		return r.markNotReadyWithRequeue(ctx, nxa, ReasonUserDisabled, fmt.Sprintf(
			"Nexus user %q is %s in Nexus; the bridge neither re-enables nor replaces it, and pulls through this NexusAccess fail until an administrator re-activates or deletes it",
			disabled.UserID, disabled.Status))
	}

	rotatedAt := (*time.Time)(nil)
	if why := nexusRotationReason(secret, current, identity, now); why != "" {
		log.FromContext(ctx).Info("creating the Nexus user of a new generation", "identity", identity, "reason", why)
		created, written, err := r.rotate(ctx, nxa, identity, secret, current, missing, now)
		if err != nil {
			return r.markError(ctx, nxa, err)
		}
		current, secret, rotatedAt = created, written, &now
	} else {
		if err := r.convergeUser(ctx, current, identity); err != nil {
			return r.markError(ctx, nxa, err)
		}
		if secret, err = r.putSecret(ctx, nxa, secret, false, func(s *corev1.Secret) { convergeSecretMeta(s, missing) }); err != nil {
			return r.markTransientError(ctx, nxa, err)
		}
	}

	if secret, err = r.retireIfDue(ctx, nxa, secret, owned, now); err != nil {
		return r.markError(ctx, nxa, err)
	}

	// Revoke what this NexusAccess no longer uses: users and the role of a
	// previous serviceAccountRef, users of a failed or superseded pass. It
	// lists every user and role of the cluster, so it runs only when
	// something changed; the janitor sweeps the same class periodically.
	if rotatedAt != nil || nxa.Status.ObservedGeneration != nxa.Generation ||
		nxa.Status.User == nil || nxa.Status.User.UserID != current.UserID {
		if err := r.deleteStale(ctx, nxa, identity, current.UserID, secret, now); err != nil {
			return r.markError(ctx, nxa, err)
		}
	}

	r.setStatusUser(nxa, current.UserID, identity, secret, rotatedAt)
	meta.SetStatusCondition(&nxa.Status.Conditions, metav1.Condition{
		Type: nexusv1alpha1.ConditionUserProvisioned, Status: metav1.ConditionTrue, Reason: ReasonReconcileSucceeded,
		Message: fmt.Sprintf("Nexus user %q with role %q provisioned", current.UserID, identity), ObservedGeneration: nxa.Generation,
	})
	requeue := r.requeueAfter(nxa, secret, now)
	if len(missing) > 0 {
		requeue = min(requeue, NexusRepositoryRecheckInterval)
		reason, message := check.notReady()
		if err := r.setNotReady(ctx, nxa, reason, message); err != nil {
			return ctrl.Result{}, fmt.Errorf("update status: %w", err)
		}
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return r.markReady(ctx, nxa, requeue)
}

// lookupRole returns the identity's role, nil when it does not exist, or
// a *nexusConflict when it is not this NexusAccess's (ADR-0009 adoption
// discipline; audit F2 for two NexusAccess objects of one identity).
func (r *NexusReconciler) lookupRole(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, identity string) (*nexus.Role, error) {
	role, err := r.Nexus.GetRole(ctx, identity)
	switch {
	case errors.Is(err, nexus.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("look up role: %w", err)
	case nexusRoleOwnedBy(r.Config.ClusterName, role, nxa.Namespace, nxa.Name):
		return role, nil
	}
	if c, ns, name, ok := parseNexusOwner(role.Description); ok && c == r.Config.ClusterName {
		return nil, &nexusConflict{reason: ReasonRoleConflict, message: fmt.Sprintf(
			"Nexus role %q (the role of ServiceAccount %s/%s) belongs to NexusAccess %s/%s, not %s/%s; refusing to adopt it. A ServiceAccount is served through one NexusAccess",
			identity, nxa.Spec.ServiceAccountRef.Namespace, nxa.Spec.ServiceAccountRef.Name, ns, name, nxa.Namespace, nxa.Name)}
	}
	return nil, &nexusConflict{reason: ReasonRoleConflict, message: fmt.Sprintf(
		"Nexus role %q exists but its description does not mark it as belonging to cluster %q; refusing to adopt it",
		identity, r.Config.ClusterName)}
}

// grantCheck is what checkGrants found out about the spec's repositories.
type grantCheck struct {
	// existing are the privileges the role grants, sorted.
	existing []string
	// missing are the repositories the role does not grant, sorted: those
	// Nexus has no privilege for, and those in conflicts.
	missing []string
	// conflicts maps a repository to why the privilege Nexus reports under
	// one of its repository-view names is not Nexus's own.
	conflicts map[string]string
}

// notReady is the Ready condition's reason and message while repositories
// are missing: PrivilegeConflict when a privilege is not Nexus's own, which
// only an administrator can resolve, else RepositoryNotFound.
func (c grantCheck) notReady() (reason, message string) {
	var absent []string
	for _, repo := range c.missing {
		if _, conflict := c.conflicts[repo]; !conflict {
			absent = append(absent, repo)
		}
	}
	const tail = "; the role grants the others, and the bridge issues no credentials for this NexusAccess until "
	if len(c.conflicts) == 0 {
		return ReasonRepositoryNotFound, fmt.Sprintf("Nexus has no %s repository %s", nexusv1alpha1.FormatDocker, quoteList(absent)) +
			tail + "every repository exists or leaves spec.repositories"
	}
	var parts []string
	for _, repo := range slices.Sorted(maps.Keys(c.conflicts)) {
		parts = append(parts, c.conflicts[repo])
	}
	if len(absent) > 0 {
		parts = append(parts, fmt.Sprintf("Nexus has no %s repository %s", nexusv1alpha1.FormatDocker, quoteList(absent)))
	}
	return ReasonPrivilegeConflict, strings.Join(parts, "; ") + tail +
		"an administrator deletes each privilege that is not Nexus's own (Nexus reports it under the repository-view name while no repository of that name exists) and every repository exists, or those repositories leave spec.repositories"
}

// checkGrants reads every privilege the spec needs. A repository is
// granted only when each of its privileges exists and is Nexus's built-in
// repository-view privilege of it (nexus.VerifyRepositoryPrivilege): a
// privilege someone created under that name while the repository does not
// exist may carry any permission, such as a wildcard privilege, and the
// bridge would hand it to every credential of the identity (ADR-0036
// decision b). A repository counts as missing when any of its privileges
// does not exist or is not Nexus's own.
func (r *NexusReconciler) checkGrants(ctx context.Context, grants []nexusGrant) (grantCheck, error) {
	var c grantCheck
	for _, g := range grants {
		granted := true
		for _, name := range g.privileges {
			p, err := r.Nexus.GetPrivilege(ctx, name)
			if errors.Is(err, nexus.ErrNotFound) {
				granted = false
				break
			}
			if err != nil {
				return grantCheck{}, fmt.Errorf("check privilege %q: %w", name, err)
			}
			if err := nexus.VerifyRepositoryPrivilege(p, g.format, g.repository); err != nil {
				if c.conflicts == nil {
					c.conflicts = map[string]string{}
				}
				c.conflicts[g.repository] = err.Error()
				granted = false
				break
			}
		}
		if granted {
			c.existing = append(c.existing, g.privileges...)
		} else {
			c.missing = append(c.missing, g.repository)
		}
	}
	slices.Sort(c.existing)
	c.existing = slices.Compact(c.existing)
	slices.Sort(c.missing)
	return c, nil
}

// convergeRole writes the role with exactly the privileges check found to
// exist (ADR-0036 decision d): a privilege the spec no longer names goes in
// the same pass, even when the same edit names a missing repository, and
// nothing missing is added. It returns the check with the missing
// repositories, which grow when a repository vanished between the check
// and the write.
func (r *NexusReconciler) convergeRole(
	ctx context.Context, nxa *nexusv1alpha1.NexusAccess, identity string,
	role *nexus.Role, grants []nexusGrant, check grantCheck,
) (grantCheck, error) {
	logger := log.FromContext(ctx)
	for attempt := 0; ; attempt++ {
		want := nexus.Role{
			ID: identity, Name: identity,
			Description: NexusRoleDescription(r.Config.ClusterName, nxa.Namespace, nxa.Name),
			Privileges:  check.existing,
		}
		var err error
		switch {
		case role == nil:
			logger.Info("creating Nexus role", "role", identity)
			_, err = r.Nexus.CreateRole(ctx, want)
		case !roleMatches(role, want):
			logger.Info("updating Nexus role to match spec", "role", identity)
			err = r.Nexus.UpdateRole(ctx, want)
		default:
			return check, nil
		}
		var dropped *nexus.PrivilegesDroppedError
		switch {
		case err == nil:
			return check, nil
		case errors.As(err, &dropped):
			// Nexus 3.91+: stored without the privileges of a repository
			// that vanished after the check. The role is right; the
			// repository is missing.
			for _, g := range grants {
				if slices.ContainsFunc(g.privileges, func(p string) bool { return slices.Contains(dropped.Dropped, p) }) &&
					!slices.Contains(check.missing, g.repository) {
					check.missing = append(check.missing, g.repository)
				}
			}
			slices.Sort(check.missing)
			return check, nil
		case errors.Is(err, nexus.ErrBadRequest) && attempt == 0:
			// Before 3.91 a repository that vanished after the check fails
			// the write with 400 and nothing is stored; for a create the
			// role may also have appeared meanwhile. Check and write again
			// once, so a removal waits for one retry at most.
			logger.Info("Nexus refused the role write; checking the privileges and the role again", "role", identity, "err", err.Error())
			if check, err = r.checkGrants(ctx, grants); err != nil {
				return grantCheck{}, err
			}
			if role, err = r.lookupRole(ctx, nxa, identity); err != nil {
				return grantCheck{}, err
			}
		default:
			return grantCheck{}, fmt.Errorf("write role %q: %w", identity, err)
		}
	}
}

// roleMatches reports whether role already is want: same name,
// description and privileges, and no contained roles (an administrator
// adding one, such as nx-admin, is drift the bridge reverts).
func roleMatches(role *nexus.Role, want nexus.Role) bool {
	got := slices.Clone(role.Privileges)
	slices.Sort(got)
	return role.Name == want.Name && role.Description == want.Description &&
		slices.Equal(slices.Compact(got), want.Privileges) && len(role.Roles) == 0
}

// ownedUsers lists the users whose ids begin with prefix and that this
// NexusAccess owns (nexusUserOwnedBy).
func (r *NexusReconciler) ownedUsers(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, prefix string) ([]nexus.User, error) {
	users, err := r.Nexus.ListUsers(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	owned := users[:0:0]
	for i := range users {
		if nexusUserOwnedBy(r.Config.ClusterName, &users[i], nxa.Namespace, nxa.Name) {
			owned = append(owned, users[i])
		}
	}
	return owned, nil
}

// foreignSecretUser explains why the user the Secret names, of the current
// identity, exists but is not this NexusAccess's (its markers were changed
// out of band), or returns "" when no such user exists. The bridge then
// neither uses nor replaces it: a new generation would leave a user with
// a valid password it can never revoke.
func (r *NexusReconciler) foreignSecretUser(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, identity, userID string) (string, error) {
	if id, _, ok := nexus.ParseUserID(userID); !ok || id != identity {
		return "", nil
	}
	u, err := r.Nexus.GetUser(ctx, userID)
	switch {
	case errors.Is(err, nexus.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("look up user: %w", err)
	}
	if nexusUserOwnedBy(r.Config.ClusterName, u, nxa.Namespace, nxa.Name) {
		return "", nil
	}
	return fmt.Sprintf("Nexus user %q, whose password the Secret holds, is no longer marked as this NexusAccess's (firstName and lastName were changed); "+
		"refusing to use or replace it. Restore its markers or delete the user", userID), nil
}

func findUser(users []nexus.User, id string) *nexus.User {
	if id == "" {
		return nil
	}
	for i := range users {
		if users[i].UserID == id {
			return &users[i]
		}
	}
	return nil
}

// adminDisabled returns the user whose status keeps this NexusAccess from
// a usable user: the current user when it is not active, or, when no
// current user exists, any owned user of the identity that is not active
// and not retiring (the current user of a Secret that was deleted, for
// example during a refusal).
func adminDisabled(owned []nexus.User, current *nexus.User, retiringID string) *nexus.User {
	if current != nil {
		if current.Status.Active() {
			return nil
		}
		return current
	}
	for i := range owned {
		if owned[i].UserID != retiringID && !owned[i].Status.Active() {
			return &owned[i]
		}
	}
	return nil
}

// secretUserID returns the user id whose password the Secret holds, or ""
// when the Secret is absent, incomplete, or its two records disagree.
func secretUserID(secret *corev1.Secret) string {
	if secret == nil {
		return ""
	}
	username, _, err := nexussecret.Credentials(secret)
	if err != nil || nexussecret.UserID(secret) != username {
		return ""
	}
	return username
}

func retiring(secret *corev1.Secret) (string, time.Time, bool) {
	if secret == nil {
		return "", time.Time{}, false
	}
	return nexussecret.Retiring(secret)
}

// nexusRotationReason returns why the identity needs a user of a new
// generation now, or "" when the current one stays.
func nexusRotationReason(secret *corev1.Secret, current *nexus.User, identity string, now time.Time) string {
	if secret == nil {
		return "user Secret missing"
	}
	username, _, err := nexussecret.Credentials(secret)
	switch {
	case err != nil:
		return "user Secret incomplete"
	case nexussecret.UserID(secret) != username:
		return "user Secret inconsistent"
	}
	if id, _, ok := nexus.ParseUserID(username); !ok || id != identity {
		return "user Secret holds the user of another identity"
	}
	if current == nil {
		return "the user the Secret names does not exist"
	}
	nb, ok := nexussecret.RotationNotBefore(secret)
	switch {
	case !ok:
		return "user Secret carries no rotation promise"
	case !now.Before(nb.Add(RotationSafetyMargin)):
		return "scheduled rotation"
	}
	return ""
}

// errRoleNotHeld is returned when a new user came back without the
// identity's role: the role was deleted between its write and the user
// create, and the user holds a hidden reference to it (ADR-0036 Context 5).
var errRoleNotHeld = errors.New("the new Nexus user does not hold the identity's role; it was deleted meanwhile and is re-created on the next pass")

// rotate creates the identity's user of a new generation and stores its
// password (ADR-0036 decision c). The previous user, if it was the
// current one of the same identity and active, is recorded as retiring;
// retireIfDue deletes it after NexusUserRetireGrace. written is the Secret
// as written.
func (r *NexusReconciler) rotate(
	ctx context.Context, nxa *nexusv1alpha1.NexusAccess, identity string,
	secret *corev1.Secret, previous *nexus.User, missing []string, now time.Time,
) (created *nexus.User, written *corev1.Secret, err error) {
	generation, err := nexus.NewGeneration()
	if err != nil {
		return nil, nil, err
	}
	userID, err := nexus.UserID(r.Config.ClusterName, nxa.Spec.ServiceAccountRef.Namespace, nxa.Spec.ServiceAccountRef.Name, generation)
	if err != nil {
		return nil, nil, err
	}
	password, err := nexus.GeneratePassword()
	if err != nil {
		return nil, nil, err
	}

	// Record the pending user first, so the janitor never takes a user
	// being created for an orphan.
	written, err = r.putSecret(ctx, nxa, secret, false, func(s *corev1.Secret) {
		s.Annotations[nexussecret.AnnotationPendingUserID] = userID
		setGrantsIncomplete(s, missing)
	})
	if err != nil {
		return nil, nil, err
	}

	created, err = r.Nexus.CreateUser(ctx, nexus.User{
		UserID:       userID,
		FirstName:    NexusUserFirstName(r.Config.ClusterName),
		LastName:     NexusUserLastName(nxa.Namespace, nxa.Name),
		EmailAddress: nexusUserEmail,
		Status:       nexus.UserActive,
		Roles:        []string{identity},
	}, password)
	if err != nil {
		return nil, nil, fmt.Errorf("create user %q: %w", userID, err)
	}
	if !slices.Contains(created.Roles, identity) {
		return nil, nil, errRoleNotHeld
	}

	retire := previous != nil && previous.UserID != userID && previous.Status.Active()
	written, err = r.putSecret(ctx, nxa, written, true, func(s *corev1.Secret) {
		s.Data = map[string][]byte{
			nexussecret.KeyUsername: []byte(userID),
			nexussecret.KeyPassword: []byte(password),
		}
		s.Annotations[nexussecret.AnnotationUserID] = userID
		s.Annotations[nexussecret.AnnotationRotationNotBefore] = nexussecret.FormatTime(now.Add(PasswordRotationInterval))
		delete(s.Annotations, nexussecret.AnnotationPendingUserID)
		if retire {
			s.Annotations[nexussecret.AnnotationRetiringUserID] = previous.UserID
			s.Annotations[nexussecret.AnnotationRetireAfter] = nexussecret.FormatTime(now.Add(NexusUserRetireGrace))
		} else {
			delete(s.Annotations, nexussecret.AnnotationRetiringUserID)
			delete(s.Annotations, nexussecret.AnnotationRetireAfter)
		}
		setGrantsIncomplete(s, missing)
	})
	if err != nil {
		// The new user's password is lost; the pending record keeps the
		// janitor away until the next pass replaces it and deletes the
		// user as stale.
		return nil, nil, err
	}
	return created, written, nil
}

// convergeUser rewrites the current user's roles to exactly the identity's
// role and its e-mail address to the bridge's, echoing its status: an
// administrator adding a role (such as nx-admin) is drift the bridge
// reverts, and a role deleted and re-created is lost by every user.
func (r *NexusReconciler) convergeUser(ctx context.Context, current *nexus.User, identity string) error {
	if slices.Equal(current.Roles, []string{identity}) && current.EmailAddress == nexusUserEmail {
		return nil
	}
	u := *current
	u.Roles, u.EmailAddress = []string{identity}, nexusUserEmail
	log.FromContext(ctx).Info("updating Nexus user to match spec", "user", u.UserID)
	if err := r.Nexus.UpdateUser(ctx, u); err != nil {
		return fmt.Errorf("update user %q: %w", u.UserID, err)
	}
	return nil
}

// retireIfDue deletes the retiring user once its instant has passed and
// clears the record. A retiring user that is no longer this NexusAccess's
// is left alone.
func (r *NexusReconciler) retireIfDue(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, secret *corev1.Secret, owned []nexus.User, now time.Time) (*corev1.Secret, error) {
	id, after, ok := retiring(secret)
	if !ok || now.Before(after) {
		return secret, nil
	}
	if findUser(owned, id) != nil {
		if err := r.Nexus.DeleteUser(ctx, id); err != nil {
			return secret, fmt.Errorf("delete retired user %q: %w", id, err)
		}
		log.FromContext(ctx).Info("deleted the Nexus user a rotation replaced", "user", id)
	}
	return r.putSecret(ctx, nxa, secret, false, func(s *corev1.Secret) {
		delete(s.Annotations, nexussecret.AnnotationRetiringUserID)
		delete(s.Annotations, nexussecret.AnnotationRetireAfter)
	})
}

// deleteStale deletes every user this NexusAccess owns but keep and a
// retiring user within its grace, and every role it owns but the
// identity's. Users of a previous identity go whatever their status: the
// identity change is their revocation.
func (r *NexusReconciler) deleteStale(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, identity, keep string, secret *corev1.Secret, now time.Time) error {
	logger := log.FromContext(ctx)
	cluster := r.Config.ClusterName
	retiringID, after, isRetiring := retiring(secret)
	users, err := r.ownedUsers(ctx, nxa, nexus.ClusterPrefix(cluster))
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.UserID == keep || (isRetiring && u.UserID == retiringID && now.Before(after)) {
			continue
		}
		if err := r.Nexus.DeleteUser(ctx, u.UserID); err != nil {
			return fmt.Errorf("delete stale user %q: %w", u.UserID, err)
		}
		logger.Info("deleted stale Nexus user no longer used by this NexusAccess", "user", u.UserID)
	}
	roles, err := r.Nexus.ListRoles(ctx, nexus.ClusterPrefix(cluster))
	if err != nil {
		return fmt.Errorf("list roles: %w", err)
	}
	for i := range roles {
		role := &roles[i]
		if role.ID == identity || !nexusRoleOwnedBy(cluster, role, nxa.Namespace, nxa.Name) {
			continue
		}
		if err := r.Nexus.DeleteRole(ctx, role.ID); err != nil {
			return fmt.Errorf("delete stale role %q: %w", role.ID, err)
		}
		logger.Info("deleted stale Nexus role no longer used by this NexusAccess", "role", role.ID)
	}
	return nil
}

// convergeSecretMeta is the Secret edit of a pass that creates no user:
// the grants-incomplete mark, and no pending user. A pending record
// outside a rotation is left by a pass that failed after recording it; the
// user it names, if created, has lost its password and must not be spared
// by the janitor.
func convergeSecretMeta(s *corev1.Secret, missing []string) {
	setGrantsIncomplete(s, missing)
	delete(s.Annotations, nexussecret.AnnotationPendingUserID)
}

// setGrantsIncomplete sets or removes the grants-incomplete mark.
func setGrantsIncomplete(s *corev1.Secret, missing []string) {
	if len(missing) == 0 {
		delete(s.Annotations, nexussecret.AnnotationGrantsIncomplete)
		return
	}
	s.Annotations[nexussecret.AnnotationGrantsIncomplete] = nexussecret.FormatGrantsIncomplete(missing)
}

// refuse reports a NexusAccess the bridge will not serve and suspends what
// it owns (ADR-0030 parity; suspendNexus). Refused objects are re-checked
// every resyncAfter, so a user an administrator re-enables meanwhile is
// deleted again. Once nothing of this bridge is left in Nexus, its
// finalizers are released (ADR-0032).
func (r *NexusReconciler) refuse(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, reason, message string) (ctrl.Result, error) {
	outcome, releasable, suspendErr := r.suspend(ctx, nxa)
	if suspendErr != nil {
		message += fmt.Sprintf("; revoking its Nexus users failed and is retried: %v", suspendErr)
		outcome = "revoking its Nexus users failed and is retried: " + suspendErr.Error()
	}
	meta.SetStatusCondition(&nxa.Status.Conditions, metav1.Condition{
		Type:               nexusv1alpha1.ConditionUserProvisioned,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            "no usable Nexus user while the NexusAccess is refused (ADR-0030): " + outcome,
		ObservedGeneration: nxa.Generation,
	})
	if err := r.setNotReady(ctx, nxa, reason, message); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}
	if suspendErr != nil {
		if errors.Is(suspendErr, nexus.ErrRateLimited) {
			return ctrl.Result{RequeueAfter: r.rateLimitWait()}, nil
		}
		return ctrl.Result{}, suspendErr
	}
	if releasable {
		if err := r.releaseRefused(ctx, nxa); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: resyncAfter(nxa)}, nil
}

// suspend deletes the Secret of a refused NexusAccess and every active
// user it owns. Nexus cannot suspend a user usefully: a disabled user's
// docker bearer token is valid again once it is re-enabled (ADR-0036
// Context 1), and resuming creates a user of a new generation anyway. A
// user that is not active (an administrator disabled or locked it) stays,
// so that resuming reports UserDisabled instead of undoing the
// administrator's decision. The role stays too; it grants nothing without
// a user of this bridge.
//
// The Secret goes first and whatever Nexus answers; Nexus is asked only
// while the object holds a finalizer this bridge may have set.
func (r *NexusReconciler) suspend(ctx context.Context, nxa *nexusv1alpha1.NexusAccess) (outcome string, releasable bool, err error) {
	secretErr := r.deleteOwnSecret(ctx, nxa)
	if !slices.ContainsFunc(r.Config.NexusReleasedFinalizers(), func(f string) bool { return controllerutil.ContainsFinalizer(nxa, f) }) {
		return "it has no Nexus user of this bridge", false, secretErr
	}
	cluster := r.Config.ClusterName
	users, err := r.ownedUsers(ctx, nxa, nexus.ClusterPrefix(cluster))
	if err != nil {
		return "", false, errors.Join(secretErr, err)
	}
	var deleted, kept []string
	for _, u := range users {
		if !u.Status.Active() {
			kept = append(kept, u.UserID)
			continue
		}
		if err := r.Nexus.DeleteUser(ctx, u.UserID); err != nil {
			return "", false, errors.Join(secretErr, fmt.Errorf("delete user %q: %w", u.UserID, err))
		}
		deleted = append(deleted, u.UserID)
		log.FromContext(ctx).Info("deleted Nexus user of a refused NexusAccess", "user", u.UserID)
	}
	roles, err := r.Nexus.ListRoles(ctx, nexus.ClusterPrefix(cluster))
	if err != nil {
		return "", false, errors.Join(secretErr, fmt.Errorf("list roles: %w", err))
	}
	var ownRoles []string
	for i := range roles {
		if nexusRoleOwnedBy(cluster, &roles[i], nxa.Namespace, nxa.Name) {
			ownRoles = append(ownRoles, roles[i].ID)
		}
	}
	var parts []string
	if len(deleted) > 0 {
		parts = append(parts, "deleted Nexus user "+quoteList(deleted))
	}
	if len(kept) > 0 {
		parts = append(parts, "Nexus user "+quoteList(kept)+" was disabled or locked by an administrator and stays")
	}
	if len(ownRoles) > 0 {
		parts = append(parts, "role "+quoteList(ownRoles)+" stays without a user of this bridge until the NexusAccess is accepted again")
	}
	if len(parts) == 0 {
		parts = append(parts, "it has no Nexus user of this bridge")
	}
	return strings.Join(parts, "; "), len(kept) == 0 && len(ownRoles) == 0 && secretErr == nil, secretErr
}

// releaseRefused removes this bridge's finalizers from a refused
// NexusAccess without users or roles of this bridge (ADR-0032; see
// Reconciler.releaseRefused for the shared finalizer).
func (r *NexusReconciler) releaseRefused(ctx context.Context, nxa *nexusv1alpha1.NexusAccess) error {
	sharedIsOurs := r.Config.NexusFinalizer() == NexusFinalizerName && r.Config.statusUserIsOurs(nxa)
	var release []string
	for _, f := range r.Config.NexusReleasedFinalizers() {
		if controllerutil.ContainsFinalizer(nxa, f) && (f != NexusFinalizerName || sharedIsOurs) {
			release = append(release, f)
		}
	}
	if len(release) == 0 {
		return nil
	}
	patch := client.MergeFromWithOptions(nxa.DeepCopy(), client.MergeFromWithOptimisticLock{})
	for _, f := range release {
		controllerutil.RemoveFinalizer(nxa, f)
	}
	if err := r.Patch(ctx, nxa, patch); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("release the finalizer of a refused NexusAccess: %w", err))
	}
	log.FromContext(ctx).Info("released a refused NexusAccess without a Nexus user or role of this bridge", "finalizers", release)
	return nil
}

// statusUserIsOurs reports whether the role nxa's status records, if any,
// is one of this cluster's (see statusRobotIsOurs).
func (c *Config) statusUserIsOurs(nxa *nexusv1alpha1.NexusAccess) bool {
	return nxa.Status.User == nil || nxa.Status.User.RoleID == "" || nexus.OwnsName(c.ClusterName, nxa.Status.User.RoleID)
}

// reconcileDelete revokes everything the NexusAccess owns: its users of
// every generation and identity, whatever their status, then its roles,
// then its Secret, and only then releases the finalizers. While Nexus
// cannot be reached the finalizers hold (DeletionBlocked).
func (r *NexusReconciler) reconcileDelete(ctx context.Context, nxa *nexusv1alpha1.NexusAccess) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	var held []string
	for _, f := range r.Config.NexusReleasedFinalizers() {
		if controllerutil.ContainsFinalizer(nxa, f) {
			held = append(held, f)
		}
	}
	if len(held) == 0 {
		return ctrl.Result{}, nil
	}
	cluster := r.Config.ClusterName
	users, err := r.ownedUsers(ctx, nxa, nexus.ClusterPrefix(cluster))
	if err != nil {
		return r.blockDeletion(ctx, nxa, held, err)
	}
	for _, u := range users {
		if err := r.Nexus.DeleteUser(ctx, u.UserID); err != nil {
			return r.blockDeletion(ctx, nxa, held, fmt.Errorf("delete user %q: %w", u.UserID, err))
		}
		logger.Info("deleted Nexus user", "user", u.UserID)
	}
	roles, err := r.Nexus.ListRoles(ctx, nexus.ClusterPrefix(cluster))
	if err != nil {
		return r.blockDeletion(ctx, nxa, held, fmt.Errorf("list roles: %w", err))
	}
	for i := range roles {
		if !nexusRoleOwnedBy(cluster, &roles[i], nxa.Namespace, nxa.Name) {
			continue
		}
		if err := r.Nexus.DeleteRole(ctx, roles[i].ID); err != nil {
			return r.blockDeletion(ctx, nxa, held, fmt.Errorf("delete role %q: %w", roles[i].ID, err))
		}
		logger.Info("deleted Nexus role", "role", roles[i].ID)
	}
	if err := r.deleteOwnSecret(ctx, nxa); err != nil {
		return r.blockDeletion(ctx, nxa, held, err)
	}
	patch := client.MergeFromWithOptions(nxa.DeepCopy(), client.MergeFromWithOptimisticLock{})
	for _, f := range held {
		controllerutil.RemoveFinalizer(nxa, f)
	}
	if err := r.Patch(ctx, nxa, patch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(fmt.Errorf("remove finalizer: %w", err))
	}
	return ctrl.Result{}, nil
}

// blockDeletion records why the finalizers cannot be released yet (see
// Reconciler.blockDeletion). A rate-limited Nexus is waited for without
// backoff errors.
func (r *NexusReconciler) blockDeletion(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, held []string, cause error) (ctrl.Result, error) {
	quoted := make([]string, len(held))
	for i, f := range held {
		quoted[i] = strconv.Quote(f)
	}
	what := "the " + strings.Join(quoted, " and ") + " finalizer"
	if len(held) > 1 {
		what += "s"
	}
	msg := fmt.Sprintf("cannot revoke the Nexus users yet, deletion is waiting: %v. "+
		"If Nexus is gone for good, remove %s by hand; the janitor deletes the orphaned users and role once Nexus is reachable",
		cause, what)
	if err := r.setNotReady(ctx, nxa, ReasonDeletionBlocked, msg); err != nil {
		log.FromContext(ctx).V(1).Info("could not record deletion-blocked status", "err", err.Error())
	}
	if errors.Is(cause, nexus.ErrRateLimited) {
		return ctrl.Result{RequeueAfter: r.rateLimitWait()}, nil
	}
	return ctrl.Result{}, cause
}

// secretConflict returns why the Secret at nxa's Secret name belongs to
// someone else, or "" when nxa may write it (nil = absent). Unlike robot
// Secrets, no Nexus Secret predates the labels, so an unlabelled Secret is
// never adopted.
func (r *NexusReconciler) secretConflict(nxa *nexusv1alpha1.NexusAccess, secret *corev1.Secret) string {
	name := nexussecret.Name(nxa.Namespace, nxa.Name)
	switch {
	case secret == nil, nexussecret.OwnedBy(secret, nxa.Namespace, nxa.Name):
		return ""
	case nexussecret.IsManaged(secret):
		return fmt.Sprintf("Secret %q is already owned by a different NexusAccess (naming collision); refusing to overwrite", name)
	}
	return fmt.Sprintf("Secret %q in %s is not a Nexus Secret of the bridge; refusing to adopt it. Rename or delete it", name, r.Config.Namespace)
}

// secretDeleteConflict adds to secretConflict that the Secret must be
// stamped for this cluster (see Reconciler.secretDeleteConflict).
func (r *NexusReconciler) secretDeleteConflict(nxa *nexusv1alpha1.NexusAccess, secret *corev1.Secret) string {
	if msg := r.secretConflict(nxa, secret); msg != "" {
		return msg
	}
	if secret != nil && secret.Labels[nexussecret.LabelCluster] != r.Config.ClusterName {
		return fmt.Sprintf("Secret %q is stamped for cluster %q, not %q",
			secret.Name, secret.Labels[nexussecret.LabelCluster], r.Config.ClusterName)
	}
	return ""
}

// deleteOwnSecret deletes nxa's Secret if nxa owns it.
func (r *NexusReconciler) deleteOwnSecret(ctx context.Context, nxa *nexusv1alpha1.NexusAccess) error {
	secret, err := r.getSecret(ctx, nxa)
	if err != nil {
		return fmt.Errorf("read user Secret: %w", err)
	}
	if secret == nil {
		return nil
	}
	if msg := r.secretDeleteConflict(nxa, secret); msg != "" {
		log.FromContext(ctx).Info("keeping a Secret at the user Secret's name that does not belong to this NexusAccess",
			"secret", secret.Name, "reason", msg)
		return nil
	}
	if err := r.Delete(ctx, secret, client.Preconditions{UID: &secret.UID}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete user Secret: %w", err)
	}
	log.FromContext(ctx).Info("deleted user Secret", "secret", secret.Name)
	return nil
}

// getSecret returns nxa's Secret, read uncached, or nil when it does not
// exist.
func (r *NexusReconciler) getSecret(ctx context.Context, nxa *nexusv1alpha1.NexusAccess) (*corev1.Secret, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	s := &corev1.Secret{}
	err := reader.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: nexussecret.Name(nxa.Namespace, nxa.Name)}, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return s, nil
}

// putSecret converges nxa's Secret: a copy of existing (nil = absent) gets
// the labels, then edit applies the pass's changes; nothing is written
// when that changes nothing, and no Secret is created without a password
// or a pending user. It returns the Secret as written.
//
// withPassword marks an edit that stores a new password, the only one of a
// user Nexus now holds. When the Secret changed since it was read, it is
// read again and the edit applied on top of the current version, as long
// as the Secret still belongs to nxa (see Reconciler.writeRobotSecret).
func (r *NexusReconciler) putSecret(
	ctx context.Context, nxa *nexusv1alpha1.NexusAccess, existing *corev1.Secret,
	withPassword bool, edit func(*corev1.Secret),
) (*corev1.Secret, error) {
	write := func(cur *corev1.Secret) (*corev1.Secret, error) {
		s := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: nexussecret.Name(nxa.Namespace, nxa.Name), Namespace: r.Config.Namespace},
			Type:       corev1.SecretTypeOpaque,
		}
		if cur != nil {
			s = cur.DeepCopy()
		}
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		for k, v := range nexussecret.Labels(r.Config.ClusterName, nxa.Namespace, nxa.Name) {
			s.Labels[k] = v
		}
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		edit(s)
		switch {
		case cur == nil && len(s.Data) == 0 && nexussecret.PendingUserID(s) == "":
			return nil, nil
		case cur == nil:
			if err := r.Create(ctx, s); err != nil {
				return nil, err
			}
			return s, nil
		case equalStringMaps(s.Labels, cur.Labels) && equalStringMaps(s.Annotations, cur.Annotations) && equalData(s.Data, cur.Data):
			return cur, nil
		}
		if err := r.Update(ctx, s); err != nil {
			return nil, err
		}
		return s, nil
	}
	if !withPassword {
		s, err := write(existing)
		if err != nil {
			return nil, fmt.Errorf("write user Secret: %w", err)
		}
		return s, nil
	}
	current := existing
	var out *corev1.Secret
	err := retry.OnError(retry.DefaultRetry, isSecretWriteRace, func() error {
		s, err := write(current)
		if err == nil {
			out = s
			return nil
		}
		if !isSecretWriteRace(err) {
			return err
		}
		fresh, readErr := r.getSecret(ctx, nxa)
		if readErr != nil {
			return fmt.Errorf("re-read user Secret to store the new password: %w", readErr)
		}
		if msg := r.secretConflict(nxa, fresh); msg != "" {
			return fmt.Errorf("cannot store the new password: %s", msg)
		}
		current = fresh
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store the new user's password: %w", err)
	}
	return out, nil
}

func equalData(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || string(bv) != string(v) {
			return false
		}
	}
	return true
}

// setStatusUser records the user, the role and the Secret in the status.
// LastRotated advances only when a new user was created.
func (r *NexusReconciler) setStatusUser(nxa *nexusv1alpha1.NexusAccess, userID, roleID string, secret *corev1.Secret, rotatedAt *time.Time) {
	if nxa.Status.User == nil {
		nxa.Status.User = &nexusv1alpha1.UserRef{}
	}
	nxa.Status.User.UserID = userID
	nxa.Status.User.RoleID = roleID
	nxa.Status.User.PasswordSecretRef = ""
	if secret != nil {
		nxa.Status.User.PasswordSecretRef = secret.Name
	}
	if rotatedAt != nil {
		t := metav1.NewTime(*rotatedAt)
		nxa.Status.User.LastRotated = &t
	}
}

// requeueAfter schedules the next pass: at the next rotation (plus the
// margin) or retirement, but no later than resyncAfter.
func (r *NexusReconciler) requeueAfter(nxa *nexusv1alpha1.NexusAccess, secret *corev1.Secret, now time.Time) time.Duration {
	next := resyncAfter(nxa)
	if secret != nil {
		if nb, ok := nexussecret.RotationNotBefore(secret); ok {
			next = min(next, nb.Add(RotationSafetyMargin).Sub(now))
		}
		if _, after, ok := nexussecret.Retiring(secret); ok {
			next = min(next, after.Sub(now))
		}
	}
	return max(next, time.Second)
}

// rateLimitWait is how long to wait before the next pass while Nexus rate
// limits the admin credential.
func (r *NexusReconciler) rateLimitWait() time.Duration {
	if r.Backoff == nil {
		return DefaultNexusRateLimitBackoff
	}
	until, blocked := r.Backoff.Until()
	if !blocked {
		return time.Second
	}
	return max(until.Sub(r.Backoff.now()), time.Second)
}

func (r *NexusReconciler) markReady(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, requeueAfter time.Duration) (ctrl.Result, error) {
	// observedGeneration advances ONLY here: this generation is fully
	// applied (ADR-0023).
	nxa.Status.ObservedGeneration = nxa.Generation
	meta.SetStatusCondition(&nxa.Status.Conditions, metav1.Condition{
		Type:               nexusv1alpha1.ConditionTrustPolicyApplied,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonEnforcedByBridge,
		Message:            "trust policy enforced by the bridge's data plane; Nexus has no trust policies",
		ObservedGeneration: nxa.Generation,
	})
	meta.SetStatusCondition(&nxa.Status.Conditions, metav1.Condition{
		Type:               nexusv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonReconcileSucceeded,
		Message:            "Nexus user and role exist with the desired repositories",
		ObservedGeneration: nxa.Generation,
	})
	if err := r.Status().Update(ctx, nxa); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// markNotReadyWithRequeue writes Ready=False for conditions resolved
// outside the NexusAccess (a conflict, a disabled user) and re-checks on
// the resync interval.
func (r *NexusReconciler) markNotReadyWithRequeue(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, reason, message string) (ctrl.Result, error) {
	if err := r.setNotReady(ctx, nxa, reason, message); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status: %w", err)
	}
	return ctrl.Result{RequeueAfter: resyncAfter(nxa)}, nil
}

// markError reports err: a *nexusConflict as a condition re-checked on the
// resync interval, anything else as a transient error (markTransientError).
func (r *NexusReconciler) markError(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, err error) (ctrl.Result, error) {
	var conflict *nexusConflict
	if errors.As(err, &conflict) {
		return r.markNotReadyWithRequeue(ctx, nxa, conflict.reason, conflict.message)
	}
	return r.markTransientError(ctx, nxa, err)
}

// markTransientError writes Ready=False with the reason nexusErrorReason
// derives and returns the cause for a retry with backoff. While Nexus rate
// limits the admin credential the pass is instead requeued for when the
// backoff ends, without an error: no call would go out before.
func (r *NexusReconciler) markTransientError(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, cause error) (ctrl.Result, error) {
	reason := nexusErrorReason(cause)
	if err := r.setNotReady(ctx, nxa, reason, cause.Error()); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status while reporting transient error %q: %w", cause.Error(), err)
	}
	if reason == ReasonNexusRateLimited {
		return ctrl.Result{RequeueAfter: r.rateLimitWait()}, nil
	}
	return ctrl.Result{}, cause
}

// nexusErrorReason classifies an error of a pass for the Ready condition.
func nexusErrorReason(err error) string {
	var apiErr *nexus.APIError
	switch {
	case errors.Is(err, nexus.ErrRateLimited):
		return ReasonNexusRateLimited
	case errors.Is(err, nexus.ErrUnauthorized), errors.Is(err, nexus.ErrForbidden):
		return ReasonNexusAuthFailed
	case errors.As(err, &apiErr) && strings.HasPrefix(apiErr.Op, "create user") &&
		errors.Is(apiErr, nexus.ErrBadRequest) && strings.Contains(strings.ToLower(apiErr.Message), "password"):
		// The operator's nexus.password.validator refused the generated
		// password (ADR-0036 decision a).
		return ReasonPasswordRejected
	}
	return ReasonNexusError
}

// setNotReady sets Ready=False without touching observedGeneration.
func (r *NexusReconciler) setNotReady(ctx context.Context, nxa *nexusv1alpha1.NexusAccess, reason, message string) error {
	meta.SetStatusCondition(&nxa.Status.Conditions, metav1.Condition{
		Type:               nexusv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: nxa.Generation,
	})
	return r.Status().Update(ctx, nxa)
}

// quoteList renders names as a quoted, comma-separated list.
func quoteList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return strings.Join(quoted, ", ")
}
