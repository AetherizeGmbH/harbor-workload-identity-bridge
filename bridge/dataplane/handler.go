// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// CredentialsPath is the HTTP path the plugin POSTs to.
const CredentialsPath = "/v1/credentials"

// maxRequestBodyBytes caps the credential-request body. The body carries
// only an image reference for audit logging; a few KiB is generous. The
// cap is the defense against an unauthenticated memory-exhaustion DoS:
// the endpoint is reachable on every node's NodePort (ADR-0008), the body
// is decoded before the SA token is verified, and a caller need only send
// a dummy "Authorization: Bearer x" header to reach the json decode. Without
// this bound a single large body can OOM the bridge. See AUDIT.md F1.
const maxRequestBodyBytes = 64 << 10 // 64 KiB

// cacheKeyTypeRegistry is the cacheKeyType we emit in every successful
// CredentialProviderResponse. The kubelet API restricts this field to
// {"Image", "Registry", "Global"}; "Registry" matches our credential model
// (one Harbor robot per HarborAccess CR has permissions across one project,
// so all repos sharing the same registry host can re-use the same creds).
// NOTE: this is DIFFERENT from the kubelet credential-provider config's
// `tokenAttributes.cacheType: ServiceAccount` (ADR-0006). With
// tokenAttributes set, kubelet keys its credential cache per ServiceAccount
// (namespace, name, UID, listed annotations) under either cacheType;
// `Token` would add the token's hash. This picks the image, registry or
// global part of the same key.
const cacheKeyTypeRegistry = "Registry"

// Request is the HTTP API the kubelet plugin POSTs to the bridge. The SA
// token rides in the Authorization: Bearer header; the body carries only
// audit information about the image being pulled.
type Request struct {
	// Image is the image reference the kubelet wants to pull. Used only
	// for audit logging — credential decisions are made from the SA
	// token's aud/sub claims (no per-image cache key, no per-image
	// permission decision).
	Image string `json:"image"`
}

// Response is the HTTP API response. The plugin translates this to the
// kubelet's CredentialProviderResponse shape on the plugin side. Per
// ADR-0013 the credentials are the robot's Basic Auth credentials —
// containerd does the registry auth handshake itself.
type Response struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ExpiresInSecs int    `json:"expires_in"`
	CacheKeyType  string `json:"cache_key_type"`
}

// HandlerConfig is the small set of knobs the handler needs alongside its
// dependencies. Kept as a separate struct rather than embedded in Handler
// so a test can construct one without supplying every field by name.
type HandlerConfig struct {
	// BridgeNamespace is where robot-credential Secrets live (ADR-0011).
	BridgeNamespace string

	// ForceLocalValidation gates whether the data plane performs full
	// local OIDC validation. Always effectively true today; the false
	// path is reserved for after upstream Harbor implements OIDC trust
	// policies (goharbor/harbor#17520). See PHASES.md.
	ForceLocalValidation bool

	// Audience is the only token audience served (ADR-0026). Empty
	// serves nothing.
	Audience string

	// RobotUsername returns the username the control plane stores in the
	// robot Secret of the given ServiceAccount: the Harbor robot prefix
	// plus the robot name (ADR-0018). A Secret holding another username is
	// not served. Required; nil serves nothing.
	RobotUsername func(saNamespace, saName string) (string, error)
}

// Handler is the HTTP handler that validates an SA token, looks up the
// matching HarborAccess CR, and returns the robot's Basic Auth
// credentials. See ADR-0013 for why we return Basic Auth rather than
// pre-minted JWTs.
type Handler struct {
	K8sClient client.Client
	Validator Validator
	Config    HandlerConfig

	// Metrics is optional. When nil, the handler operates without
	// instrumenting requests — tests that do not care about metrics
	// keep their fixtures slim. main.go always sets this.
	Metrics *Metrics

	// Now is the time source used to bound the credential cache duration.
	// nil means time.Now.
	Now func() time.Time

	// Limiter bounds requests per source IP; nil means no limit.
	Limiter *SourceLimiter

	// Audit receives one line per request that reached token validation:
	// issued, denied, or unavailable (a 503 or 500 without credentials,
	// also for a token that could not be judged because the signing keys
	// were unavailable), with the caller's attribution. Refusals before
	// that are not logged, so a flood cannot flood the log: a rate-limited
	// request, a missing bearer or a bad body is counted in the metrics, a
	// wrong method or path is not counted either. main wires a logger
	// fixed at info level, so BRIDGE_LOG_LEVEL=warn cannot silence the
	// audit trail. Unset falls back to the request logger.
	Audit logr.Logger
}

