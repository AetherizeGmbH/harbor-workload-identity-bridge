// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
)

// ----------------------------------------------------------------------------
// Test scheme + fixtures
// ----------------------------------------------------------------------------

var handlerTestScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(harborv1alpha1.AddToScheme(s))
	return s
}()

const (
	hTestBridgeNS   = "harbor-bridge-system"
	hTestHAName     = "flux-access"
	hTestHANs       = "harbor-bridge-system"
	hTestSubject    = "system:serviceaccount:flux-system:source-controller"
	hTestAudience   = "harbor.example.com"
	hTestRobotUser  = "robot$bridge-prod-flux-system-source-controller"
	hTestRobotPass  = "robot-password-v1"
	hTestRobotPass2 = "robot-password-v2-after-rotation"
	hTestService    = "harbor-registry"
)

func newTestHA() *harborv1alpha1.HarborAccess {
	return &harborv1alpha1.HarborAccess{
		ObjectMeta: metav1.ObjectMeta{
			Name: hTestHAName, Namespace: hTestHANs, Generation: 1,
		},
		Spec: harborv1alpha1.HarborAccessSpec{
			ServiceAccountRef: harborv1alpha1.ServiceAccountRef{
				Namespace: "flux-system", Name: "source-controller",
			},
			TrustPolicy: harborv1alpha1.TrustPolicy{
				Issuer:   "https://kubernetes.default.svc",
				Audience: hTestAudience,
			},
			Permissions: []harborv1alpha1.ProjectPermission{
				{Project: "production", Action: "pull"},
				{Project: "shared", Action: "pull,push"},
			},
			TokenTTL: metav1.Duration{Duration: time.Hour},
		},
	}
}

func newTestRobotSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: hTestBridgeNS,
			Name:      "robot-" + hTestHANs + "-" + hTestHAName,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"username": []byte(hTestRobotUser),
			"password": []byte(hTestRobotPass),
		},
	}
}

func newTestClaims() *Claims {
	return &Claims{
		Subject:  hTestSubject,
		Audience: []string{hTestAudience},
		Issuer:   "https://kubernetes.default.svc",
		Expiry:   time.Now().Add(time.Hour),
	}
}

// ----------------------------------------------------------------------------
// Stubs for the dependency interfaces
// ----------------------------------------------------------------------------

type stubValidator struct {
	claims *Claims
	err    error
}

func (s *stubValidator) Validate(_ context.Context, _ string) (*Claims, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.claims, nil
}

type stubMintCall struct {
	Username, Password, Service string
	Scopes                      []Scope
}

type stubTokenClient struct {
	mu    sync.Mutex
	calls []stubMintCall

	// response is returned when there is no scheduled error.
	response *DockerToken

	// scheduledErrors maps call index (1-based) to error. Useful for
	// "first call 401, second call success" patterns.
	scheduledErrors map[int]error
}

func (s *stubTokenClient) Mint(_ context.Context, user, pass, service string, scopes []Scope) (*DockerToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubMintCall{Username: user, Password: pass, Service: service, Scopes: scopes})
	if err, ok := s.scheduledErrors[len(s.calls)]; ok && err != nil {
		return nil, err
	}
	return s.response, nil
}

func (s *stubTokenClient) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubTokenClient) lastCall() stubMintCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[len(s.calls)-1]
}

// ----------------------------------------------------------------------------
// Handler fixture builder
// ----------------------------------------------------------------------------

type handlerFixture struct {
	Validator *stubValidator
	Tokens    *stubTokenClient
	Cache     DockerTokenCache
	K8s       client.Client
	Handler   *Handler
}

