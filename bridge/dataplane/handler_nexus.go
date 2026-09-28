// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/nexussecret"
)

// serveNexus answers a request with a valid token for an image of the
// Nexus backend from the NexusAccess objects and their nexususer Secrets
// (ADR-0033 decisions d to f). The checks follow the Harbor path's, in the
// same order, with the Nexus Secret contract's differences: the Secret's
// ownership labels are required (no Nexus Secret predates them), a Secret
// marked grants-incomplete is refused, and the identity check compares
// identities rather than one exact username, because every rotation
// replaces the user under a new generation.
func (h *Handler) serveNexus(ctx context.Context, w http.ResponseWriter, logger logr.Logger, caller []any, claims *Claims, req *Request) {
	image := truncate(req.Image, maxAuditImageLen)
	// answer writes the audit line and the HTTP answer of a request that
	// gets no credentials.
	answer := func(status int, body, result, line, reason string, fields ...any) {
		h.audit(logger).Info(line, slices.Concat(caller, claimFields(claims),
			[]any{"reason", reason}, fields, []any{"requested_image", image})...)
		http.Error(w, body, status)
		h.record(AccessKindNexus, result)
	}
	const (
		denied      = "credential denied"
		unavailable = "credential unavailable"
		notYet      = "credentials not yet available; retry"
	)

	matched, audMatched, deleting, err := h.findNexusAccess(ctx, claims)
	if err != nil {
		logger.Error(err, "list NexusAccess", "subject", claims.Subject)
		if h.Metrics != nil && h.Metrics.Nexus != nil {
			h.Metrics.Nexus.NexusAccessLookupFailures.Inc()
		}
		answer(http.StatusInternalServerError, "internal error", ResultServerError, unavailable,
			"nexusaccess_lookup_failed", "err", truncate(err.Error(), 200))
		return
	}
	if matched == nil && deleting != nil {
		// Deleting a NexusAccess is the revocation. Its users may outlive
		// the deletion while Nexus refuses (DeletionBlocked), but no new
		// node or pod gets a password from here.
		answer(http.StatusForbidden, "the NexusAccess for the requesting service account is being deleted",
			ResultForbidden, denied, "nexusaccess_deleting", "nexusaccess", deleting.Namespace+"/"+deleting.Name)
		return
	}
	if matched == nil {
		answer(http.StatusForbidden, "no matching NexusAccess for the requesting service account and audience",
			ResultForbidden, denied, "no_matching_nexusaccess", "audiences", claims.Audience)
		return
	}
	ref := matched.Namespace + "/" + matched.Name
	// The reconciler refuses such an object as InvalidSpec and deletes its
	// Secret and users (ADR-0030 parity). Until that pass has run the
	// Secret from before is still there. Refused, not skipped, as on the
	// Harbor path: skipping would switch to another NexusAccess of the
	// same identity.
	if err := nexusSpecError(matched); err != nil {
		answer(http.StatusForbidden, "the matching NexusAccess has an invalid spec; see its Ready condition",
			ResultForbidden, denied, "invalid_nexusaccess_spec", "nexusaccess", ref, "err", truncate(err.Error(), 200))
		return
	}

	secret, err := h.readNexusSecret(ctx, matched)
	switch {
	case apierrors.IsNotFound(err):
		// The control plane has not written it yet, or a forced rotation
		// is replacing it. 503 lets the plugin retry.
		h.countUserSecretMissing()
		answer(http.StatusServiceUnavailable, notYet, ResultUnavailable, unavailable,
			"secret_missing", "nexusaccess", ref)
		return
	case err != nil:
		logger.Error(err, "read Nexus user Secret", "nexusaccess", ref)
		answer(http.StatusInternalServerError, "internal error", ResultServerError, unavailable,
			"secret_unreadable", "nexusaccess", ref, "err", truncate(err.Error(), 200))
		return
	}
	// Serve only a Secret the control plane wrote for exactly this
	// NexusAccess: a hand-made Secret at that name, a robot Secret, or one
	// stamped for another NexusAccess (a naming collision, ADR-0018) is
	// never handed out.
	if !nexussecret.OwnedBy(secret, matched.Namespace, matched.Name) {
		logger.Error(errors.New("nexus user Secret is not owned by the matched NexusAccess"),
			"refusing to issue credentials", "nexusaccess", ref, "secret", secret.Name)
		answer(http.StatusForbidden, "credential Secret ownership mismatch", ResultForbidden, denied,
			"secret_owner_mismatch", "nexusaccess", ref)
		return
	}
	// A repository the spec names does not exist, so the role lacks its
	// privileges (ADR-0033 decision d). Refused whatever the annotation's
	// value: a partial grant must not look like a working one.
	if missing, incomplete := nexussecret.GrantsIncomplete(secret); incomplete {
		answer(http.StatusForbidden,
			"the matching NexusAccess names a Nexus repository that does not exist; see its Ready condition",
			ResultForbidden, denied, "grants_incomplete", "nexusaccess", ref,
			"missing_repositories", strings.Join(missing, ","))
		return
	}
	// A Secret that only records the user being created has no password;
	// one whose username and user-id record disagree is replaced by the
	// control plane at once, deleting that user (it treats both as
	// incomplete). Neither is served.
	username, password, err := nexussecret.Credentials(secret)
	if err == nil && nexussecret.UserID(secret) != username {
		err = fmt.Errorf("nexus user Secret %s/%s holds user %q but records user %q", secret.Namespace, secret.Name, username, nexussecret.UserID(secret))
	}
	if err != nil {
		h.countUserSecretMissing()
		answer(http.StatusServiceUnavailable, notYet, ResultUnavailable, unavailable,
			"secret_incomplete", "nexusaccess", ref, "err", truncate(err.Error(), 200))
		return
	}

	// The Secret must hold a user of the object's current identity, any
	// generation. After a serviceAccountRef change it holds the previous
	// identity's user until the control plane has created the new one and
	// deletes the old one right after; handing that password to the new
	// identity would leave kubelet caching a dead password (ADR-0023).
	want, err := h.nexusIdentity(matched)
	if err != nil {
		logger.Error(err, "Nexus identity of NexusAccess", "nexusaccess", ref)
		answer(http.StatusInternalServerError, "internal error", ResultServerError, unavailable,
			"nexus_identity_unknown", "nexusaccess", ref, "err", truncate(err.Error(), 200))
		return
	}
	if got, ok := h.Config.Nexus.UserIdentity(username); !ok || got != want {
		answer(http.StatusServiceUnavailable, notYet, ResultUnavailable, unavailable,
			"secret_for_previous_identity", "nexusaccess", ref, "nexus_user", username, "expected_identity", want)
		return
	}

	// Cap kubelet's cache at the rotation promise (ADR-0023). Unlike a
	// robot Secret, no Nexus Secret predates the promise: one without it
	// was edited by hand, and the control plane replaces its user soon,
	// so nothing is cached.
	now := h.now()
	notBefore, ok := nexussecret.RotationNotBefore(secret)
	if !ok {
		notBefore = now
	}
	ttl := cacheDuration(matched.Spec.TokenTTL.Duration, notBefore, true, now)

	creds := &robotCreds{username: username, password: password}
	h.writeResponse(w, creds, ttl)
	h.audit(logger).Info("credential issued", slices.Concat(caller, claimFields(claims), []any{
		"audience", audMatched,
		"nexusaccess", ref,
		"generation", matched.Generation,
		"nexus_user", username,
		"ttl_seconds", int(ttl / time.Second),
		// Asserted by the caller, not verified.
		"requested_image", image,
	})...)
	h.record(AccessKindNexus, ResultOK)
}

