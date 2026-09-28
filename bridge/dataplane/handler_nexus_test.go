// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/nexussecret"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// ----------------------------------------------------------------------------
// Fixtures
// ----------------------------------------------------------------------------

var nexusTestScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(harborv1alpha1.AddToScheme(s))
	utilruntime.Must(nexusv1alpha1.AddToScheme(s))
	return s
}()

const (
	nTestNXANs      = "team-a"
	nTestNXAName    = "web"
	nTestSubject    = "system:serviceaccount:team-a:web"
	nTestCluster    = "prod"
	nTestGeneration = "0123456789abcdef"
	nTestPassword   = "nexus-password-v1"
	nTestImage      = "nexus.example.com:8443/docker-hosted/app:v1"
	nTestHarborImg  = "harbor.example.com/production/app:v1"
)

func nTestUserID(t *testing.T, saNamespace, saName string) string {
	t.Helper()
	id, err := nexus.UserID(nTestCluster, saNamespace, saName, nTestGeneration)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func newTestNXA() *nexusv1alpha1.NexusAccess {
	return &nexusv1alpha1.NexusAccess{
		ObjectMeta: metav1.ObjectMeta{Name: nTestNXAName, Namespace: nTestNXANs, Generation: 2},
		Spec: nexusv1alpha1.NexusAccessSpec{
			ServiceAccountRef: nexusv1alpha1.ServiceAccountRef{Namespace: nTestNXANs, Name: "web"},
			TrustPolicy: nexusv1alpha1.TrustPolicy{
				Issuer:   "https://kubernetes.default.svc",
				Audience: hTestAudience,
			},
			Repositories: []nexusv1alpha1.RepositoryGrant{
				{Name: "docker-hosted", Access: nexusv1alpha1.AccessPull, Format: nexusv1alpha1.FormatDocker},
			},
			TokenTTL: harborv1alpha1.Duration{Duration: time.Hour},
		},
	}
}

// newTestNexusSecret is the Secret the control plane writes for
// newTestNXA: the user of the identity team-a/web, promised for a day.
func newTestNexusSecret(t *testing.T) *corev1.Secret {
	t.Helper()
	user := nTestUserID(t, nTestNXANs, "web")
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: hTestBridgeNS,
			Name:      nexussecret.Name(nTestNXANs, nTestNXAName),
			Labels:    nexussecret.Labels(nTestCluster, nTestNXANs, nTestNXAName),
			Annotations: map[string]string{
				nexussecret.AnnotationUserID:            user,
				nexussecret.AnnotationRotationNotBefore: nexussecret.FormatTime(time.Now().Add(24 * time.Hour)),
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			nexussecret.KeyUsername: []byte(user),
			nexussecret.KeyPassword: []byte(nTestPassword),
		},
	}
}

func newTestNexusClaims() *Claims {
	c := newTestClaims()
	c.Subject = nTestSubject
	return c
}

// testNexusHandlerConfig is testHandlerConfig with the Nexus backend as
// main wires it for cluster "prod": the real naming functions.
func testNexusHandlerConfig(t *testing.T) HandlerConfig {
	t.Helper()
	hc := testHandlerConfig()
	hc.HarborRegistryHosts = mustHosts(t, "harbor.example.com")
	hc.Nexus = &NexusBackend{
		RegistryHosts: mustHosts(t, "nexus.example.com:8443"),
		IdentityName: func(saNamespace, saName string) (string, error) {
			return nexus.IdentityName(nTestCluster, saNamespace, saName)
		},
		UserIdentity: func(userID string) (string, bool) {
			identity, _, ok := nexus.ParseUserID(userID)
			return identity, ok
		},
	}
	return hc
}

func newNexusFakeClientBuilder() *fake.ClientBuilder {
	return fake.NewClientBuilder().WithScheme(nexusTestScheme).
		WithIndex(&harborv1alpha1.HarborAccess{}, HarborAccessSubjectField, harborAccessSubjectIndex).
		WithIndex(&nexusv1alpha1.NexusAccess{}, NexusAccessSubjectField, nexusAccessSubjectIndex)
}

type nexusFixture struct {
	Handler *Handler
	Audit   *captured
	Reg     *prometheus.Registry
}

// newNexusFixture serves newTestNXA with its Secret, plus the Harbor
// fixture for the Harbor ServiceAccount, with metrics and a captured audit
// log. objs replaces the default objects when given.
func newNexusFixture(t *testing.T, claims *Claims, objs ...client.Object) *nexusFixture {
	t.Helper()
	if objs == nil {
		objs = []client.Object{newTestNXA(), newTestNexusSecret(t), newTestHA(), newTestRobotSecret()}
	}
	return newNexusFixtureWith(t, newNexusFakeClientBuilder().WithObjects(objs...).Build(), claims)
}

