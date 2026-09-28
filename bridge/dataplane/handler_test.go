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

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
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
	hTestRobotUser = "robot$bridge-prod.flux-system.source-controller"
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
			TokenTTL: harborv1alpha1.Duration{Duration: time.Hour},
		},
	}
}

func newTestRobotSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: hTestBridgeNS,
			Name:      "robot-" + hTestHANs + "." + hTestHAName,
			Labels:    robotsecret.Labels("prod", hTestHANs, hTestHAName),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"username": []byte(hTestRobotUser),
			"password": []byte(hTestRobotPass),
		},
	}
}

// testRobotUsername is the robot username main wires for cluster "prod"
// with Harbor's default prefix: the fixture Secret holds it.
func testRobotUsername(saNamespace, saName string) (string, error) {
	return "robot$bridge-prod." + saNamespace + "." + saName, nil
}

func testHandlerConfig() HandlerConfig {
	return HandlerConfig{
		BridgeNamespace:      hTestBridgeNS,
		ForceLocalValidation: true,
		Audience:             hTestAudience,
		RobotUsername:        testRobotUsername,
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

// newFakeClientBuilder is a fake client builder with the cache index the
// handler lists HarborAccess objects by.
func newFakeClientBuilder() *fake.ClientBuilder {
	return fake.NewClientBuilder().WithScheme(handlerTestScheme).
		WithIndex(&harborv1alpha1.HarborAccess{}, HarborAccessSubjectField, harborAccessSubjectIndex)
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
	k8s := newFakeClientBuilder().
		WithObjects(objs...).
		Build()
	validator := &stubValidator{claims: newTestClaims()}
	return &handlerFixture{
		Validator: validator,
		K8s:       k8s,
		Handler: &Handler{
			K8sClient: k8s,
			Validator: validator,
			Config:    testHandlerConfig(),
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
	ha.Spec.TokenTTL = harborv1alpha1.Duration{Duration: 15 * time.Minute}
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

// unsortedListClient returns its HarborAccess items in a fixed, caller-
// controlled order regardless of namespace/name, so a test can prove
// findHarborAccess sorts internally rather than leaning on the informer's
// (or the fake client's) own ordering. Only List is exercised by
// findHarborAccess; the embedded nil client.Client makes every other method
// a compile-time satisfier this test never calls.
type unsortedListClient struct {
	client.Client
	items []harborv1alpha1.HarborAccess
}

func (c *unsortedListClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	hal, ok := list.(*harborv1alpha1.HarborAccessList)
	if !ok {
		return errors.New("unsortedListClient: unexpected list type")
	}
	hal.Items = c.items
	return nil
}

func TestFindHarborAccess_MultipleMatches_DeterministicSelection(t *testing.T) {
	// Two CRs claim the same (subject, issuer, audience) identity — an
	// operator misconfiguration. findHarborAccess must resolve it the same
	// way on every call: the namespace/name-sorted-first match, never
	// whichever the informer cache happened to list first. A
	// non-deterministic pick would let a workload intermittently receive a
	// more- or less-privileged robot than intended.
	mk := func(name string, action harborv1alpha1.HarborAction) harborv1alpha1.HarborAccess {
		return harborv1alpha1.HarborAccess{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hTestBridgeNS},
			Spec: harborv1alpha1.HarborAccessSpec{
				ServiceAccountRef: harborv1alpha1.ServiceAccountRef{
					Namespace: "flux-system", Name: "source-controller",
				},
				TrustPolicy: harborv1alpha1.TrustPolicy{
					Issuer:   "https://kubernetes.default.svc",
					Audience: hTestAudience,
				},
				Permissions: []harborv1alpha1.ProjectPermission{{Project: "p", Action: action}},
			},
		}
	}
	// Deliberately reverse-sorted List order: a "return the first match
	// iterated" regression picks zzz-dup; the contract requires aaa-dup.
	h := &Handler{
		K8sClient: &unsortedListClient{
			items: []harborv1alpha1.HarborAccess{mk("zzz-dup", "pull,push"), mk("aaa-dup", "pull")},
		},
		Validator: &stubValidator{claims: newTestClaims()},
		Config:    testHandlerConfig(),
	}
	for i := 0; i < 5; i++ {
		matched, aud, _, err := h.findHarborAccess(context.Background(), newTestClaims())
		if err != nil {
			t.Fatalf("iteration %d: findHarborAccess: %v", i, err)
		}
		if matched == nil {
			t.Fatalf("iteration %d: expected a match, got nil", i)
		}
		if matched.Name != "aaa-dup" {
			t.Fatalf("iteration %d: selected %q, want deterministic min %q (List returned zzz-dup first)", i, matched.Name, "aaa-dup")
		}
		// The sorted-first CR carries pull-only; proves we did not silently
		// hand the workload the more-privileged pull,push robot.
		if got := string(matched.Spec.Permissions[0].Action); got != "pull" {
			t.Fatalf("iteration %d: selected CR action = %q, want %q", i, got, "pull")
		}
		if aud != hTestAudience {
			t.Errorf("iteration %d: aud = %q, want %q", i, aud, hTestAudience)
		}
	}
}

// The handler lists only the CRs naming the token's ServiceAccount, from
// the cache index and without deep-copying them; a plain List copied
// every HarborAccess on every request, also for tokens matching none.
func TestFindHarborAccess_ListsByIndexedSubjectWithoutDeepCopy(t *testing.T) {
	other := newTestHA()
	other.Name = "other"
	other.Spec.ServiceAccountRef.Name = "someone-else"
	var opts client.ListOptions
	listed := 0
	k8s := newFakeClientBuilder().WithObjects(newTestHA(), other).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, o ...client.ListOption) error {
				opts.ApplyOptions(o)
				err := c.List(ctx, list, o...)
				if hal, ok := list.(*harborv1alpha1.HarborAccessList); ok {
					listed = len(hal.Items)
				}
				return err
			},
		}).Build()
	h := &Handler{K8sClient: k8s, Config: testHandlerConfig()}
	matched, _, _, err := h.findHarborAccess(context.Background(), newTestClaims())
	if err != nil {
		t.Fatal(err)
	}
	if matched == nil || matched.Name != hTestHAName {
		t.Fatalf("matched %v, want %s", matched, hTestHAName)
	}
	if v, ok := opts.FieldSelector.RequiresExactMatch(HarborAccessSubjectField); opts.FieldSelector == nil || !ok || v != hTestSubject {
		t.Errorf("listed with field selector %v, want %s=%s", opts.FieldSelector, HarborAccessSubjectField, hTestSubject)
	}
	if opts.UnsafeDisableDeepCopy == nil || !*opts.UnsafeDisableDeepCopy {
		t.Error("listed with deep copies")
	}
	if listed != 1 {
		t.Errorf("%d HarborAccess objects listed, want only the one naming the token's ServiceAccount", listed)
	}
	if got := harborAccessSubjectIndex(newTestRobotSecret()); got != nil {
		t.Errorf("index of a non-HarborAccess = %v, want nil", got)
	}
}

func TestFindHarborAccess_EmptyAudienceOrIssuer_NeverMatches(t *testing.T) {
	// The CRD enforces MinLength=1 on trustPolicy.{audience,issuer}, but the
	// data plane must not depend on that. A CR that somehow carries an
	// empty audience must NOT match a token whose aud claim is (or
	// contains) the empty string, and likewise for issuer.
	base := func() *harborv1alpha1.HarborAccess {
		return &harborv1alpha1.HarborAccess{
			ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: hTestBridgeNS},
			Spec: harborv1alpha1.HarborAccessSpec{
				ServiceAccountRef: harborv1alpha1.ServiceAccountRef{
					Namespace: "flux-system", Name: "source-controller",
				},
				TrustPolicy: harborv1alpha1.TrustPolicy{
					Issuer:   "https://kubernetes.default.svc",
					Audience: hTestAudience,
				},
				Permissions: []harborv1alpha1.ProjectPermission{{Project: "p", Action: "pull"}},
			},
		}
	}
	cases := map[string]struct {
		mutateCR     func(*harborv1alpha1.HarborAccess)
		claimsAud    []string
		claimsIssuer string
	}{
		"empty CR audience vs empty token aud": {
			mutateCR:     func(h *harborv1alpha1.HarborAccess) { h.Spec.TrustPolicy.Audience = "" },
			claimsAud:    []string{""},
			claimsIssuer: "https://kubernetes.default.svc",
		},
		"empty CR issuer vs empty token issuer": {
			mutateCR:     func(h *harborv1alpha1.HarborAccess) { h.Spec.TrustPolicy.Issuer = "" },
			claimsAud:    []string{hTestAudience},
			claimsIssuer: "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cr := base()
			tc.mutateCR(cr)
			k8s := newFakeClientBuilder().
				WithObjects(cr).
				Build()
			h := &Handler{
				K8sClient: k8s,
				Validator: &stubValidator{},
				Config:    testHandlerConfig(),
			}
			claims := &Claims{Subject: hTestSubject, Audience: tc.claimsAud, Issuer: tc.claimsIssuer}
			matched, _, _, err := h.findHarborAccess(context.Background(), claims)
			if err != nil {
				t.Fatalf("findHarborAccess: %v", err)
			}
			if matched != nil {
				t.Fatalf("empty audience/issuer must not match; got CR %s/%s", matched.Namespace, matched.Name)
			}
		})
	}
}