func (h *Handler) countUserSecretMissing() {
	if h.Metrics != nil && h.Metrics.Nexus != nil {
		h.Metrics.Nexus.UserSecretMissing.Inc()
	}
}

// findNexusAccess is findHarborAccess for NexusAccess objects: the one
// whose serviceAccountRef is the token's subject and whose trust policy
// the token satisfies, a deleting one when only such match, and the aud
// value that matched (matchAccess).
func (h *Handler) findNexusAccess(ctx context.Context, claims *Claims) (matched *nexusv1alpha1.NexusAccess, audience string, deleting *nexusv1alpha1.NexusAccess, err error) {
	// Through the cache's index and without deep copies, as
	// findHarborAccess: the handler never mutates a listed object.
	var list nexusv1alpha1.NexusAccessList
	if err := h.K8sClient.List(ctx, &list,
		client.MatchingFields{NexusAccessSubjectField: claims.Subject},
		client.UnsafeDisableDeepCopy,
	); err != nil {
		return nil, "", nil, fmt.Errorf("list NexusAccess: %w", err)
	}
	cands := make([]accessCandidate, len(list.Items))
	for i := range list.Items {
		nxa := &list.Items[i]
		cands[i] = accessCandidate{
			namespace: nxa.Namespace,
			name:      nxa.Name,
			deleting:  !nxa.DeletionTimestamp.IsZero(),
			subject:   nexusAccessSubject(nxa),
			issuer:    nxa.Spec.TrustPolicy.Issuer,
			audience:  nxa.Spec.TrustPolicy.Audience,
		}
	}
	m, aud, d := h.matchAccess(ctx, "NexusAccess objects", claims, cands)
	switch {
	case m >= 0:
		return &list.Items[m], aud, nil, nil
	case d >= 0:
		return nil, "", &list.Items[d], nil
	}
	return nil, "", nil, nil
}

