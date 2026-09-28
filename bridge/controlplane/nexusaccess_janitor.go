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

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/nexussecret"
)

// NexusJanitor is the Janitor of the Nexus backend (ADR-0036): it
// periodically deletes the Nexus users and roles of this cluster that no
// NexusAccess uses any more (the owner is gone or no longer selected, or
// now maps to another identity), users a rotation superseded whose grace
// has passed, users of an identity that no Secret names, and Nexus Secrets
// whose NexusAccess is gone. It is the backstop for everything the
// reconciler cannot see: finalizers removed by hand, a bridge that was
// down while an object was deleted, a failed cleanup.
//
// It implements manager.Runnable without LeaderElectionRunnable, so it
// runs on the leader only.
type NexusJanitor struct {
	// Client deletes Secrets and patches finalizers.
	Client client.Client

	// Reader must be UNCACHED (mgr.GetAPIReader()). A sweep lists the
	// users first and then the Secrets and the owners live: a user created
	// after its pending record was written, and listed, is then always
	// seen as pending, and no user is judged against a spec older than the
	// one it was created from. Before each deletion the owner and its
	// Secret are read live once more. Falls back to Client when nil.
	Reader client.Reader

	// Nexus is the client shared with the reconciler (NexusBackoff.Wrap):
	// while Nexus rate limits the admin credential, sweeps stop calling.
	Nexus  nexus.Client
	Config *Config

	// Clock defaults to RealClock.
	Clock Clock

	// SweepInterval defaults to DefaultSweepInterval.
	SweepInterval time.Duration
}

// Start runs the periodic sweep loop until ctx is cancelled.
func (j *NexusJanitor) Start(ctx context.Context) error {
	interval := j.SweepInterval
	if interval == 0 {
		interval = DefaultSweepInterval
	}
	logger := log.FromContext(ctx).WithName("nexus-janitor").WithValues("interval", interval)
	logger.Info("starting orphan Nexus user sweep")
	if err := j.Sweep(ctx); err != nil {
		logger.Error(err, "initial sweep failed")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("nexus janitor stopped")
			return nil
		case <-ticker.C:
			if err := j.Sweep(ctx); err != nil {
				logger.Error(err, "sweep failed")
			}
		}
	}
}

func (j *NexusJanitor) reader() client.Reader {
	if j.Reader != nil {
		return j.Reader
	}
	return j.Client
}

func (j *NexusJanitor) now() time.Time {
	if j.Clock == nil {
		return time.Now()
	}
	return j.Clock.Now()
}

// Sweep performs one pass. Users are listed first, then roles, then the
// Nexus Secrets and the NexusAccess objects, both uncached. Per-object
// failures are logged and skipped. The Secret sweep needs no Nexus and
// runs while Nexus is unreachable; releasing finalizers does not.
func (j *NexusJanitor) Sweep(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("nexus-janitor")
	prefix := nexus.ClusterPrefix(j.Config.ClusterName)
	users, usersErr := j.Nexus.ListUsers(ctx, prefix)
	roles, rolesErr := j.Nexus.ListRoles(ctx, prefix)
	secrets, secretsErr := j.listSecrets(ctx)
	owners, err := j.listOwners(ctx)
	if err != nil {
		return fmt.Errorf("list NexusAccess objects: %w", err)
	}
	if secretsErr != nil {
		logger.Error(secretsErr, "failed to list Nexus Secrets")
	}
	if usersErr == nil {
		j.sweepUsers(ctx, users, secrets, secretsErr == nil, owners)
	}
	if rolesErr == nil {
		j.sweepRoles(ctx, roles, owners)
	}
	if secretsErr == nil {
		j.sweepSecrets(ctx, secrets, owners)
	}
	if err := errors.Join(usersErr, rolesErr); err != nil {
		return fmt.Errorf("list Nexus users and roles: %w", err)
	}
	j.releaseUnselected(ctx, owners)
	return nil
}

func (j *NexusJanitor) listOwners(ctx context.Context) (map[types.NamespacedName]*nexusv1alpha1.NexusAccess, error) {
	var list nexusv1alpha1.NexusAccessList
	if err := j.reader().List(ctx, &list); err != nil {
		return nil, err
	}
	owners := make(map[types.NamespacedName]*nexusv1alpha1.NexusAccess, len(list.Items))
	for i := range list.Items {
		nxa := &list.Items[i]
		owners[types.NamespacedName{Namespace: nxa.Namespace, Name: nxa.Name}] = nxa
	}
	return owners, nil
}

