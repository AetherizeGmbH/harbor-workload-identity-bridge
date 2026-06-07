// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
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
// `tokenAttributes.cacheType: ServiceAccount` (ADR-0006) — that controls
// kubelet's SA-token cache, this controls the credential cache.
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

	// 1. Validate the SA token (signature, expiry, issuer).
	claims, err := h.Validator.Validate(ctx, rawToken)
	if err != nil {
		logger.V(1).Info("token validation failed", "err", err.Error(), "image", req.Image)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		h.recordOIDCFailure(err)
		h.recordResult(ResultUnauthorized)
		return
	}

	// 2. Find the HarborAccess whose serviceAccountRef-derived subject
	// matches claims.sub AND whose trustPolicy.audience appears in
	// claims.aud.
	matched, audMatched, err := h.findHarborAccess(ctx, claims)
	if err != nil {
		logger.Error(err, "list HarborAccess", "subject", claims.Subject)
		http.Error(w, "internal error", http.StatusInternalServerError)
		if h.Metrics != nil {
			h.Metrics.HarborAccessLookupFailures.Inc()
		}
		h.recordResult(ResultServerError)
		return
	}
	if matched == nil {
		logger.Info("no matching HarborAccess for request",
			"subject", claims.Subject, "audiences", claims.Audience, "image", req.Image)
		http.Error(w, "no matching HarborAccess for the requesting service account and audience",
			http.StatusForbidden)
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
				"harboraccess", matched.Namespace+"/"+matched.Name, "image", req.Image)
			http.Error(w, "credential Secret ownership mismatch", http.StatusForbidden)
			h.recordResult(ResultForbidden)
			return
		}
		if apierrors.IsNotFound(err) {
			// Robot Secret should exist whenever the CR is Ready; absence
			// means the control plane is mid-rotation or has not yet
			// caught up. 503 lets the plugin retry with backoff rather
			// than fail the workload outright.
			logger.Info("robot Secret not yet available",
				"harboraccess", matched.Namespace+"/"+matched.Name,
				"image", req.Image)
			http.Error(w, "credentials not yet available; retry", http.StatusServiceUnavailable)
			if h.Metrics != nil {
				h.Metrics.RobotSecretMissing.Inc()
			}
			h.recordResult(ResultUnavailable)
			return
		}
		logger.Error(err, "read robot Secret",
			"harboraccess", matched.Namespace+"/"+matched.Name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		h.recordResult(ResultServerError)
		return
	}

	// 4. Tell kubelet how long it may cache these credentials. We use the
	// CR's spec.tokenTTL: if shorter than the reconciler's 24h rotation
	// interval, this bounds the staleness window after a rotation. If
	// longer, a rotation can render kubelet's cached creds stale until
	// the cache expires (the operator chose this tolerance via the spec).
	ttl := matched.Spec.TokenTTL.Duration
	if ttl <= 0 {
		ttl = time.Hour
	}

	h.writeResponse(w, creds, ttl)
	auditIssuance(logger, claims, matched, audMatched, &req, creds.username, ttl)
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

func (h *Handler) recordOIDCFailure(err error) {
	if h.Metrics == nil {
		return
	}
	h.Metrics.OIDCValidationFailures.WithLabelValues(classifyOIDCError(err)).Inc()
}

// findHarborAccess returns the HarborAccess CR whose serviceAccountRef
// matches the token's sub AND whose trustPolicy.audience appears in the
// token's aud claim. Returns (nil, "", nil) when nothing matches.
//
// The audience value that matched is also returned so the audit log
// records the exact aud string the kubelet projected the token with.
func (h *Handler) findHarborAccess(ctx context.Context, claims *Claims) (*harborv1alpha1.HarborAccess, string, error) {
	var list harborv1alpha1.HarborAccessList
	if err := h.K8sClient.List(ctx, &list); err != nil {
		return nil, "", fmt.Errorf("list HarborAccess: %w", err)
	}
	for i := range list.Items {
		ha := &list.Items[i]
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
			if aud == ha.Spec.TrustPolicy.Audience {
				return ha, aud, nil
			}
		}
	}
	return nil, "", nil
}