// nexusSpecError reports a spec the reconciler refuses as InvalidSpec
// that the CRD cannot rule out: a tokenTTL Go cannot parse (the Duration
// type tolerates it so the cache keeps working), and no repositories (a
// NexusAccess stored without a spec).
func nexusSpecError(nxa *nexusv1alpha1.NexusAccess) error {
	if err := nxa.Spec.TokenTTL.Err(); err != nil {
		return fmt.Errorf("spec.tokenTTL: %w", err)
	}
	if len(nxa.Spec.Repositories) == 0 {
		return errors.New("spec.repositories is empty")
	}
	return nil
}

// nexusIdentity is the Nexus identity name of the NexusAccess's
// ServiceAccount, which every user id of the identity starts with.
func (h *Handler) nexusIdentity(nxa *nexusv1alpha1.NexusAccess) (string, error) {
	n := h.Config.Nexus
	if n == nil || n.IdentityName == nil || n.UserIdentity == nil {
		return "", errors.New("HandlerConfig.Nexus.IdentityName and UserIdentity must be set")
	}
	return n.IdentityName(nxa.Spec.ServiceAccountRef.Namespace, nxa.Spec.ServiceAccountRef.Name)
}

// readNexusSecret reads the NexusAccess's user Secret from the bridge
// namespace through the cache.
func (h *Handler) readNexusSecret(ctx context.Context, nxa *nexusv1alpha1.NexusAccess) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := h.K8sClient.Get(ctx,
		types.NamespacedName{Namespace: h.Config.BridgeNamespace, Name: nexussecret.Name(nxa.Namespace, nxa.Name)},
		secret); err != nil {
		return nil, err
	}
	return secret, nil
}

// NexusAccessSubjectField is the cache index findNexusAccess lists by: the
// ServiceAccount subject a NexusAccess serves. Register it with
// IndexNexusAccessBySubject before the manager starts, and only with the
// Nexus backend: indexing creates the NexusAccess informer.
const NexusAccessSubjectField = "dataplane.subject"

// nexusAccessSubject is the token subject of the ServiceAccount a
// NexusAccess names (injective, as harborAccessSubject).
func nexusAccessSubject(nxa *nexusv1alpha1.NexusAccess) string {
	return "system:serviceaccount:" + nxa.Spec.ServiceAccountRef.Namespace + ":" + nxa.Spec.ServiceAccountRef.Name
}

// IndexNexusAccessBySubject registers NexusAccessSubjectField on the
// manager's cache.
func IndexNexusAccessBySubject(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, &nexusv1alpha1.NexusAccess{}, NexusAccessSubjectField, nexusAccessSubjectIndex)
}

func nexusAccessSubjectIndex(obj client.Object) []string {
	nxa, ok := obj.(*nexusv1alpha1.NexusAccess)
	if !ok {
		return nil
	}
	return []string{nexusAccessSubject(nxa)}
}