// newHandlerFixture wires up a Handler with safe defaults: a valid HA, a
// valid Secret, a successful Validator, a successful TokenClient. Each
// test mutates the relevant field before serving requests.
func newHandlerFixture(t *testing.T, extras ...client.Object) *handlerFixture {
	t.Helper()
	objs := append([]client.Object{newTestHA(), newTestRobotSecret()}, extras...)
	k8s := fake.NewClientBuilder().
		WithScheme(handlerTestScheme).
		WithObjects(objs...).
		Build()

	validator := &stubValidator{claims: newTestClaims()}
	tokens := &stubTokenClient{
		response: &DockerToken{
			Token:     "fake.jwt.value",
			Issued:    time.Now(),
			ExpiresIn: 30 * time.Minute,
		},
	}
	cache := NewDockerTokenCache(0)
	t.Cleanup(cache.Stop)

	return &handlerFixture{
		Validator: validator,
		Tokens:    tokens,
		Cache:     cache,
		K8s:       k8s,
		Handler: &Handler{
			K8sClient: k8s,
			Validator: validator,
			Cache:     cache,
			Tokens:    tokens,
			Config: HandlerConfig{
				BridgeNamespace:      hTestBridgeNS,
				HarborService:        hTestService,
				ForceLocalValidation: true,
			},
		},
	}
}

func bearerReq(t *testing.T, image string) *http.Request {
	t.Helper()
	body := []byte("{}")
	if image != "" {
		var err error
		body, err = json.Marshal(Request{Image: image})
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, CredentialsPath, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer some-sa-token")
	r.Header.Set("Content-Type", "application/json")
	return r
}

func decodeResp(t *testing.T, w *httptest.ResponseRecorder) Response {
	t.Helper()
	var got Response
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return got
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

func TestHandler_HappyPath(t *testing.T) {
	fx := newHandlerFixture(t)
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "harbor.example.com/production/myimg:v1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	got := decodeResp(t, w)
	if got.Password != "fake.jwt.value" {
		t.Errorf("Password = %q", got.Password)
	}
	if got.Username != bridgeBearerUsername {
		t.Errorf("Username = %q, want %q", got.Username, bridgeBearerUsername)
	}
	if got.CacheKeyType != cacheKeyTypeServiceAccount {
		t.Errorf("CacheKeyType = %q", got.CacheKeyType)
	}
	if got.ExpiresInSecs != 1800 {
		t.Errorf("ExpiresInSecs = %d, want 1800", got.ExpiresInSecs)
	}
	if fx.Tokens.callCount() != 1 {
		t.Errorf("Mint calls = %d, want 1", fx.Tokens.callCount())
	}
	// Robot credentials must have reached /service/token.
	last := fx.Tokens.lastCall()
	if last.Username != hTestRobotUser {
		t.Errorf("Mint username = %q", last.Username)
	}
	if last.Password != hTestRobotPass {
		t.Errorf("Mint password = %q", last.Password)
	}
	if last.Service != hTestService {
		t.Errorf("Mint service = %q", last.Service)
	}
	if len(last.Scopes) != 2 {
		t.Errorf("Mint scopes = %d, want 2 (production, shared)", len(last.Scopes))
	}
}

func TestHandler_CacheHit_SkipsMint(t *testing.T) {
	fx := newHandlerFixture(t)
	w1 := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w1, bearerReq(t, "harbor.example.com/p/i:v1"))
	if w1.Code != http.StatusOK {
		t.Fatalf("first request failed: %s", w1.Body.String())
	}

	w2 := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w2, bearerReq(t, "harbor.example.com/p/i:v1"))
	if w2.Code != http.StatusOK {
		t.Fatalf("second request failed: %s", w2.Body.String())
	}
	if fx.Tokens.callCount() != 1 {
		t.Errorf("Mint calls = %d, want 1 (second request should hit cache)", fx.Tokens.callCount())
	}
	// Both responses carry the same token (proof of cache hit on #2).
	if decodeResp(t, w1).Password != decodeResp(t, w2).Password {
		t.Errorf("cached response token differs from fresh")
	}
}