func newNexusFixtureWith(t *testing.T, k8s client.Client, claims *Claims) *nexusFixture {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.Nexus = NewNexusMetrics(reg)
	audit := &captured{}
	return &nexusFixture{
		Handler: &Handler{
			K8sClient: k8s,
			Validator: &stubValidator{claims: claims},
			Config:    testNexusHandlerConfig(t),
			Metrics:   m,
			Audit:     audit.logger(),
		},
		Audit: audit,
		Reg:   reg,
	}
}

func (f *nexusFixture) serve(t *testing.T, image string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	f.Handler.ServeHTTP(w, bearerReq(t, image))
	return w
}

// expectRefused asserts a status without credentials, an audit line with
// every fragment, and the result in both issuance counters.
func (f *nexusFixture) expectRefused(t *testing.T, w *httptest.ResponseRecorder, status int, result string, fragments ...string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, status, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, nTestPassword) || strings.Contains(body, hTestRobotPass) {
		t.Fatalf("response carries credentials: %s", body)
	}
	f.expectAudit(t, fragments...)
	if got := counter(t, f.Reg, "bridge_credential_issuances_total", map[string]string{"result": result}); got != 1 {
		t.Errorf("bridge_credential_issuances_total{result=%q} = %v, want 1", result, got)
	}
	if got := counter(t, f.Reg, "bridge_nexus_credential_issuances_total", map[string]string{"result": result}); got != 1 {
		t.Errorf("bridge_nexus_credential_issuances_total{result=%q} = %v, want 1", result, got)
	}
}

func (f *nexusFixture) expectAudit(t *testing.T, fragments ...string) {
	t.Helper()
	out := f.Audit.joined()
	for _, want := range fragments {
		if !strings.Contains(out, want) {
			t.Errorf("audit log lacks %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, nTestPassword) || strings.Contains(out, hTestRobotPass) {
		t.Fatalf("audit log carries a password:\n%s", out)
	}
}

func updateSecret(t *testing.T, s *corev1.Secret, edit func(*corev1.Secret)) *corev1.Secret {
	t.Helper()
	edit(s)
	return s
}

// ----------------------------------------------------------------------------
// Serving
// ----------------------------------------------------------------------------

func TestHandler_Nexus_ServesTheUserOfTheMatchedNexusAccess(t *testing.T) {
	f := newNexusFixture(t, newTestNexusClaims())
	w := f.serve(t, nTestImage)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	got := decodeResp(t, w)
	want := Response{
		Username:      nTestUserID(t, nTestNXANs, "web"),
		Password:      nTestPassword,
		ExpiresInSecs: 3600,
		CacheKeyType:  cacheKeyTypeRegistry,
	}
	if got != want {
		t.Errorf("response = %+v, want %+v", got, want)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	f.expectAudit(t, `"credential issued"`, `"access_kind"="nexus"`, `"subject"="`+nTestSubject+`"`,
		`"nexusaccess"="team-a/web"`, `"generation"=2`, `"nexus_user"="`+want.Username+`"`,
		`"ttl_seconds"=3600`, `"requested_image"="`+nTestImage+`"`, `"audience"="`+hTestAudience+`"`)
	if got := counter(t, f.Reg, "bridge_credential_issuances_total", map[string]string{"result": ResultOK}); got != 1 {
		t.Errorf("ok issuances = %v, want 1", got)
	}
	if got := counter(t, f.Reg, "bridge_nexus_credential_issuances_total", map[string]string{"result": ResultOK}); got != 1 {
		t.Errorf("ok Nexus issuances = %v, want 1", got)
	}
}

// The rotation promise caps kubelet's cache (ADR-0023). A Nexus Secret
// without one was edited by hand and its user is replaced soon, so
// nothing is cached; unlike robot Secrets, none predates the promise.
func TestHandler_Nexus_CacheNeverOutlivesTheRotationPromise(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		notBefore string // "" removes the annotation
		ttl       time.Duration
		want      int
	}{
		{"far promise keeps tokenTTL", nexussecret.FormatTime(now.Add(24 * time.Hour)), time.Hour, 3600},
		{"near promise cuts tokenTTL", nexussecret.FormatTime(now.Add(10*time.Minute + 30*time.Second)), time.Hour, 630},
		{"passed promise caches nothing", nexussecret.FormatTime(now.Add(-time.Minute)), time.Hour, 0},
		{"no promise caches nothing", "", time.Hour, 0},
		{"unparseable promise caches nothing", "tomorrow", time.Hour, 0},
		{"custom tokenTTL", nexussecret.FormatTime(now.Add(24 * time.Hour)), 20 * time.Minute, 1200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nxa := newTestNXA()
			nxa.Spec.TokenTTL.Duration = tc.ttl
			sec := updateSecret(t, newTestNexusSecret(t), func(s *corev1.Secret) {
				if tc.notBefore == "" {
					delete(s.Annotations, nexussecret.AnnotationRotationNotBefore)
				} else {
					s.Annotations[nexussecret.AnnotationRotationNotBefore] = tc.notBefore
				}
			})
			f := newNexusFixture(t, newTestNexusClaims(), nxa, sec)
			f.Handler.Now = func() time.Time { return now }
			w := f.serve(t, nTestImage)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
			}
			if got := decodeResp(t, w).ExpiresInSecs; got != tc.want {
				t.Errorf("expires_in = %d, want %d", got, tc.want)
			}
		})
	}
}