// robotCreds carries the username and password read from a robot Secret.
// Private to keep the wire response struct from accidentally accreting
// credentials fields.
type robotCreds struct {
	username string
	password string
}

func (h *Handler) readRobotSecret(ctx context.Context, ha *harborv1alpha1.HarborAccess) (*robotCreds, error) {
	name := robotSecretName(ha)
	secret := &corev1.Secret{}
	if err := h.K8sClient.Get(ctx,
		types.NamespacedName{Namespace: h.Config.BridgeNamespace, Name: name},
		secret); err != nil {
		return nil, err
	}
	// Read-path collision backstop (AUDIT.md F2). If the Secret carries the
	// bridge's ownership labels and they name a DIFFERENT HarborAccess than
	// the one matched for this token, two CRs have collided on the
	// dash-joined Secret name. Refuse rather than hand one workload's SA the
	// other workload's robot password.
	if secret.Labels[labelManagedBy] == labelManagedByValue &&
		(secret.Labels[labelHarborAccessNamespace] != ha.Namespace ||
			secret.Labels[labelHarborAccessName] != ha.Name) {
		return nil, fmt.Errorf("%w: Secret %s/%s is stamped for HarborAccess %s/%s, not %s/%s",
			errSecretOwnerMismatch, h.Config.BridgeNamespace, name,
			secret.Labels[labelHarborAccessNamespace], secret.Labels[labelHarborAccessName],
			ha.Namespace, ha.Name)
	}
	user := string(secret.Data["username"])
	pass := string(secret.Data["password"])
	if user == "" || pass == "" {
		return nil, fmt.Errorf("robot Secret %s/%s missing username and/or password keys",
			h.Config.BridgeNamespace, name)
	}
	return &robotCreds{username: user, password: pass}, nil
}

// robotSecretName mirrors controlplane.secretNameFor — kept here as a
// local helper so the data plane does not import the control plane
// (ADR-0002). The format ("robot-<haNs>-<haName>") is the cross-package
// contract; if the control plane ever changes its secret naming it has to
// announce that here too. Worth a future ADR if the contract gets richer.
func robotSecretName(ha *harborv1alpha1.HarborAccess) string {
	return "robot-" + ha.Namespace + "-" + ha.Name
}

// Robot-Secret ownership label keys. Mirrored from controlplane/labels.go —
// the data plane does not import the control plane (ADR-0002), the same way
// robotSecretName mirrors secretNameFor. The reconciler stamps these on every
// robot Secret it writes.
const (
	labelManagedBy             = "harbor.aetherize.io/managed-by"
	labelManagedByValue        = "harbor-workload-identity-bridge"
	labelHarborAccessNamespace = "harbor.aetherize.io/harboraccess-namespace"
	labelHarborAccessName      = "harbor.aetherize.io/harboraccess-name"
)

// errSecretOwnerMismatch is returned by readRobotSecret when the robot Secret
// found at the expected name is stamped as belonging to a different
// HarborAccess than the one matched for this request. This is the read-path
// backstop for the Secret-name collision class (AUDIT.md F2): the Secret name
// "robot-<haNs>-<haName>" is dash-joined and therefore ambiguous, so even if
// two distinct CRs collapse to the same Secret name, a token matched to CR A
// must never receive a Secret stamped for CR B.
var errSecretOwnerMismatch = errors.New("robot Secret owner mismatch")

func (h *Handler) writeResponse(w http.ResponseWriter, creds *robotCreds, ttl time.Duration) {
	w.Header().Set("Content-Type", "application/json")
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
	logger interface {
		Info(msg string, kv ...any)
	},
	claims *Claims,
	matched *harborv1alpha1.HarborAccess,
	audienceMatched string,
	req *Request,
	robotName string,
	ttl time.Duration,
) {
	logger.Info("credential issued",
		"subject", claims.Subject,
		"audience", audienceMatched,
		"harboraccess", matched.Namespace+"/"+matched.Name,
		"generation", matched.Generation,
		"robot", robotName,
		"ttl_seconds", int(ttl/time.Second),
		"image", req.Image,
	)
}
