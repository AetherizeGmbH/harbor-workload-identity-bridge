// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	hTestBridgeNS  = "harbor-bridge-system"
	hTestHAName    = "flux-access"
	hTestHANs      = "harbor-bridge-system"
	hTestSubject   = "system:serviceaccount:flux-system:source-controller"
	hTestAudience  = "harbor.example.com"
	hTestRobotUser = "robot$bridge-prod-flux-system-source-controller"
	hTestRobotPass = "robot-password-v1"
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
// Stubs
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

// ----------------------------------------------------------------------------
// Handler fixture builder
// ----------------------------------------------------------------------------

type handlerFixture struct {
	Validator *stubValidator
	K8s       client.Client
	Handler   *Handler
}

func newHandlerFixture(t *testing.T, extras ...client.Object) *handlerFixture {
	t.Helper()
	objs := append([]client.Object{newTestHA(), newTestRobotSecret()}, extras...)
	k8s := fake.NewClientBuilder().
		WithScheme(handlerTestScheme).
		WithObjects(objs...).
		Build()
	validator := &stubValidator{claims: newTestClaims()}
	return &handlerFixture{
		Validator: validator,
		K8s:       k8s,
		Handler: &Handler{
			K8sClient: k8s,
			Validator: validator,
			Config: HandlerConfig{
				BridgeNamespace:      hTestBridgeNS,
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

func TestHandler_HappyPath_ReturnsRobotBasicAuth(t *testing.T) {
	// Per ADR-0013, the response Username and Password are the robot's
	// actual credentials read from the bridge-namespace Secret. Containerd
	// uses these as HTTP Basic Auth to Harbor's /service/token.
	fx := newHandlerFixture(t)
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "harbor.example.com/production/myimg:v1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	got := decodeResp(t, w)
	if got.Username != hTestRobotUser {
		t.Errorf("Username = %q, want %q (robot's actual user, not a bearer marker)", got.Username, hTestRobotUser)
	}
	if got.Password != hTestRobotPass {
		t.Errorf("Password = %q, want %q (robot's actual password)", got.Password, hTestRobotPass)
	}
	if got.CacheKeyType != cacheKeyTypeRegistry {
		t.Errorf("CacheKeyType = %q, want %q", got.CacheKeyType, cacheKeyTypeRegistry)
	}
	// ExpiresInSecs reflects spec.tokenTTL (1h in our fixture).
	if got.ExpiresInSecs != 3600 {
		t.Errorf("ExpiresInSecs = %d, want 3600 (spec.tokenTTL=1h)", got.ExpiresInSecs)
	}
}

func TestHandler_RespectsTokenTTL(t *testing.T) {
	fx := newHandlerFixture(t)
	ha := &harborv1alpha1.HarborAccess{}
	_ = fx.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: hTestHANs, Name: hTestHAName}, ha)
	ha.Spec.TokenTTL = metav1.Duration{Duration: 15 * time.Minute}
	_ = fx.K8s.Update(context.Background(), ha)

	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, ""))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if got := decodeResp(t, w).ExpiresInSecs; got != 900 {
		t.Errorf("ExpiresInSecs = %d, want 900 (15m)", got)
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
		WithObjects(newTestHA()).
		Build()
	h := &Handler{
		K8sClient: k8s,
		Validator: &stubValidator{claims: newTestClaims()},
		Config: HandlerConfig{
			BridgeNamespace:      hTestBridgeNS,
			ForceLocalValidation: true,
		},
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestHandler_SecretInWrongNamespace_503(t *testing.T) {
	// A Secret with the right name but in the wrong namespace must NOT
	// be picked up — ADR-0011's blast-radius story rests on the data
	// plane reading only from the bridge namespace.
	wrongNs := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "some-other-namespace",
			Name:      "robot-" + hTestHANs + "-" + hTestHAName,
		},
		Data: map[string][]byte{
			"username": []byte("attacker-supplied"),
			"password": []byte("attacker-supplied"),
		},
	}
	k8s := fake.NewClientBuilder().
		WithScheme(handlerTestScheme).
		WithObjects(newTestHA(), wrongNs).
		Build()
	h := &Handler{
		K8sClient: k8s,
		Validator: &stubValidator{claims: newTestClaims()},
		Config: HandlerConfig{
			BridgeNamespace:      hTestBridgeNS,
			ForceLocalValidation: true,
		},
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (Secret in wrong namespace must not be used)", w.Code)
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

func TestHandler_BadBody_400(t *testing.T) {
	fx := newHandlerFixture(t)
	r := httptest.NewRequest(http.MethodPost, CredentialsPath, strings.NewReader("not json"))
	r.Header.Set("Authorization", "Bearer x")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandler_EmptyBody_OK(t *testing.T) {
	// Body is optional; an empty body should not block credential issuance.
	fx := newHandlerFixture(t)
	r := httptest.NewRequest(http.MethodPost, CredentialsPath, nil)
	r.Header.Set("Authorization", "Bearer some-sa-token")
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d (empty body should be accepted): %s", w.Code, w.Body.String())
	}
}

// ----------------------------------------------------------------------------
// Security regression tests (see AUDIT.md)
// ----------------------------------------------------------------------------

// AUDIT.md F1: the credential-request body is bounded by maxRequestBodyBytes
// and the decode happens before token validation, so an attacker who supplies
// only a dummy Bearer header must not be able to make the bridge buffer an
// arbitrarily large body. We assert the oversized body is rejected (400) and
// never reaches the validator.
func TestHandler_RejectsOversizedBody(t *testing.T) {
	fx := newHandlerFixture(t)
	// A JSON string value larger than the cap. The decoder must error out
	// via MaxBytesReader rather than buffer the whole thing.
	huge := strings.Repeat("A", maxRequestBodyBytes+1<<10)
	body := []byte(`{"image":"` + huge + `"}`)
	r := httptest.NewRequest(http.MethodPost, CredentialsPath, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer some-sa-token")
	r.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for oversized body; body=%s", w.Code, w.Body.String())
	}
}

// AUDIT.md F5: a CR whose trustPolicy.issuer disagrees with the validated
// token's iss claim must not be matched, even when sub and aud line up.
func TestHandler_IssuerMismatch_NoMatch(t *testing.T) {
	fx := newHandlerFixture(t)
	// Token validated as a different issuer than the CR declares.
	fx.Validator.claims = &Claims{
		Subject:  hTestSubject,
		Audience: []string{hTestAudience},
		Issuer:   "https://attacker.example.com",
		Expiry:   time.Now().Add(time.Hour),
	}
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "harbor.example.com/production/img:v1"))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no matching HarborAccess on issuer mismatch); body=%s",
			w.Code, w.Body.String())
	}
}

