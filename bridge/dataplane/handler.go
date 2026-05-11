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

// bridgeBearerUsername is the username we put in the credential-provider
// response. Per ADR-0005 the password is a docker registry v2 bearer JWT
// (not the robot's actual password); the username is the docker convention
// for "the password is a bearer token, not a basic-auth password".
//
// Note for Phase 6 e2e: containerd's actual auth flow Basic-Auths the
// credentials to the registry's /service/token endpoint as input to the
// auth handshake. We commit to ADR-0005's intent here and validate end-to-
// end behaviour against a real Harbor in e2e; if containerd rejects this
// shape, ADR-0005 gets superseded by a follow-up that returns the robot's
// basic-auth credentials instead.
const bridgeBearerUsername = "<token>"

// cacheKeyTypeServiceAccount mirrors KEP-4412's CredentialProviderCacheKeyType.
// We always emit this value; see ADR-0006 for why the kubelet credential-
// provider config sets cacheType: ServiceAccount.
const cacheKeyTypeServiceAccount = "ServiceAccount"

// Request is the HTTP API the kubelet plugin POSTs to the bridge.
// The SA token is in the Authorization: Bearer header; the body carries
// only audit information about the image being pulled.
type Request struct {
	// Image is the image reference the kubelet wants to pull. Used only
	// for audit logging — credential decisions are made from the SA
	// token's aud/sub claims, not the image (see ADR-0007).
	Image string `json:"image"`
}

// Response is the HTTP API response. The plugin translates this to the
// kubelet's CredentialProviderResponse shape on the plugin side.
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

	// HarborService is the value of the "service" query parameter passed
	// to Harbor's /service/token endpoint. Harbor's canonical value is
	// "harbor-registry"; the Helm chart can override for non-default
	// deployments.
	HarborService string

	// ForceLocalValidation gates whether the data plane performs full
	// local OIDC validation. Always effectively true today; the false
	// path is reserved for after upstream Harbor implements OIDC trust
	// policies (goharbor/harbor#17520). See PHASES.md.
	ForceLocalValidation bool
}