// maxAuditImageLen bounds the caller-supplied image reference in audit
// lines; the body may carry up to 64 KiB.
const maxAuditImageLen = 512

func (h *Handler) audit(fallback logr.Logger) logr.Logger {
	if h.Audit.GetSink() != nil {
		return h.Audit
	}
	return fallback
}

// callerFields attributes a request: the TCP source and, with mTLS, the
// client certificate's subject.
func callerFields(r *http.Request) []any {
	fields := []any{"source", sourceIP(r)}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		fields = append(fields, "client_cert", r.TLS.PeerCertificates[0].Subject.String())
	}
	return fields
}

// claimFields attributes a validated token to its pod and node.
func claimFields(c *Claims) []any {
	return []any{"subject", c.Subject, "pod", c.Pod, "pod_uid", c.PodUID, "node", c.Node}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()
	logger := log.FromContext(ctx).WithName("dataplane")

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != CredentialsPath {
		http.NotFound(w, r)
		return
	}
	// Before any parsing or token validation: the cheapest possible
	// refusal. Not logged per request, or a flood would flood the log.
	if !h.Limiter.Allow(sourceIP(r)) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		h.recordResult(ResultRateLimited)
		return
	}
	caller := callerFields(r)

	// Every return below this point is one request from a metrics
	// perspective; observe duration on exit.
	defer func() {
		if h.Metrics != nil {
			h.Metrics.IssuanceDuration.Observe(time.Since(start).Seconds())
		}
	}()

	if !h.Config.ForceLocalValidation {
		// Plumbed but not implemented (PHASES.md / ADR-0009). The
		// alternative path is "Harbor validates OIDC itself" — only
		// available once goharbor/harbor#17520 lands.
		http.Error(w, "alternative validation path not yet implemented; set forceLocalValidation=true",
			http.StatusNotImplemented)
		h.recordResult(ResultServerError)
		return
	}

	rawToken, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || rawToken == "" {
		http.Error(w, "missing Bearer credential", http.StatusUnauthorized)
		h.recordResult(ResultUnauthorized)
		return
	}

	var req Request
	if r.Body != nil && r.ContentLength != 0 {
		defer func() { _ = r.Body.Close() }()
		// Bound the body before decoding: it is attacker-reachable and
		// parsed before token validation (AUDIT.md F1).
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// Body is optional but if present must parse — otherwise the
			// audit log loses the image and we shouldn't pretend.
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			h.recordResult(ResultBadRequest)
			return
		}
	}

	// 1. Validate the SA token (signature, expiry, issuer, lifetime, pod
	// binding; ADR-0028).
	claims, err := h.Validator.Validate(ctx, rawToken)
	if errors.Is(err, ErrSigningKeysUnavailable) {
		// The bridge could not judge the token: an outage on the JWKS
		// side, not a bad token. 503 lets the plugin retry and then fail
		// visibly, where a 401 would read as a refusal.
		h.audit(logger).Info("credential unavailable", append(caller,
			"reason", "signing_keys_unavailable", "err", truncate(err.Error(), 200),
			"requested_image", truncate(req.Image, maxAuditImageLen))...)
		http.Error(w, "token signing keys unavailable; retry", http.StatusServiceUnavailable)
		h.recordOIDCFailure(OIDCReasonKeysUnavailable)
		h.recordResult(ResultUnavailable)
		return
	}
	if err != nil {
		category := classifyOIDCError(err)
		h.audit(logger).Info("credential denied", append(caller,
			"reason", "invalid_token", "category", category, "err", truncate(err.Error(), 200),
			"requested_image", truncate(req.Image, maxAuditImageLen))...)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		h.recordOIDCFailure(category)
		h.recordResult(ResultUnauthorized)
		return
	}

	// 2. Find the HarborAccess whose serviceAccountRef-derived subject
	// matches claims.sub AND whose trustPolicy.audience appears in
	// claims.aud.
	matched, audMatched, deleting, err := h.findHarborAccess(ctx, claims)
	if err != nil {
		logger.Error(err, "list HarborAccess", "subject", claims.Subject)
		h.audit(logger).Info("credential unavailable", append(append(caller, claimFields(claims)...),
			"reason", "harboraccess_lookup_failed", "err", truncate(err.Error(), 200),
			"requested_image", truncate(req.Image, maxAuditImageLen))...)
		http.Error(w, "internal error", http.StatusInternalServerError)
		if h.Metrics != nil {
			h.Metrics.HarborAccessLookupFailures.Inc()
		}
		h.recordResult(ResultServerError)
		return
	}
	if matched == nil && deleting != nil {
		// Deleting a HarborAccess is the revocation. Its robot may outlive
		// the deletion for a while (Harbor unreachable: DeletionBlocked),
		// but no new node or pod gets its password from here.
		h.audit(logger).Info("credential denied", append(append(caller, claimFields(claims)...),
			"reason", "harboraccess_deleting", "harboraccess", deleting.Namespace+"/"+deleting.Name,
			"requested_image", truncate(req.Image, maxAuditImageLen))...)
		http.Error(w, "the HarborAccess for the requesting service account is being deleted",
			http.StatusForbidden)
		h.recordResult(ResultForbidden)
		return
	}
	if matched == nil {
		h.audit(logger).Info("credential denied", append(append(caller, claimFields(claims)...),
			"reason", "no_matching_harboraccess", "audiences", claims.Audience,
			"requested_image", truncate(req.Image, maxAuditImageLen))...)
		http.Error(w, "no matching HarborAccess for the requesting service account and audience",
			http.StatusForbidden)
		h.recordResult(ResultForbidden)
		return
	}
	// The reconciler reports such a HarborAccess as InvalidSpec and leaves
	// its robot as it was: spec edits are not applied, the password is not
	// rotated. Serving the robot's Secret would hand out those stale
	// grants. The matched object is refused, not skipped: skipping would
	// silently switch to another HarborAccess for the same identity, with
	// other permissions (see findHarborAccess on several matches).
	if err := specError(matched); err != nil {
		h.audit(logger).Info("credential denied", append(append(caller, claimFields(claims)...),
			"reason", "invalid_harboraccess_spec", "harboraccess", matched.Namespace+"/"+matched.Name,
			"err", truncate(err.Error(), 200),
			"requested_image", truncate(req.Image, maxAuditImageLen))...)
		http.Error(w, "the matching HarborAccess has an invalid spec; see its Ready condition", http.StatusForbidden)
		h.recordResult(ResultForbidden)
		return
	}

	// 3. Read the robot's Basic Auth credentials from the bridge-namespace
	// Secret (ADR-0011) and hand them to the plugin. Containerd will do
	// the registry auth handshake itself (ADR-0013).
	creds, err := h.readRobotSecret(ctx, matched)
	if err != nil {
		if errors.Is(err, errSecretOwnerMismatch) {
			// Secret-name collision (AUDIT.md F2): the Secret at the
			// expected name belongs to a different HarborAccess. Deny —
			// never cross-wire one workload's credentials to another.
			logger.Error(err, "robot Secret owner mismatch; refusing to issue credentials",
				"harboraccess", matched.Namespace+"/"+matched.Name)
			h.audit(logger).Info("credential denied", append(append(caller, claimFields(claims)...),
				"reason", "secret_owner_mismatch", "harboraccess", matched.Namespace+"/"+matched.Name,
				"requested_image", truncate(req.Image, maxAuditImageLen))...)
			http.Error(w, "credential Secret ownership mismatch", http.StatusForbidden)
			h.recordResult(ResultForbidden)
			return
		}
		if apierrors.IsNotFound(err) {
			// Robot Secret should exist whenever the CR is Ready; absence
			// means the control plane is mid-rotation or has not yet
			// caught up. 503 lets the plugin retry with backoff rather
			// than fail the workload outright.
			h.audit(logger).Info("credential unavailable", append(append(caller, claimFields(claims)...),
				"reason", "secret_missing", "harboraccess", matched.Namespace+"/"+matched.Name,
				"requested_image", truncate(req.Image, maxAuditImageLen))...)
			http.Error(w, "credentials not yet available; retry", http.StatusServiceUnavailable)
			if h.Metrics != nil {
				h.Metrics.RobotSecretMissing.Inc()
			}
			h.recordResult(ResultUnavailable)
			return
		}
		logger.Error(err, "read robot Secret",
			"harboraccess", matched.Namespace+"/"+matched.Name)
		h.audit(logger).Info("credential unavailable", append(append(caller, claimFields(claims)...),
			"reason", "secret_unreadable", "harboraccess", matched.Namespace+"/"+matched.Name,
			"err", truncate(err.Error(), 200),
			"requested_image", truncate(req.Image, maxAuditImageLen))...)
		http.Error(w, "internal error", http.StatusInternalServerError)
		h.recordResult(ResultServerError)
		return
	}

	// 4. The Secret must hold the robot of the CR's current identity.
	// After a serviceAccountRef change it holds the previous identity's
	// robot until the control plane has created the new one, and deletes
	// the old one right after. Handing that password to the new identity
	// would leave kubelet caching a dead password for up to tokenTTL
	// (ADR-0023); 503 makes the plugin retry and cache nothing.
	want, err := h.robotUsername(matched)
	if err != nil {
		logger.Error(err, "robot username for HarborAccess",
			"harboraccess", matched.Namespace+"/"+matched.Name)
		h.audit(logger).Info("credential unavailable", append(append(caller, claimFields(claims)...),
			"reason", "robot_name_unknown", "harboraccess", matched.Namespace+"/"+matched.Name,
			"err", truncate(err.Error(), 200),
			"requested_image", truncate(req.Image, maxAuditImageLen))...)
		http.Error(w, "internal error", http.StatusInternalServerError)
		h.recordResult(ResultServerError)
		return
	}
	if creds.username != want {
		h.audit(logger).Info("credential unavailable", append(append(caller, claimFields(claims)...),
			"reason", "secret_for_previous_identity", "harboraccess", matched.Namespace+"/"+matched.Name,
			"robot", creds.username, "expected_robot", want,
			"requested_image", truncate(req.Image, maxAuditImageLen))...)
		http.Error(w, "credentials not yet available; retry", http.StatusServiceUnavailable)
		h.recordResult(ResultUnavailable)
		return
	}

	// 5. Tell kubelet how long it may cache these credentials: the CR's
	// spec.tokenTTL, cut short so no cache outlives the password. The
	// control plane promises (rotation-not-before) not to rotate before a
	// given instant; caching past it would hand containerd a dead password
	// after the scheduled rotation until the cache expired (ADR-0023).
	ttl := cacheDuration(matched.Spec.TokenTTL.Duration, creds.rotationNotBefore, creds.hasRotationNotBefore, h.now())

	h.writeResponse(w, creds, ttl)
	auditIssuance(h.audit(logger), caller, claims, matched, audMatched, &req, creds.username, ttl)
	h.recordResult(ResultOK)
}