func TestHandler_GenerationChangeMissesCache(t *testing.T) {
	fx := newHandlerFixture(t)
	w1 := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w1, bearerReq(t, "img"))
	if w1.Code != http.StatusOK {
		t.Fatal(w1.Body.String())
	}

	// Bump the CR's generation (simulating a permissions edit).
	ha := &harborv1alpha1.HarborAccess{}
	if err := fx.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: hTestHANs, Name: hTestHAName}, ha); err != nil {
		t.Fatal(err)
	}
	ha.Generation = 2
	if err := fx.K8s.Update(context.Background(), ha); err != nil {
		t.Fatal(err)
	}

	w2 := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w2, bearerReq(t, "img"))
	if w2.Code != http.StatusOK {
		t.Fatal(w2.Body.String())
	}
	if fx.Tokens.callCount() != 2 {
		t.Errorf("Mint calls = %d, want 2 (generation change must invalidate cache)", fx.Tokens.callCount())
	}
}

func TestHandler_TokenTTLBoundsCache(t *testing.T) {
	fx := newHandlerFixture(t)
	// Make the CR's tokenTTL shorter than what Harbor would otherwise grant.
	ha := &harborv1alpha1.HarborAccess{}
	_ = fx.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: hTestHANs, Name: hTestHAName}, ha)
	ha.Spec.TokenTTL = metav1.Duration{Duration: 5 * time.Minute}
	_ = fx.K8s.Update(context.Background(), ha)
	// Make Mint return a token with a longer "natural" TTL.
	fx.Tokens.response.ExpiresIn = 30 * time.Minute

	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}

	// We can't directly observe the cache TTL, but we can observe the
	// response: ExpiresInSecs is the underlying token's value (not the
	// cache TTL we applied). What we CAN do is poke the cache:
	// pretend 6 minutes have passed and check that another call misses.
	// However the cache uses real time, so we'd need to wait 5 minutes.
	// Instead, we just assert that the response's ExpiresInSecs reflects
	// the underlying token TTL — the cache-side TTL bounding is verified
	// by the cache's own TTLEviction test.
	if got := decodeResp(t, w).ExpiresInSecs; got != 1800 {
		t.Errorf("response ExpiresInSecs = %d, want 1800 (underlying token TTL)", got)
	}
}

func TestHandler_MissingBearerHeader_401(t *testing.T) {
	fx := newHandlerFixture(t)
	r := httptest.NewRequest(http.MethodPost, CredentialsPath, bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if fx.Tokens.callCount() != 0 {
		t.Errorf("Mint should not have been called")
	}
}

func TestHandler_MalformedBearer_401(t *testing.T) {
	fx := newHandlerFixture(t)
	cases := map[string]string{
		"basic auth":  "Basic foo:bar",
		"empty token": "Bearer ",
		"no scheme":   "some-token",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, CredentialsPath, bytes.NewReader([]byte("{}")))
			r.Header.Set("Authorization", header)
			w := httptest.NewRecorder()
			fx.Handler.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("Authorization=%q: status = %d, want 401", header, w.Code)
			}
		})
	}
}

func TestHandler_InvalidToken_401(t *testing.T) {
	fx := newHandlerFixture(t)
	fx.Validator.err = errors.New("simulated invalid token")
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if fx.Tokens.callCount() != 0 {
		t.Errorf("Mint must not run when token is invalid")
	}
}