// A rotation replaces the user under a new generation; the data plane
// serves whichever generation of the identity the Secret holds.
func TestHandler_Nexus_ServesAnyGenerationOfTheIdentity(t *testing.T) {
	user, err := nexus.UserID(nTestCluster, nTestNXANs, "web", "fedcba9876543210")
	if err != nil {
		t.Fatal(err)
	}
	sec := updateSecret(t, newTestNexusSecret(t), func(s *corev1.Secret) {
		s.Data[nexussecret.KeyUsername] = []byte(user)
		s.Annotations[nexussecret.AnnotationUserID] = user
		// The previous user outlives the Secret write by a grace; the
		// record is the control plane's business.
		s.Annotations[nexussecret.AnnotationRetiringUserID] = nTestUserID(t, nTestNXANs, "web")
		s.Annotations[nexussecret.AnnotationRetireAfter] = nexussecret.FormatTime(time.Now().Add(5 * time.Minute))
	})
	f := newNexusFixture(t, newTestNexusClaims(), newTestNXA(), sec)
	w := f.serve(t, nTestImage)
	if w.Code != http.StatusOK || decodeResp(t, w).Username != user {
		t.Fatalf("status = %d, want 200 with user %s; body = %s", w.Code, user, w.Body.String())
	}
}

// ----------------------------------------------------------------------------
// Refusals
// ----------------------------------------------------------------------------

// A repository the spec names does not exist (ADR-0033 decision d): the
// role lacks its privileges, and the data plane issues nothing, whatever
// the annotation says.
func TestHandler_Nexus_GrantsIncomplete_Refused(t *testing.T) {
	for _, value := range []string{"absent-repo", "a,b", "", " "} {
		t.Run(value, func(t *testing.T) {
			sec := updateSecret(t, newTestNexusSecret(t), func(s *corev1.Secret) {
				s.Annotations[nexussecret.AnnotationGrantsIncomplete] = value
			})
			f := newNexusFixture(t, newTestNexusClaims(), newTestNXA(), sec)
			w := f.serve(t, nTestImage)
			fragments := []string{`"credential denied"`, `"reason"="grants_incomplete"`, `"nexusaccess"="team-a/web"`, `"access_kind"="nexus"`}
			if value == "absent-repo" {
				fragments = append(fragments, `"missing_repositories"="absent-repo"`)
			}
			f.expectRefused(t, w, http.StatusForbidden, ResultForbidden, fragments...)
		})
	}
}

// Deleting a NexusAccess revokes it at once, although its finalizer keeps
// it and its Secret while Nexus refuses the users' deletion.
func TestHandler_Nexus_Deleting_Refused(t *testing.T) {
	nxa := newTestNXA()
	now := metav1.Now()
	nxa.DeletionTimestamp = &now
	nxa.Finalizers = []string{"nexus.aetherize.io/user"}
	f := newNexusFixture(t, newTestNexusClaims(), nxa, newTestNexusSecret(t))
	f.expectRefused(t, f.serve(t, nTestImage), http.StatusForbidden, ResultForbidden,
		`"reason"="nexusaccess_deleting"`, `"nexusaccess"="team-a/web"`)
}