// AUDIT.md F2: the robot Secret name "robot-<haNs>-<haName>" is dash-joined
// and therefore ambiguous, so two distinct HarborAccess CRs can collapse to
// the same Secret name. The read path must refuse to hand a token matched to
// CR A a Secret that is stamped (via labels) for CR B — otherwise one
// workload's SA receives another's robot credentials. We expect 403, not 200.
func TestHandler_SecretOwnerMismatch_Forbidden(t *testing.T) {
	fx := newHandlerFixture(t)
	sec := &corev1.Secret{}
	if err := fx.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: hTestBridgeNS, Name: "robot-" + hTestHANs + "-" + hTestHAName},
		sec); err != nil {
		t.Fatal(err)
	}
	// Stamp the Secret as owned by a DIFFERENT HarborAccess.
	sec.Labels = map[string]string{
		"harbor.aetherize.io/managed-by":             "harbor-workload-identity-bridge",
		"harbor.aetherize.io/harboraccess-namespace": "other-ns",
		"harbor.aetherize.io/harboraccess-name":      "other-ha",
	}
	if err := fx.K8s.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "harbor.example.com/production/img:v1"))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (Secret owner mismatch must not disclose creds); body=%s",
			w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), hTestRobotPass) {
		t.Fatal("response body leaked the robot password on an owner mismatch")
	}
}