func (j *NexusJanitor) liveOwner(ctx context.Context, key types.NamespacedName) (*nexusv1alpha1.NexusAccess, error) {
	nxa := &nexusv1alpha1.NexusAccess{}
	err := j.reader().Get(ctx, key, nxa)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	return nxa, nil
}

// listSecrets lists this cluster's Nexus Secrets, uncached, by name.
func (j *NexusJanitor) listSecrets(ctx context.Context) (map[string]*corev1.Secret, error) {
	var list corev1.SecretList
	if err := j.reader().List(ctx, &list,
		client.InNamespace(j.Config.Namespace),
		client.MatchingLabels{
			nexussecret.LabelManagedBy:  nexussecret.LabelManagedByValue,
			nexussecret.LabelAccessKind: nexussecret.AccessKindNexus,
			nexussecret.LabelCluster:    j.Config.ClusterName,
		}); err != nil {
		return nil, err
	}
	out := make(map[string]*corev1.Secret, len(list.Items))
	for i := range list.Items {
		out[list.Items[i].Name] = &list.Items[i]
	}
	return out, nil
}

// liveSecret reads the Secret of the NexusAccess key uncached; nil when it
// does not exist or is not that NexusAccess's.
func (j *NexusJanitor) liveSecret(ctx context.Context, key types.NamespacedName) (*corev1.Secret, error) {
	s := &corev1.Secret{}
	err := j.reader().Get(ctx, client.ObjectKey{Namespace: j.Config.Namespace, Name: nexussecret.Name(key.Namespace, key.Name)}, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	case !nexussecret.OwnedBy(s, key.Namespace, key.Name):
		return nil, nil
	}
	return s, nil
}

// ownerOf returns the NexusAccess a user or role of this cluster names in
// its markers; ok is false for objects that are not this cluster's.
func (j *NexusJanitor) ownerOf(id, markers string) (types.NamespacedName, bool) {
	if !nexus.OwnsName(j.Config.ClusterName, id) {
		return types.NamespacedName{}, false
	}
	c, ns, name, ok := parseNexusOwner(markers)
	if !ok || c != j.Config.ClusterName {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: ns, Name: name}, true
}

func (j *NexusJanitor) sweepUsers(ctx context.Context, users []nexus.User, secrets map[string]*corev1.Secret, secretsKnown bool, owners map[types.NamespacedName]*nexusv1alpha1.NexusAccess) {
	logger := log.FromContext(ctx).WithName("nexus-janitor")
	for i := range users {
		u := &users[i]
		if _, _, ok := nexus.ParseUserID(u.UserID); !ok || u.Source != nexus.DefaultSource {
			continue
		}
		key, ok := j.ownerOf(u.UserID, u.FirstName+" "+u.LastName)
		if !ok {
			continue
		}
		owner := owners[key]
		var secret *corev1.Secret
		if s := secrets[nexussecret.Name(key.Namespace, key.Name)]; owner != nil && s != nil && nexussecret.OwnedBy(s, key.Namespace, key.Name) {
			secret = s
		}
		if j.userVerdict(u, owner, secret, secretsKnown, users) == "" {
			continue
		}
		live, err := j.liveOwner(ctx, key)
		if err != nil {
			logger.Error(err, "failed to check NexusAccess existence", "nexusaccess", key.String(), "user", u.UserID)
			continue
		}
		var liveSecret *corev1.Secret
		if live != nil {
			if liveSecret, err = j.liveSecret(ctx, key); err != nil {
				logger.Error(err, "failed to read the NexusAccess's Secret", "nexusaccess", key.String(), "user", u.UserID)
				continue
			}
		}
		why := j.userVerdict(u, live, liveSecret, true, users)
		if why == "" {
			continue
		}
		logger.Info("deleting Nexus user", "user", u.UserID, "nexusaccess", key.String(), "reason", why)
		if err := j.Nexus.DeleteUser(ctx, u.UserID); err != nil {
			logger.Error(err, "failed to delete Nexus user", "user", u.UserID)
		}
	}
}