// A live NexusAccess of the same identity is served while another one is
// being deleted, even when the deleting one sorts first.
func TestFindNexusAccess_SkipsNexusAccessBeingDeleted(t *testing.T) {
	gone := newTestNXA()
	gone.Name = "aaa-deleting"
	now := metav1.Now()
	gone.DeletionTimestamp = &now
	gone.Finalizers = []string{"nexus.aetherize.io/user"}
	live := newTestNXA()
	live.Name = "zzz-live"
	h := &Handler{K8sClient: newNexusFakeClientBuilder().WithObjects(gone, live).Build(), Config: testNexusHandlerConfig(t)}
	matched, _, deleting, err := h.findNexusAccess(context.Background(), newTestNexusClaims())
	if err != nil {
		t.Fatal(err)
	}
	if matched == nil || matched.Name != "zzz-live" || deleting != nil {
		t.Fatalf("matched %v, deleting %v; want the live object and no deleting one", matched, deleting)
	}
}

// After a serviceAccountRef change the Secret holds the previous
// identity's user until the control plane has created the new one. The
// new identity gets 503 (the plugin retries, kubelet caches nothing); the
// previous identity no longer matches at all.
func TestHandler_Nexus_IdentityChange(t *testing.T) {
	nxa := newTestNXA()
	nxa.Spec.ServiceAccountRef.Name = "api"
	newClaims := newTestNexusClaims()
	newClaims.Subject = "system:serviceaccount:team-a:api"

	f := newNexusFixture(t, newClaims, nxa, newTestNexusSecret(t)) // still the user of team-a/web
	want, err := nexus.IdentityName(nTestCluster, nTestNXANs, "api")
	if err != nil {
		t.Fatal(err)
	}
	f.expectRefused(t, f.serve(t, nTestImage), http.StatusServiceUnavailable, ResultUnavailable,
		`"credential unavailable"`, `"reason"="secret_for_previous_identity"`,
		`"nexus_user"="`+nTestUserID(t, nTestNXANs, "web")+`"`, `"expected_identity"="`+want+`"`)

	old := newNexusFixture(t, newTestNexusClaims(), nxa, newTestNexusSecret(t))
	old.expectRefused(t, old.serve(t, nTestImage), http.StatusForbidden, ResultForbidden,
		`"reason"="no_matching_nexusaccess"`)
}

// A username that is no user id the control plane builds (a hand-edited
// Secret, an administrator's account) is never served.
func TestHandler_Nexus_ForeignUsername_Refused(t *testing.T) {
	for _, user := range []string{
		"admin",
		"bridge-prod.team-a.web", // the role id, no generation
		"bridge-prod.team-a.web_0123456789ABCDEF",  // upper-case generation
		"bridge-other.team-a.web_0123456789abcdef", // another cluster's identity
		"BRIDGE-PROD.team-a.web_0123456789abcdef",  // Nexus matches ids case-insensitively, the bridge does not
	} {
		t.Run(user, func(t *testing.T) {
			sec := updateSecret(t, newTestNexusSecret(t), func(s *corev1.Secret) {
				s.Data[nexussecret.KeyUsername] = []byte(user)
				s.Annotations[nexussecret.AnnotationUserID] = user
			})
			f := newNexusFixture(t, newTestNexusClaims(), newTestNXA(), sec)
			f.expectRefused(t, f.serve(t, nTestImage), http.StatusServiceUnavailable, ResultUnavailable,
				`"reason"="secret_for_previous_identity"`)
		})
	}
}

// A Secret without a complete password: one that only records the user
// being created, one with half the keys, and one whose username and
// user-id record disagree. The control plane replaces each; the data
// plane answers 503 so the plugin retries.
func TestHandler_Nexus_IncompleteSecret_503(t *testing.T) {
	for name, edit := range map[string]func(*corev1.Secret){
		"pending user only": func(s *corev1.Secret) {
			// What the control plane writes before it creates the first
			// user of an identity.
			s.Data = nil
			s.Annotations = map[string]string{nexussecret.AnnotationPendingUserID: "bridge-prod.team-a.web_fedcba9876543210"}
		},
		"no password": func(s *corev1.Secret) { delete(s.Data, nexussecret.KeyPassword) },
		"no username": func(s *corev1.Secret) { delete(s.Data, nexussecret.KeyUsername) },
		"user-id record differs": func(s *corev1.Secret) {
			s.Annotations[nexussecret.AnnotationUserID] = "bridge-prod.team-a.web_fedcba9876543210"
		},
		"user-id record missing": func(s *corev1.Secret) { delete(s.Annotations, nexussecret.AnnotationUserID) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newNexusFixture(t, newTestNexusClaims(), newTestNXA(), updateSecret(t, newTestNexusSecret(t), edit))
			f.expectRefused(t, f.serve(t, nTestImage), http.StatusServiceUnavailable, ResultUnavailable,
				`"credential unavailable"`, `"reason"="secret_incomplete"`, `"nexusaccess"="team-a/web"`)
			if got := counter(t, f.Reg, "bridge_nexus_user_secret_missing_total", nil); got != 1 {
				t.Errorf("bridge_nexus_user_secret_missing_total = %v, want 1", got)
			}
		})
	}
}