// recordResult is the single metrics-increment helper for the Issuances
// counter. Centralised so the label-value contract is enforced in one
// place; new return sites must add a recordResult call.
func (h *Handler) recordResult(result string) {
	if h.Metrics == nil {
		return
	}
	h.Metrics.Issuances.WithLabelValues(result).Inc()
}

// recordOIDCFailure counts a Validate failure under its classifyOIDCError
// category.
func (h *Handler) recordOIDCFailure(category string) {
	if h.Metrics == nil {
		return
	}
	h.Metrics.OIDCValidationFailures.WithLabelValues(category).Inc()
}

// findHarborAccess returns the HarborAccess CR whose serviceAccountRef
// matches the token's sub AND whose trustPolicy.audience appears in the
// token's aud claim. Returns a nil CR when nothing matches.
//
// A CR that is being deleted (deletionTimestamp set) never matches:
// deletion is the revocation, and its finalizer can hold it in the cache
// for as long as Harbor refuses the robot's deletion. When nothing else
// matches, the first such CR is returned as deleting, for the audit log.
//
// The audience value that matched is also returned so the audit log
// records the exact aud string the kubelet projected the token with.
//
// Selection is deterministic (AUDIT.md F7). In a correct configuration
// exactly one CR matches a given (subject, issuer, audience). If two or
// more match, that is an operator misconfiguration — two CRs claim the
// same workload identity, typically with different permission sets.
// Returning whichever CR k8s.List happened to yield first would let the
// effective permission set flip between bridge restarts and between the
// two HA replicas (List order is not stable across informer caches), so
// a workload could intermittently receive a more- or less-privileged
// robot than intended. We therefore pick the namespace/name-sorted first
// match and log the ambiguity so an operator can resolve it.
func (h *Handler) findHarborAccess(ctx context.Context, claims *Claims) (matched *harborv1alpha1.HarborAccess, audience string, deleting *harborv1alpha1.HarborAccess, err error) {
	var list harborv1alpha1.HarborAccessList
	if err := h.K8sClient.List(ctx, &list); err != nil {
		return nil, "", nil, fmt.Errorf("list HarborAccess: %w", err)
	}
	type match struct {
		ha  *harborv1alpha1.HarborAccess
		aud string
	}
	var matches, deletingMatches []match
	for i := range list.Items {
		ha := &list.Items[i]
		// Defense-in-depth (AUDIT.md F13): a CR with an empty audience or
		// issuer must never match. The CRD enforces MinLength=1 on both
		// trustPolicy.audience and trustPolicy.issuer, but the data plane is
		// the security boundary and must not rely solely on CRD validation
		// (a CR applied with --validate=false, or a future API revision that
		// relaxes the marker, would otherwise let an empty trustPolicy.audience
		// match a token carrying aud:"" — a silent auth bypass).
		if ha.Spec.TrustPolicy.Audience == "" || ha.Spec.TrustPolicy.Issuer == "" {
			continue
		}
		// The bridge serves exactly one audience (ADR-0026). The reconciler
		// already marks a CR with another audience not ready; the data
		// plane, as the security boundary, never matches one either. An
		// unset configured audience matches nothing (fail closed).
		if ha.Spec.TrustPolicy.Audience != h.Config.Audience {
			continue
		}
		expectedSub := "system:serviceaccount:" + ha.Spec.ServiceAccountRef.Namespace + ":" + ha.Spec.ServiceAccountRef.Name
		if expectedSub != claims.Subject {
			continue
		}
		// Defense-in-depth (AUDIT.md F5): the Validator already pins iss to
		// the bridge's configured issuer, and the reconciler refuses to
		// provision a robot for a CR whose trustPolicy.issuer disagrees with
		// the cluster issuer. Re-checking here means a CR is never matched
		// against a token from an issuer it did not declare, even if those
		// upstream invariants regress.
		if ha.Spec.TrustPolicy.Issuer != claims.Issuer {
			continue
		}
		for _, aud := range claims.Audience {
			// Never honor an empty aud entry, even against a (guarded-above)
			// non-empty CR audience — keeps the match total over both sides.
			if aud == "" {
				continue
			}
			if aud == ha.Spec.TrustPolicy.Audience {
				if ha.DeletionTimestamp.IsZero() {
					matches = append(matches, match{ha: ha, aud: aud})
				} else {
					deletingMatches = append(deletingMatches, match{ha: ha, aud: aud})
				}
				break
			}
		}
	}
	byName := func(ms []match) {
		sort.Slice(ms, func(i, j int) bool {
			if ms[i].ha.Namespace != ms[j].ha.Namespace {
				return ms[i].ha.Namespace < ms[j].ha.Namespace
			}
			return ms[i].ha.Name < ms[j].ha.Name
		})
	}
	if len(matches) == 0 {
		if len(deletingMatches) == 0 {
			return nil, "", nil, nil
		}
		byName(deletingMatches)
		return nil, "", deletingMatches[0].ha, nil
	}
	byName(matches)
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.ha.Namespace + "/" + m.ha.Name
		}
		log.FromContext(ctx).WithName("dataplane").Info(
			"multiple HarborAccess CRs match this token; selecting deterministically by namespace/name — resolve this ambiguity, the matched CRs grant potentially different permissions",
			"subject", claims.Subject,
			"audience", matches[0].aud,
			"matches", strings.Join(names, ","),
			"selected", names[0],
		)
	}
	return matches[0].ha, matches[0].aud, nil, nil
}