// deletingHA is the fixture CR after `kubectl delete` while the bridge's
// finalizer holds it (e.g. Harbor refuses the robot's deletion).
func deletingHA() *harborv1alpha1.HarborAccess {
	ha := newTestHA()
	now := metav1.Now()
	ha.DeletionTimestamp = &now
	ha.Finalizers = []string{"harbor.aetherize.io/robot"}
	return ha
}

// Deleting a HarborAccess revokes it at once: the data plane stops
// handing out the robot's password although the CR and its Secret stay
// until the finalizer is released.
func TestHandler_HarborAccessBeingDeleted_NotServed(t *testing.T) {
	var audit captured
	k8s := newFakeClientBuilder().
		WithObjects(deletingHA(), newTestRobotSecret()).Build()
	h := &Handler{
		K8sClient: k8s,
		Validator: &stubValidator{claims: newTestClaims()},
		Config:    testHandlerConfig(),
		Audit:     audit.logger(),
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), hTestRobotPass) {
		t.Fatalf("status %d body %q, want 403 without credentials", w.Code, w.Body.String())
	}
	out := audit.joined()
	for _, want := range []string{`"credential denied"`, `"reason"="harboraccess_deleting"`, `"harboraccess"="harbor-bridge-system/flux-access"`} {
		if !strings.Contains(out, want) {
			t.Errorf("audit line lacks %s:\n%s", want, out)
		}
	}
}