func TestHandler_Nexus_MissingSecret_503(t *testing.T) {
	f := newNexusFixture(t, newTestNexusClaims(), newTestNXA())
	f.expectRefused(t, f.serve(t, nTestImage), http.StatusServiceUnavailable, ResultUnavailable,
		`"reason"="secret_missing"`, `"nexusaccess"="team-a/web"`)
	if got := counter(t, f.Reg, "bridge_nexus_user_secret_missing_total", nil); got != 1 {
		t.Errorf("bridge_nexus_user_secret_missing_total = %v, want 1", got)
	}
	if got := counter(t, f.Reg, "bridge_robot_secret_missing_total", nil); got != 0 {
		t.Errorf("bridge_robot_secret_missing_total = %v, want 0", got)
	}
}

// The data plane serves a Nexus Secret only when the control plane wrote
// it for exactly the matched NexusAccess.
func TestHandler_Nexus_SecretNotOwned_Refused(t *testing.T) {
	for name, edit := range map[string]func(*corev1.Secret){
		"stamped for another NexusAccess": func(s *corev1.Secret) {
			s.Labels[nexussecret.LabelNexusAccessName] = "other"
		},
		"no owner labels": func(s *corev1.Secret) {
			delete(s.Labels, nexussecret.LabelNexusAccessNamespace)
			delete(s.Labels, nexussecret.LabelNexusAccessName)
		},
		"not managed": func(s *corev1.Secret) { s.Labels = nil },
		"no access kind": func(s *corev1.Secret) {
			delete(s.Labels, nexussecret.LabelAccessKind)
		},
		"another access kind": func(s *corev1.Secret) {
			s.Labels[nexussecret.LabelAccessKind] = "harbor"
		},
		"a robot Secret's labels": func(s *corev1.Secret) {
			s.Labels = robotsecret.Labels(nTestCluster, nTestNXANs, nTestNXAName)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newNexusFixture(t, newTestNexusClaims(), newTestNXA(), updateSecret(t, newTestNexusSecret(t), edit))
			f.expectRefused(t, f.serve(t, nTestImage), http.StatusForbidden, ResultForbidden,
				`"reason"="secret_owner_mismatch"`, `"nexusaccess"="team-a/web"`)
		})
	}
}

// The reconciler refuses such an object as InvalidSpec and deletes its
// Secret; until then the data plane must not serve the Secret from before.
func TestHandler_Nexus_InvalidSpec_Refused(t *testing.T) {
	for name, edit := range map[string]func(*nexusv1alpha1.NexusAccess){
		"tokenTTL in days": func(n *nexusv1alpha1.NexusAccess) {
			if err := json.Unmarshal([]byte(`"1d"`), &n.Spec.TokenTTL); err != nil || n.Spec.TokenTTL.Err() == nil {
				t.Fatalf("tokenTTL 1d decoded as valid (%v)", err)
			}
		},
		"no repositories": func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories = nil },
	} {
		t.Run(name, func(t *testing.T) {
			nxa := newTestNXA()
			edit(nxa)
			f := newNexusFixture(t, newTestNexusClaims(), nxa, newTestNexusSecret(t))
			f.expectRefused(t, f.serve(t, nTestImage), http.StatusForbidden, ResultForbidden,
				`"reason"="invalid_nexusaccess_spec"`, `"nexusaccess"="team-a/web"`)
		})
	}
}