// specError reports a spec the reconciler rejects as InvalidSpec although
// the CRD admitted it: the rule that now rejects it did not exist when the
// object was stored.
func specError(ha *harborv1alpha1.HarborAccess) error {
	if err := ha.Spec.TokenTTL.Err(); err != nil {
		return fmt.Errorf("spec.tokenTTL: %w", err)
	}
	return nil
}

// robotCreds carries the username and password read from a robot Secret.
// Private to keep the wire response struct from accidentally accreting
// credentials fields.
type robotCreds struct {
	username string
	password string

	rotationNotBefore    time.Time
	hasRotationNotBefore bool
}

// cacheDuration bounds tokenTTL by the rotation promise. A Secret without
// the promise (written by an older bridge) keeps the plain tokenTTL; a
// promise that has already passed yields 0, which tells kubelet not to
// cache at all until the control plane rotates.
func cacheDuration(tokenTTL time.Duration, notBefore time.Time, hasNotBefore bool, now time.Time) time.Duration {
	ttl := tokenTTL
	if ttl <= 0 {
		ttl = time.Hour
	}
	if !hasNotBefore {
		return ttl
	}
	remaining := notBefore.Sub(now)
	if remaining <= 0 {
		return 0
	}
	if remaining < ttl {
		// Round DOWN to whole seconds: the wire field is seconds, and
		// rounding up could overshoot the promise by up to a second.
		return remaining.Truncate(time.Second)
	}
	return ttl
}