// A CR being deleted also drops out of the duplicate-CR selection, so a
// live CR for the same identity is served, even when the deleting one
// sorts first.
func TestFindHarborAccess_SkipsHarborAccessBeingDeleted(t *testing.T) {
	gone := deletingHA()
	gone.Name = "aaa-deleting"
	live := newTestHA()
	live.Name = "zzz-live"
	h := &Handler{
		K8sClient: newFakeClientBuilder().WithObjects(gone, live).Build(),
		Config:    testHandlerConfig(),
	}
	matched, _, deleting, err := h.findHarborAccess(context.Background(), newTestClaims())
	if err != nil {
		t.Fatal(err)
	}
	if matched == nil || matched.Name != "zzz-live" || deleting != nil {
		t.Fatalf("matched %v, deleting %v; want the live CR and no deleting one", matched, deleting)
	}
}

// After a serviceAccountRef change, the robot Secret still holds the
// previous identity's robot until the control plane has created the new
// one; it deletes the old robot right after. The new identity must not be
// handed that password: kubelet would cache it for tokenTTL and fail
// every pull once the robot is gone.
func TestHandler_SecretForPreviousIdentity_503(t *testing.T) {
	var audit captured
	ha := newTestHA()
	ha.Spec.ServiceAccountRef = harborv1alpha1.ServiceAccountRef{Namespace: "team-b", Name: "new-sa"}
	claims := newTestClaims()
	claims.Subject = "system:serviceaccount:team-b:new-sa"
	h := &Handler{
		K8sClient: newFakeClientBuilder().
			WithObjects(ha, newTestRobotSecret()).Build(), // still robot$bridge-prod.flux-system.source-controller
		Validator: &stubValidator{claims: claims},
		Config:    testHandlerConfig(),
		Audit:     audit.logger(),
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), hTestRobotPass) {
		t.Fatalf("status %d body %q, want 503 without credentials", w.Code, w.Body.String())
	}
	out := audit.joined()
	for _, want := range []string{`"credential unavailable"`, `"reason"="secret_for_previous_identity"`,
		`"robot"="` + hTestRobotUser + `"`, `"expected_robot"="robot$bridge-prod.team-b.new-sa"`} {
		if !strings.Contains(out, want) {
			t.Errorf("audit line lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, hTestRobotPass) {
		t.Fatal("audit line carries the robot password")
	}
}

// Without a way to tell which robot a Secret must hold, nothing is served.
func TestHandler_RobotUsernameUnset_ServesNothing(t *testing.T) {
	fx := newHandlerFixture(t)
	fx.Handler.Config.RobotUsername = nil
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), hTestRobotPass) {
		t.Fatalf("status %d body %q, want 500 without credentials", w.Code, w.Body.String())
	}
}