// ADR-0026: an object naming another audience or issuer is never served,
// nor one with an empty trust policy.
func TestHandler_Nexus_TrustPolicyMismatch_NoMatch(t *testing.T) {
	for name, edit := range map[string]func(*nexusv1alpha1.NexusAccess, *Claims){
		"foreign audience": func(n *nexusv1alpha1.NexusAccess, c *Claims) {
			n.Spec.TrustPolicy.Audience = "https://kubernetes.default.svc"
			c.Audience = []string{"https://kubernetes.default.svc"}
		},
		"token lacks the audience": func(_ *nexusv1alpha1.NexusAccess, c *Claims) { c.Audience = []string{"other"} },
		"other issuer": func(n *nexusv1alpha1.NexusAccess, _ *Claims) {
			n.Spec.TrustPolicy.Issuer = "https://other.example.com"
		},
		"empty audience": func(n *nexusv1alpha1.NexusAccess, c *Claims) {
			n.Spec.TrustPolicy.Audience = ""
			c.Audience = []string{""}
		},
		"other ServiceAccount": func(_ *nexusv1alpha1.NexusAccess, c *Claims) {
			c.Subject = "system:serviceaccount:team-a:webx"
		},
	} {
		t.Run(name, func(t *testing.T) {
			nxa, claims := newTestNXA(), newTestNexusClaims()
			edit(nxa, claims)
			f := newNexusFixture(t, claims, nxa, newTestNexusSecret(t))
			f.expectRefused(t, f.serve(t, nTestImage), http.StatusForbidden, ResultForbidden,
				`"reason"="no_matching_nexusaccess"`)
		})
	}
}

func TestHandler_Nexus_LookupFailure_500(t *testing.T) {
	k8s := newNexusFakeClientBuilder().WithObjects(newTestNXA(), newTestNexusSecret(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, o ...client.ListOption) error {
				if _, ok := list.(*nexusv1alpha1.NexusAccessList); ok {
					return errors.New("apiserver down")
				}
				return c.List(ctx, list, o...)
			},
		}).Build()
	f := newNexusFixtureWith(t, k8s, newTestNexusClaims())
	f.expectRefused(t, f.serve(t, nTestImage), http.StatusInternalServerError, ResultServerError,
		`"reason"="nexusaccess_lookup_failed"`, "apiserver down")
	if got := counter(t, f.Reg, "bridge_nexusaccess_lookup_failures_total", nil); got != 1 {
		t.Errorf("bridge_nexusaccess_lookup_failures_total = %v, want 1", got)
	}
	if got := counter(t, f.Reg, "bridge_harboraccess_lookup_failures_total", nil); got != 0 {
		t.Errorf("bridge_harboraccess_lookup_failures_total = %v, want 0", got)
	}
}

func TestHandler_Nexus_SecretUnreadable_500(t *testing.T) {
	k8s := newNexusFakeClientBuilder().WithObjects(newTestNXA(), newTestNexusSecret(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, o ...client.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("RBAC"))
				}
				return c.Get(ctx, key, obj, o...)
			},
		}).Build()
	f := newNexusFixtureWith(t, k8s, newTestNexusClaims())
	f.expectRefused(t, f.serve(t, nTestImage), http.StatusInternalServerError, ResultServerError,
		`"reason"="secret_unreadable"`)
}

// Without the naming functions the handler cannot tell whose user a
// Secret holds, and serves nothing.
func TestHandler_Nexus_NamingUnset_ServesNothing(t *testing.T) {
	for name, edit := range map[string]func(*NexusBackend){
		"IdentityName": func(n *NexusBackend) { n.IdentityName = nil },
		"UserIdentity": func(n *NexusBackend) { n.UserIdentity = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newNexusFixture(t, newTestNexusClaims())
			edit(f.Handler.Config.Nexus)
			f.expectRefused(t, f.serve(t, nTestImage), http.StatusInternalServerError, ResultServerError,
				`"reason"="nexus_identity_unknown"`)
		})
	}
}

// Two NexusAccess objects for one identity: the namespace/name-sorted
// first wins on every call, whatever order the cache lists them in.
func TestFindNexusAccess_MultipleMatches_DeterministicSelection(t *testing.T) {
	mk := func(name string, access nexusv1alpha1.RepositoryAccess) nexusv1alpha1.NexusAccess {
		n := newTestNXA()
		n.Name = name
		n.Spec.Repositories[0].Access = access
		return *n
	}
	h := &Handler{
		K8sClient: &unsortedNexusListClient{items: []nexusv1alpha1.NexusAccess{mk("zzz-dup", nexusv1alpha1.AccessPullPush), mk("aaa-dup", nexusv1alpha1.AccessPull)}},
		Config:    testNexusHandlerConfig(t),
	}
	for i := 0; i < 5; i++ {
		matched, aud, _, err := h.findNexusAccess(context.Background(), newTestNexusClaims())
		if err != nil {
			t.Fatal(err)
		}
		if matched == nil || matched.Name != "aaa-dup" || matched.Spec.Repositories[0].Access != nexusv1alpha1.AccessPull {
			t.Fatalf("iteration %d: selected %v, want aaa-dup with pull", i, matched)
		}
		if aud != hTestAudience {
			t.Errorf("aud = %q, want %q", aud, hTestAudience)
		}
	}
}