// robotUsername is the username the robot Secret of ha must hold.
func (h *Handler) robotUsername(ha *harborv1alpha1.HarborAccess) (string, error) {
	if h.Config.RobotUsername == nil {
		return "", errors.New("HandlerConfig.RobotUsername is not set")
	}
	return h.Config.RobotUsername(ha.Spec.ServiceAccountRef.Namespace, ha.Spec.ServiceAccountRef.Name)
}

func (h *Handler) readRobotSecret(ctx context.Context, ha *harborv1alpha1.HarborAccess) (*robotCreds, error) {
	name := robotsecret.Name(ha.Namespace, ha.Name)
	secret := &corev1.Secret{}
	if err := h.K8sClient.Get(ctx,
		types.NamespacedName{Namespace: h.Config.BridgeNamespace, Name: name},
		secret); err != nil {
		return nil, err
	}
	// Read-path collision backstop (audit F2). If the Secret carries the
	// bridge's ownership labels and they name a DIFFERENT HarborAccess than
	// the one matched for this token, two CRs have collided on one Secret
	// name. Refuse rather than hand one workload's SA the other workload's
	// robot password.
	// Serve only Secrets the control plane wrote (it stamps its labels on
	// every write): a Secret someone else created at that name is never
	// handed out as robot credentials.
	if !robotsecret.IsManaged(secret) {
		return nil, fmt.Errorf("%w: Secret %s/%s is not managed by the bridge", errSecretOwnerMismatch, h.Config.BridgeNamespace, name)
	}
	if robotsecret.StampedForOther(secret, ha.Namespace, ha.Name) {
		return nil, fmt.Errorf("%w: Secret %s/%s is stamped for HarborAccess %s/%s, not %s/%s",
			errSecretOwnerMismatch, h.Config.BridgeNamespace, name,
			secret.Labels[robotsecret.LabelHarborAccessNamespace], secret.Labels[robotsecret.LabelHarborAccessName],
			ha.Namespace, ha.Name)
	}
	user, pass, err := robotsecret.Credentials(secret)
	if err != nil {
		return nil, err
	}
	nb, ok := robotsecret.RotationNotBefore(secret)
	return &robotCreds{username: user, password: pass, rotationNotBefore: nb, hasRotationNotBefore: ok}, nil
}