// userVerdict says why u must go given its owner (nil = gone) and the
// owner's Secret (nil = absent), or "" when it stays. secretsKnown false
// means the Secrets could not be listed: a user of the owner's current
// identity then stays. users is the sweep's user listing.
func (j *NexusJanitor) userVerdict(u *nexus.User, owner *nexusv1alpha1.NexusAccess, secret *corev1.Secret, secretsKnown bool, users []nexus.User) string {
	switch {
	case owner == nil:
		return "owning NexusAccess is gone"
	case !j.Config.Selects(owner):
		return "owning NexusAccess is no longer selected by this bridge"
	}
	identity, err := nexus.IdentityName(j.Config.ClusterName, owner.Spec.ServiceAccountRef.Namespace, owner.Spec.ServiceAccountRef.Name)
	if err != nil {
		return "owning NexusAccess's serviceAccountRef maps to no Nexus name"
	}
	if id, _, _ := nexus.ParseUserID(u.UserID); id != identity {
		return "owning NexusAccess now uses identity " + identity
	}
	if !secretsKnown {
		return ""
	}
	current := secretUserID(secret)
	switch {
	case current == u.UserID:
		return ""
	case secret != nil && nexussecret.PendingUserID(secret) == u.UserID:
		// Being created (ADR-0036 decision c).
		return ""
	}
	if id, after, ok := retiring(secret); ok && id == u.UserID {
		if j.now().Before(after) {
			return ""
		}
		return "superseded by a rotation, and its grace has passed"
	}
	if !u.Status.Active() && !slices.ContainsFunc(users, func(x nexus.User) bool { return current != "" && x.UserID == current }) {
		// Disabled or locked by an administrator while the Secret names no
		// existing user: the reconciler reports it as UserDisabled
		// instead of replacing it, and it authenticates nothing.
		return ""
	}
	return "not the user its NexusAccess's Secret names"
}

func (j *NexusJanitor) sweepRoles(ctx context.Context, roles []nexus.Role, owners map[types.NamespacedName]*nexusv1alpha1.NexusAccess) {
	logger := log.FromContext(ctx).WithName("nexus-janitor")
	for i := range roles {
		role := &roles[i]
		key, ok := j.ownerOf(role.ID, role.Description)
		if !ok || j.roleVerdict(role, owners[key]) == "" {
			continue
		}
		live, err := j.liveOwner(ctx, key)
		if err != nil {
			logger.Error(err, "failed to check NexusAccess existence", "nexusaccess", key.String(), "role", role.ID)
			continue
		}
		why := j.roleVerdict(role, live)
		if why == "" {
			continue
		}
		logger.Info("deleting Nexus role", "role", role.ID, "nexusaccess", key.String(), "reason", why)
		if err := j.Nexus.DeleteRole(ctx, role.ID); err != nil {
			logger.Error(err, "failed to delete Nexus role", "role", role.ID)
		}
	}
}

// roleVerdict says why role must go given its owner (nil = gone), or "".
func (j *NexusJanitor) roleVerdict(role *nexus.Role, owner *nexusv1alpha1.NexusAccess) string {
	switch {
	case owner == nil:
		return "owning NexusAccess is gone"
	case !j.Config.Selects(owner):
		return "owning NexusAccess is no longer selected by this bridge"
	}
	identity, err := nexus.IdentityName(j.Config.ClusterName, owner.Spec.ServiceAccountRef.Namespace, owner.Spec.ServiceAccountRef.Name)
	if err != nil {
		return "owning NexusAccess's serviceAccountRef maps to no Nexus name"
	}
	if role.ID != identity {
		return "owning NexusAccess now uses role " + identity
	}
	return ""
}