type unsortedNexusListClient struct {
	client.Client
	items []nexusv1alpha1.NexusAccess
}

func (c *unsortedNexusListClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l, ok := list.(*nexusv1alpha1.NexusAccessList)
	if !ok {
		return errors.New("unsortedNexusListClient: unexpected list type")
	}
	l.Items = c.items
	return nil
}

// Like HarborAccess objects, NexusAccess objects are listed by the
// indexed subject and without deep copies.
func TestFindNexusAccess_ListsByIndexedSubjectWithoutDeepCopy(t *testing.T) {
	other := newTestNXA()
	other.Name = "other"
	other.Spec.ServiceAccountRef.Name = "someone-else"
	var opts client.ListOptions
	listed := 0
	k8s := newNexusFakeClientBuilder().WithObjects(newTestNXA(), other).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, o ...client.ListOption) error {
				opts.ApplyOptions(o)
				err := c.List(ctx, list, o...)
				if l, ok := list.(*nexusv1alpha1.NexusAccessList); ok {
					listed = len(l.Items)
				}
				return err
			},
		}).Build()
	h := &Handler{K8sClient: k8s, Config: testNexusHandlerConfig(t)}
	matched, _, _, err := h.findNexusAccess(context.Background(), newTestNexusClaims())
	if err != nil {
		t.Fatal(err)
	}
	if matched == nil || matched.Name != nTestNXAName {
		t.Fatalf("matched %v, want %s", matched, nTestNXAName)
	}
	if v, ok := opts.FieldSelector.RequiresExactMatch(NexusAccessSubjectField); opts.FieldSelector == nil || !ok || v != nTestSubject {
		t.Errorf("listed with field selector %v, want %s=%s", opts.FieldSelector, NexusAccessSubjectField, nTestSubject)
	}
	if opts.UnsafeDisableDeepCopy == nil || !*opts.UnsafeDisableDeepCopy {
		t.Error("listed with deep copies")
	}
	if listed != 1 {
		t.Errorf("%d NexusAccess objects listed, want 1", listed)
	}
	if got := nexusAccessSubjectIndex(newTestHA()); got != nil {
		t.Errorf("index of a non-NexusAccess = %v, want nil", got)
	}
}

// ----------------------------------------------------------------------------
// Routing between the backends
// ----------------------------------------------------------------------------

// With both backends, an image of Harbor's host is served from the
// HarborAccess exactly as before, now with access_kind in the audit line;
// the Nexus metrics do not count it.
func TestHandler_Routing_HarborImageServedFromHarborAccess(t *testing.T) {
	f := newNexusFixture(t, newTestClaims())
	w := f.serve(t, nTestHarborImg)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
	}
	if got := decodeResp(t, w); got.Username != hTestRobotUser || got.Password != hTestRobotPass {
		t.Fatalf("response = %+v, want the robot's credentials", got)
	}
	f.expectAudit(t, `"credential issued"`, `"access_kind"="harbor"`, `"harboraccess"="`+hTestHANs+"/"+hTestHAName+`"`)
	for _, r := range nexusResults {
		if got := counter(t, f.Reg, "bridge_nexus_credential_issuances_total", map[string]string{"result": r}); got != 0 {
			t.Errorf("Nexus issuances{result=%q} = %v, want 0", r, got)
		}
	}
}

// A request never falls back to the other backend: a Nexus image of an
// identity that only has a HarborAccess is refused, and the other way
// round, although the other backend would have served the identity.
func TestHandler_Routing_NoFallbackToTheOtherBackend(t *testing.T) {
	harborOnly := newNexusFixture(t, newTestClaims(), newTestHA(), newTestRobotSecret())
	harborOnly.expectRefused(t, harborOnly.serve(t, nTestImage), http.StatusForbidden, ResultForbidden,
		`"reason"="no_matching_nexusaccess"`, `"access_kind"="nexus"`)

	nexusOnly := newNexusFixture(t, newTestNexusClaims(), newTestNXA(), newTestNexusSecret(t))
	w := nexusOnly.serve(t, nTestHarborImg)
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), nTestPassword) {
		t.Fatalf("status = %d body %q, want 403 without credentials", w.Code, w.Body.String())
	}
	nexusOnly.expectAudit(t, `"reason"="no_matching_harboraccess"`, `"access_kind"="harbor"`)
}