// errSecretOwnerMismatch is returned by readRobotSecret when the robot Secret
// found at the expected name is stamped as belonging to a different
// HarborAccess than the one matched for this request. This is the read-path
// backstop for the Secret-name collision class (audit F2). The Secret name
// is dot-joined ("robot-<haNs>.<haName>", ADR-0018) and therefore
// injective, so this never fires in normal operation; it remains so that even
// if two distinct CRs ever collapsed to the same Secret name (an invariant
// regression), a token matched to CR A is never handed a Secret stamped for CR B.
var errSecretOwnerMismatch = errors.New("robot Secret owner mismatch")

func (h *Handler) writeResponse(w http.ResponseWriter, creds *robotCreds, ttl time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	// The body carries a live registry password: no intermediary may
	// store it.
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(Response{
		Username:      creds.username,
		Password:      creds.password,
		ExpiresInSecs: int(ttl / time.Second),
		CacheKeyType:  cacheKeyTypeRegistry,
	})
}

// auditIssuance writes the per-request audit log line. Required by
// SECURITY.md (Phase 6 doc) and PHASES.md: one structured line per
// credential issuance, including subject, matched HarborAccess, robot
// name, TTL, and image. logr's WithValues keeps the line greppable by
// any single field.
func auditIssuance(
	logger logr.Logger,
	caller []any,
	claims *Claims,
	matched *harborv1alpha1.HarborAccess,
	audienceMatched string,
	req *Request,
	robotName string,
	ttl time.Duration,
) {
	logger.Info("credential issued", append(append(caller, claimFields(claims)...),
		"audience", audienceMatched,
		"harboraccess", matched.Namespace+"/"+matched.Name,
		"generation", matched.Generation,
		"robot", robotName,
		"ttl_seconds", int(ttl/time.Second),
		// Asserted by the caller, not verified.
		"requested_image", truncate(req.Image, maxAuditImageLen),
	)...)
}