func TestHandler_MissingRobotSecret_503(t *testing.T) {
	// Bridge namespace exists but the Secret is missing — control plane
	// is mid-rotation or hasn't caught up yet. 503 invites the plugin
	// to retry.
	k8s := newFakeClientBuilder().
		WithObjects(newTestHA()).
		Build()
	h := &Handler{
		K8sClient: k8s,
		Validator: &stubValidator{claims: newTestClaims()},
		Config:    testHandlerConfig(),
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
			Name:      "robot-" + hTestHANs + "." + hTestHAName,
		},
		Data: map[string][]byte{
			"username": []byte("attacker-supplied"),
			"password": []byte("attacker-supplied"),
		},
	}
	k8s := newFakeClientBuilder().
		WithObjects(newTestHA(), wrongNs).
		Build()
	h := &Handler{
		K8sClient: k8s,
		Validator: &stubValidator{claims: newTestClaims()},
		Config:    testHandlerConfig(),
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
// Security regression tests
// ----------------------------------------------------------------------------

// The credential-request body is bounded by maxRequestBodyBytes
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

// A CR whose trustPolicy.issuer disagrees with the validated
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

// TestHandler_SecretNameMatchesControlPlane: the handler must read the
// Secret under exactly the name the control plane writes. Both use
// robotsecret.Name now; this pins the literal so a change is deliberate.
func TestHandler_SecretNameMatchesControlPlane(t *testing.T) {
	if got := robotsecret.Name(hTestHANs, hTestHAName); got != newTestRobotSecret().Name {
		t.Fatalf("fixture Secret name %q != robotsecret.Name %q", newTestRobotSecret().Name, got)
	}
}

// TestHandler_CacheNeverOutlivesTheRotationPromise pins ADR-0023: kubelet
// must not cache the password past the control plane's rotation-not-before
// instant, or the scheduled daily rotation breaks pulls on every node
// that cached within the last tokenTTL.
func TestHandler_CacheNeverOutlivesTheRotationPromise(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		notBefore string // "" = annotation absent (older bridge)
		want      int
	}{
		{"no promise keeps tokenTTL", "", 3600},
		{"far promise keeps tokenTTL", now.Add(20 * time.Hour).Format(time.RFC3339), 3600},
		{"near promise caps the cache", now.Add(10*time.Minute + 500*time.Millisecond).Format(time.RFC3339Nano), 600},
		{"passed promise disables caching", now.Add(-time.Second).Format(time.RFC3339), 0},
		{"unparseable promise keeps tokenTTL", "tomorrow", 3600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sec := newTestRobotSecret()
			if tc.notBefore != "" {
				sec.Annotations = map[string]string{robotsecret.AnnotationRotationNotBefore: tc.notBefore}
			}
			k8s := newFakeClientBuilder().WithObjects(newTestHA(), sec).Build()
			h := &Handler{
				K8sClient: k8s,
				Validator: &stubValidator{claims: newTestClaims()},
				Config:    testHandlerConfig(),
				Now:       func() time.Time { return now },
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, bearerReq(t, ""))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if got := decodeResp(t, w).ExpiresInSecs; got != tc.want {
				t.Errorf("ExpiresInSecs = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestHandler_ResponseIsNotStorable(t *testing.T) {
	fx := newHandlerFixture(t)
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, ""))
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store on a response carrying a password", got)
	}
}

// Defense-in-depth read-path backstop. Since ADR-0018 the Secret name
// "robot-<haNs>.<haName>" is dot-joined and injective, so in normal
// operation two CRs never share a Secret name. This test forces the
// owner-label mismatch directly to prove that, even if that invariant ever
// regressed, the read path refuses to hand a token matched to CR A a Secret
// stamped (via labels) for CR B — returning 403, never the other CR's creds.
func TestHandler_SecretOwnerMismatch_Forbidden(t *testing.T) {
	fx := newHandlerFixture(t)
	sec := &corev1.Secret{}
	if err := fx.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: hTestBridgeNS, Name: "robot-" + hTestHANs + "." + hTestHAName},
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

// ADR-0026: a CR naming another audience is never served, even when the
// token carries exactly that audience (e.g. the apiserver's default one).
func TestHandler_ForeignAudienceCR_NotServed(t *testing.T) {
	const foreign = "https://kubernetes.default.svc"
	ha := newTestHA()
	ha.Spec.TrustPolicy.Audience = foreign
	claims := newTestClaims()
	claims.Audience = []string{foreign}
	k8s := newFakeClientBuilder().
		WithObjects(ha, newTestRobotSecret()).
		Build()
	h := &Handler{
		K8sClient: k8s,
		Validator: &stubValidator{claims: claims},
		Config:    testHandlerConfig(),
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestHandler_UnsetAudienceServesNothing(t *testing.T) {
	k8s := newFakeClientBuilder().
		WithObjects(newTestHA(), newTestRobotSecret()).
		Build()
	h := &Handler{
		K8sClient: k8s,
		Validator: &stubValidator{claims: newTestClaims()},
		Config:    HandlerConfig{BridgeNamespace: hTestBridgeNS, ForceLocalValidation: true},
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (fail closed without a configured audience)", w.Code)
	}
}

// The data plane never hands out a Secret the control plane did not write.
func TestHandler_UnmanagedSecret_NotServed(t *testing.T) {
	unmanaged := newTestRobotSecret()
	unmanaged.Labels = nil
	k8s := newFakeClientBuilder().
		WithObjects(newTestHA(), unmanaged).
		Build()
	h := &Handler{
		K8sClient: k8s,
		Validator: &stubValidator{claims: newTestClaims()},
		Config:    testHandlerConfig(),
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bearerReq(t, "img"))
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), hTestRobotPass) {
		t.Fatalf("status %d body %q, want 403 without credentials", w.Code, w.Body.String())
	}
}

// ADR-0028 end to end through the real validator: a long-lived or unbound
// token is a 401, audited as invalid_token with its category and counted
// under that category; kubelet's one-hour pod-bound token is served. The
// token itself never reaches the audit line.
func TestHandler_TokenPolicyDenialsAreAuditedByCategory(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name     string
		mutate   func(jwt.MapClaims)
		status   int
		category string
	}{
		{"kubelet token", func(jwt.MapClaims) {}, http.StatusOK, ""},
		{"one-year token", func(c jwt.MapClaims) { c["exp"] = now.Add(8760 * time.Hour).Unix() }, http.StatusUnauthorized, OIDCReasonExcessiveLifetime},
		{"no iat", func(c jwt.MapClaims) { delete(c, "iat"); delete(c, "nbf") }, http.StatusUnauthorized, OIDCReasonExcessiveLifetime},
		{"not bound to a pod", func(c jwt.MapClaims) { delete(c, "kubernetes.io") }, http.StatusUnauthorized, OIDCReasonNotPodBound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fi := newFixtureIssuer(t)
			ha := newTestHA()
			ha.Spec.TrustPolicy.Issuer = fi.URL()
			reg := prometheus.NewRegistry()
			var audit captured
			h := &Handler{
				K8sClient: newFakeClientBuilder().WithObjects(ha, newTestRobotSecret()).Build(),
				Validator: newValidatorFor(t, fi),
				Config:    testHandlerConfig(),
				Metrics:   NewMetrics(reg),
				Audit:     audit.logger(),
			}

			claims := fi.standardClaims()
			claims["aud"] = hTestAudience
			claims["sub"] = hTestSubject
			tc.mutate(claims)
			token := fi.signToken(t, claims)
			r := bearerReq(t, "img")
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			out := audit.joined()
			if strings.Contains(out, token) {
				t.Fatal("audit line contains the token")
			}
			if tc.category == "" {
				if !strings.Contains(out, `"pod_uid"="3f2c0a1e"`) {
					t.Errorf("issuance not attributed to the bound pod:\n%s", out)
				}
				return
			}
			for _, want := range []string{`"credential denied"`, `"reason"="invalid_token"`, `"category"="` + tc.category + `"`} {
				if !strings.Contains(out, want) {
					t.Errorf("denial line lacks %s:\n%s", want, out)
				}
			}
			if got := counter(t, reg, "bridge_oidc_validation_failures_total", map[string]string{"reason": tc.category}); got != 1 {
				t.Errorf("oidc_failures{reason=%s} = %v, want 1", tc.category, got)
			}
		})
	}
}