// sweepSecrets deletes Nexus Secrets of this cluster that no NexusAccess
// of this bridge reads (see Janitor.sweepSecrets).
func (j *NexusJanitor) sweepSecrets(ctx context.Context, secrets map[string]*corev1.Secret, owners map[types.NamespacedName]*nexusv1alpha1.NexusAccess) {
	logger := log.FromContext(ctx).WithName("nexus-janitor")
	for _, s := range secrets {
		ns, name, ok := nexussecret.Owner(s)
		if !ok {
			continue
		}
		key := types.NamespacedName{Namespace: ns, Name: name}
		if j.secretVerdict(s, owners[key]) == "" {
			continue
		}
		live, err := j.liveOwner(ctx, key)
		if err != nil {
			logger.Error(err, "failed to check NexusAccess existence", "nexusaccess", key.String(), "secret", s.Name)
			continue
		}
		why := j.secretVerdict(s, live)
		if why == "" {
			continue
		}
		logger.Info("deleting orphan Nexus Secret", "reason", why, "secret", s.Name, "nexusaccess", key.String())
		if err := j.Client.Delete(ctx, s, client.Preconditions{UID: &s.UID}); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to delete orphan Nexus Secret", "secret", s.Name)
		}
	}
}

func (j *NexusJanitor) secretVerdict(s *corev1.Secret, owner *nexusv1alpha1.NexusAccess) string {
	switch {
	case owner == nil:
		return "owning NexusAccess gone"
	case !j.Config.Selects(owner):
		return "owning NexusAccess no longer selected by this bridge"
	case s.Name != nexussecret.Name(owner.Namespace, owner.Name):
		return "not at the name its NexusAccess uses (" + nexussecret.Name(owner.Namespace, owner.Name) + ")"
	}
	return ""
}

// releaseUnselected removes this bridge's finalizers from NexusAccess
// objects it no longer selects once no user or role of theirs is left in
// Nexus, read again after the owner listing (see Janitor.releaseUnselected).
func (j *NexusJanitor) releaseUnselected(ctx context.Context, owners map[types.NamespacedName]*nexusv1alpha1.NexusAccess) {
	if j.Config.NexusFinalizer() == NexusFinalizerName {
		return
	}
	logger := log.FromContext(ctx).WithName("nexus-janitor")
	type candidate struct {
		nxa     *nexusv1alpha1.NexusAccess
		release []string
	}
	var candidates []candidate
	for _, nxa := range owners {
		if j.Config.Selects(nxa) {
			continue
		}
		servedHere := nxa.Status.User != nil && j.Config.statusUserIsOurs(nxa)
		var release []string
		for _, f := range j.Config.NexusReleasedFinalizers() {
			if controllerutil.ContainsFinalizer(nxa, f) && (f != NexusFinalizerName || servedHere) {
				release = append(release, f)
			}
		}
		if len(release) > 0 {
			candidates = append(candidates, candidate{nxa, release})
		}
	}
	if len(candidates) == 0 {
		return
	}
	cluster := j.Config.ClusterName
	users, err := j.Nexus.ListUsers(ctx, nexus.ClusterPrefix(cluster))
	if err != nil {
		logger.Error(err, "failed to list Nexus users before releasing NexusAccess objects no longer selected")
		return
	}
	roles, err := j.Nexus.ListRoles(ctx, nexus.ClusterPrefix(cluster))
	if err != nil {
		logger.Error(err, "failed to list Nexus roles before releasing NexusAccess objects no longer selected")
		return
	}
	for _, c := range candidates {
		nxa, key := c.nxa, types.NamespacedName{Namespace: c.nxa.Namespace, Name: c.nxa.Name}
		if slices.ContainsFunc(users, func(u nexus.User) bool { return nexusUserOwnedBy(cluster, &u, nxa.Namespace, nxa.Name) }) ||
			slices.ContainsFunc(roles, func(r nexus.Role) bool { return nexusRoleOwnedBy(cluster, &r, nxa.Namespace, nxa.Name) }) {
			logger.Info("keeping the finalizer of a NexusAccess no longer selected until its Nexus users and role are revoked", "nexusaccess", key.String())
			continue
		}
		patch := client.MergeFromWithOptions(nxa.DeepCopy(), client.MergeFromWithOptimisticLock{})
		for _, f := range c.release {
			controllerutil.RemoveFinalizer(nxa, f)
		}
		if err := j.Client.Patch(ctx, nxa, patch); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "failed to release NexusAccess no longer selected", "nexusaccess", key.String())
			continue
		}
		logger.Info("released NexusAccess no longer selected by this bridge", "nexusaccess", key.String(), "finalizers", c.release)
	}
}