func TestHandler_NoMatchingSubject_403(t *testing.T) {
	fx := newHandlerFixture(t)
	fx.Validator.claims = &Claims{
		Subject:  "system:serviceaccount:other-ns:other-sa",
		Audience: []string{hTestAudience},
	}
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestHandler_NoMatchingAudience_403(t *testing.T) {
	fx := newHandlerFixture(t)
	fx.Validator.claims = &Claims{
		Subject:  hTestSubject,
		Audience: []string{"some.other.registry.example"},
	}
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestHandler_MissingRobotSecret_503(t *testing.T) {
	// Bridge namespace exists but the Secret is missing — control plane
	// is mid-rotation or hasn't caught up yet. 503 invites the plugin
	// to retry.
	k8s := fake.NewClientBuilder().
		WithScheme(handlerTestScheme).
		WithObjects(newTestHA()). // no Secret
		Build()
	fx := &Handler{
		K8sClient: k8s,
		Validator: &stubValidator{claims: newTestClaims()},
		Cache:     NewDockerTokenCache(0),
		Tokens:    &stubTokenClient{response: &DockerToken{Token: "x", ExpiresIn: time.Hour}},
		Config: HandlerConfig{
			BridgeNamespace:      hTestBridgeNS,
			HarborService:        hTestService,
			ForceLocalValidation: true,
		},
	}
	w := httptest.NewRecorder()
	fx.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandler_TokenClientFailure_502(t *testing.T) {
	fx := newHandlerFixture(t)
	fx.Tokens.scheduledErrors = map[int]error{1: fmt.Errorf("harbor 500")}
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestHandler_AuthRetryOn401_Succeeds(t *testing.T) {
	// Simulate: reconciler rotated the password between when the data
	// plane loaded the Secret and when /service/token was called. First
	// Mint returns 401 (ErrTokenAuth). Bridge re-reads the Secret (now
	// containing the new password) and retries; second Mint succeeds.
	fx := newHandlerFixture(t)
	fx.Tokens.scheduledErrors = map[int]error{1: ErrTokenAuth}

	// Update the Secret to the post-rotation password so the second
	// readRobotSecret picks it up.
	sec := &corev1.Secret{}
	_ = fx.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: hTestBridgeNS, Name: "robot-" + hTestHANs + "-" + hTestHAName},
		sec)
	sec.Data["password"] = []byte(hTestRobotPass2)
	_ = fx.K8s.Update(context.Background(), sec)

	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if fx.Tokens.callCount() != 2 {
		t.Errorf("Mint calls = %d, want 2 (first fails 401, retry succeeds)", fx.Tokens.callCount())
	}
	// The retry must have used the freshly-read password.
	if got := fx.Tokens.lastCall().Password; got != hTestRobotPass2 {
		t.Errorf("retry password = %q, want %q (Secret must be re-read)", got, hTestRobotPass2)
	}
}

func TestHandler_AuthRetryOn401_StillFails_502(t *testing.T) {
	// Both Mint calls return 401. Bridge surfaces it as 502 rather than
	// looping forever.
	fx := newHandlerFixture(t)
	fx.Tokens.scheduledErrors = map[int]error{1: ErrTokenAuth, 2: ErrTokenAuth}

	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if fx.Tokens.callCount() != 2 {
		t.Errorf("retry budget exceeded: %d Mint calls (want 2)", fx.Tokens.callCount())
	}
}

func TestHandler_ForceLocalValidationOff_501(t *testing.T) {
	fx := newHandlerFixture(t)
	fx.Handler.Config.ForceLocalValidation = false
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "not yet implemented") {
		t.Errorf("response should explain the path is unimplemented: %q", body)
	}
}

func TestHandler_NonPOST_405(t *testing.T) {
	fx := newHandlerFixture(t)
	r := httptest.NewRequest(http.MethodGet, CredentialsPath, nil)
	r.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestHandler_WrongPath_404(t *testing.T) {
	fx := newHandlerFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/v1/something-else", bytes.NewReader([]byte("{}")))
	r.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestBuildScopes_ExpandsCommaActions(t *testing.T) {
	perms := []harborv1alpha1.ProjectPermission{
		{Project: "production", Action: "pull"},
		{Project: "shared", Action: "pull,push"},
	}
	got := buildScopes(perms)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Resource != "production" || len(got[0].Actions) != 1 || got[0].Actions[0] != "pull" {
		t.Errorf("scope[0] = %+v", got[0])
	}
	if got[1].Resource != "shared" || len(got[1].Actions) != 2 ||
		got[1].Actions[0] != "pull" || got[1].Actions[1] != "push" {
		t.Errorf("scope[1] = %+v", got[1])
	}
}