// Handler is the HTTP handler that validates an SA token, looks up the
// matching HarborAccess CR, and either serves a cached docker bearer JWT
// or mints a fresh one via Harbor's /service/token.
type Handler struct {
	K8sClient client.Client
	Validator Validator
	Cache     DockerTokenCache
	Tokens    HarborTokenClient
	Config    HandlerConfig
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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

	if !h.Config.ForceLocalValidation {
		// Plumbed but not implemented (PHASES.md / ADR-0009). The
		// alternative path is "Harbor validates OIDC itself" — only
		// available once goharbor/harbor#17520 lands.
		http.Error(w, "alternative validation path not yet implemented; set forceLocalValidation=true",
			http.StatusNotImplemented)
		return
	}

	// Authorization: Bearer <SA-token>
	rawToken, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || rawToken == "" {
		http.Error(w, "missing Bearer credential", http.StatusUnauthorized)
		return
	}

	var req Request
	if r.Body != nil && r.ContentLength != 0 {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			// Body is optional but if present must parse — otherwise the
			// audit log loses the image and we shouldn't pretend.
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}

	// 1. Validate the SA token (signature, expiry, issuer).
	claims, err := h.Validator.Validate(ctx, rawToken)
	if err != nil {
		logger.V(1).Info("token validation failed", "err", err.Error(), "image", req.Image)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	// 2. Find the matching HarborAccess.
	matched, audMatched, err := h.findHarborAccess(ctx, claims)
	if err != nil {
		logger.Error(err, "list HarborAccess", "subject", claims.Subject)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if matched == nil {
		logger.Info("no matching HarborAccess for request",
			"subject", claims.Subject, "audiences", claims.Audience, "image", req.Image)
		http.Error(w, "no matching HarborAccess for the requesting service account and audience",
			http.StatusForbidden)
		return
	}

	// 3. Cache lookup. ADR-0007: lazy invalidation via generation in the key.
	key := CacheKey{
		HarborAccessNamespace: matched.Namespace,
		HarborAccessName:      matched.Name,
		Generation:            matched.Generation,
		Subject:               claims.Subject,
	}
	if tok, hit := h.Cache.Get(key); hit {
		h.writeResponse(w, tok)
		auditIssuance(ctx, logger, claims, matched, audMatched, &req, "", tok, true)
		return
	}

	// 4. Cache miss — read the robot's credentials and mint a fresh token.
	creds, err := h.readRobotSecret(ctx, matched)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Robot Secret should exist whenever the CR is Ready; absence
			// means the control plane is mid-rotation or has not yet
			// caught up. 503 lets the plugin retry with backoff rather
			// than fail the workload outright.
			logger.Info("robot Secret not yet available",
				"harboraccess", matched.Namespace+"/"+matched.Name,
				"image", req.Image)
			http.Error(w, "credentials not yet available; retry", http.StatusServiceUnavailable)
			return
		}
		logger.Error(err, "read robot Secret",
			"harboraccess", matched.Namespace+"/"+matched.Name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	scopes := buildScopes(matched.Spec.Permissions)
	tok, err := h.Tokens.Mint(ctx, creds.username, creds.password, h.Config.HarborService, scopes)
	if errors.Is(err, ErrTokenAuth) {
		// ADR-0007 one-shot 401 retry: the password we read may have
		// been rotated since the reconciler wrote it. Re-read and try
		// once more.
		logger.V(1).Info("/service/token returned 401; re-reading Secret and retrying once",
			"harboraccess", matched.Namespace+"/"+matched.Name)
		creds, rerr := h.readRobotSecret(ctx, matched)
		if rerr == nil {
			tok, err = h.Tokens.Mint(ctx, creds.username, creds.password, h.Config.HarborService, scopes)
		}
	}
	if err != nil {
		logger.Error(err, "mint docker token",
			"harboraccess", matched.Namespace+"/"+matched.Name)
		http.Error(w, "credential issuance failed", http.StatusBadGateway)
		return
	}

	// 5. Cache TTL bounded by spec.tokenTTL: the CR can ask for shorter
	// TTL than Harbor would otherwise grant (e.g. for sensitive workloads).
	ttl := tok.ExpiresIn
	if specTTL := matched.Spec.TokenTTL.Duration; specTTL > 0 && specTTL < ttl {
		ttl = specTTL
	}
	h.Cache.Set(key, tok, ttl)

	h.writeResponse(w, tok)
	auditIssuance(ctx, logger, claims, matched, audMatched, &req, creds.username, tok, false)
}

// findHarborAccess returns the HarborAccess CR whose serviceAccountRef
// matches the token's sub AND whose trustPolicy.audience appears in the
// token's aud claim. Returns (nil, "", nil) when nothing matches.
//
// For each match we also return which audience string matched, so the
// audit log records the exact aud value the kubelet projected the token
// with (helpful when a CR's audience and a token's audience use slightly
// different forms).
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

// buildScopes converts the CR's permissions into the docker registry v2
// scope shape. The "pull,push" form on a permission expands to two
// actions in one Scope entry.
func buildScopes(perms []harborv1alpha1.ProjectPermission) []Scope {
	out := make([]Scope, 0, len(perms))
	for _, p := range perms {
		actions := []string{}
		for _, a := range strings.Split(string(p.Action), ",") {
			a = strings.TrimSpace(a)
			if a != "" {
				actions = append(actions, a)
			}
		}
		out = append(out, Scope{
			Type:     "repository",
			Resource: p.Project,
			Actions:  actions,
		})
	}
	return out
}

func (h *Handler) writeResponse(w http.ResponseWriter, tok *DockerToken) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Response{
		Username:      bridgeBearerUsername,
		Password:      tok.Token,
		ExpiresInSecs: int(tok.ExpiresIn / time.Second),
		CacheKeyType:  cacheKeyTypeServiceAccount,
	})
}

// auditIssuance writes the per-request audit log line. Required by
// SECURITY.md (Phase 6 doc) and PHASES.md: one structured line per
// credential issuance, including subject, matched HarborAccess, robot
// name, scopes, TTL, cache outcome, and image. logr's WithValues keeps
// the line greppable by any single field.
func auditIssuance(
	_ context.Context,
	logger interface {
		Info(msg string, kv ...any)
	},
	claims *Claims,
	matched *harborv1alpha1.HarborAccess,
	audienceMatched string,
	req *Request,
	robotName string,
	tok *DockerToken,
	cached bool,
) {
	logger.Info("credential issued",
		"subject", claims.Subject,
		"audience", audienceMatched,
		"harboraccess", matched.Namespace+"/"+matched.Name,
		"generation", matched.Generation,
		"robot", robotName,
		"ttl_seconds", int(tok.ExpiresIn/time.Second),
		"cached", cached,
		"image", req.Image,
	)
}