// An image of neither backend is refused once the token is valid, with an
// attributed audit line; so is a request that names no image.
func TestHandler_Routing_ImageOfNoBackend_Refused(t *testing.T) {
	for _, image := range []string{"docker.io/library/nginx:1.27", "nexus.example.com/docker-hosted/app:v1", ""} {
		t.Run(image, func(t *testing.T) {
			f := newNexusFixture(t, newTestNexusClaims())
			w := f.serve(t, image)
			if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), nTestPassword) {
				t.Fatalf("status = %d body %q, want 403 without credentials", w.Code, w.Body.String())
			}
			f.expectAudit(t, `"credential denied"`, `"reason"="no_backend"`, `"access_kind"="none"`, `"subject"="`+nTestSubject+`"`)
			if got := counter(t, f.Reg, "bridge_credential_issuances_total", map[string]string{"result": ResultForbidden}); got != 1 {
				t.Errorf("forbidden issuances = %v, want 1", got)
			}
			if got := counter(t, f.Reg, "bridge_nexus_credential_issuances_total", map[string]string{"result": ResultForbidden}); got != 0 {
				t.Errorf("forbidden Nexus issuances = %v, want 0", got)
			}
		})
	}
}

// A bad token on a Nexus image is counted and audited as the Nexus
// backend's; the token itself never reaches the log.
func TestHandler_Routing_InvalidTokenForNexusImage(t *testing.T) {
	f := newNexusFixture(t, nil)
	f.Handler.Validator = &stubValidator{err: errors.New("oidc: token is expired")}
	f.expectRefused(t, f.serve(t, nTestImage), http.StatusUnauthorized, ResultUnauthorized,
		`"reason"="invalid_token"`, `"access_kind"="nexus"`, `"category"="expired"`)
	if strings.Contains(f.Audit.joined(), "some-sa-token") {
		t.Fatal("audit line carries the token")
	}
}

// The Harbor path never serves a Secret of another access kind, even one
// at the robot Secret's name with the robot Secret's labels.
func TestHandler_Harbor_RefusesSecretOfAnotherAccessKind(t *testing.T) {
	sec := newTestRobotSecret()
	sec.Labels[nexussecret.LabelAccessKind] = nexussecret.AccessKindNexus
	for name, hc := range map[string]HandlerConfig{
		"Harbor alone": testHandlerConfig(),
		"with Nexus":   testNexusHandlerConfig(t),
	} {
		t.Run(name, func(t *testing.T) {
			var audit captured
			h := &Handler{
				K8sClient: newNexusFakeClientBuilder().WithObjects(newTestHA(), sec).Build(),
				Validator: &stubValidator{claims: newTestClaims()},
				Config:    hc,
				Audit:     audit.logger(),
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, bearerReq(t, nTestHarborImg))
			if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), hTestRobotPass) {
				t.Fatalf("status = %d body %q, want 403 without credentials", w.Code, w.Body.String())
			}
			if out := audit.joined(); !strings.Contains(out, `"reason"="secret_owner_mismatch"`) {
				t.Errorf("audit line lacks secret_owner_mismatch:\n%s", out)
			}
		})
	}
}

// With Harbor alone nothing changes: every image is Harbor's (a Nexus
// host's too), the audit lines carry no access_kind, and no Nexus series
// is exported.
func TestHandler_HarborAlone_Unchanged(t *testing.T) {
	var audit captured
	reg := prometheus.NewRegistry()
	h := &Handler{
		K8sClient: newNexusFakeClientBuilder().WithObjects(newTestHA(), newTestRobotSecret(), newTestNXA(), newTestNexusSecret(t)).Build(),
		Validator: &stubValidator{claims: newTestClaims()},
		Config:    testHandlerConfig(),
		Metrics:   NewMetrics(reg),
		Audit:     audit.logger(),
	}
	for _, image := range []string{nTestHarborImg, nTestImage, "docker.io/library/nginx", ""} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, bearerReq(t, image))
		if w.Code != http.StatusOK || decodeResp(t, w).Username != hTestRobotUser {
			t.Fatalf("image %q: status = %d, want the robot's credentials", image, w.Code)
		}
	}
	if out := audit.joined(); strings.Contains(out, "access_kind") {
		t.Errorf("Harbor-only audit lines carry access_kind:\n%s", out)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if strings.Contains(mf.GetName(), "nexus") {
			t.Errorf("Harbor-only bridge exports %s", mf.GetName())
		}
	}
}
