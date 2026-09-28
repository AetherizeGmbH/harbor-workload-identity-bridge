// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus/nexustest"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/nexussecret"
)

const (
	testNXANamespace = "tenant"
	testNXAName      = "web"
	testRepo         = "docker-hosted"
)

// testIdentity is the Nexus role id and user-id stem of newNexusAccess's
// ServiceAccount.
var testIdentity = "bridge-" + testCluster + "." + testSANamespace + "." + testSAName

// stepClock is a Clock tests move forward.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func newStepClock() *stepClock {
	return &stepClock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newNexusAccess() *nexusv1alpha1.NexusAccess {
	return &nexusv1alpha1.NexusAccess{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testNXAName,
			Namespace:  testNXANamespace,
			Generation: 1,
			UID:        "3f6d2c1e-0000-4000-8000-000000000001",
			Finalizers: []string{NexusFinalizerName},
		},
		Spec: nexusv1alpha1.NexusAccessSpec{
			ServiceAccountRef: nexusv1alpha1.ServiceAccountRef{Namespace: testSANamespace, Name: testSAName},
			TrustPolicy:       nexusv1alpha1.TrustPolicy{Issuer: testIssuer, Audience: "harbor.example.com"},
			Repositories:      []nexusv1alpha1.RepositoryGrant{{Name: testRepo, Access: nexusv1alpha1.AccessPull, Format: nexusv1alpha1.FormatDocker}},
			TokenTTL:          harborv1alpha1.Duration{Duration: time.Hour},
		},
	}
}

// nexusHarness is a NexusReconciler against a fake apiserver and a fake
// Nexus.
type nexusHarness struct {
	t     *testing.T
	r     *NexusReconciler
	nx    *nexustest.Server
	clock *stepClock
	key   types.NamespacedName
}

func newNexusHarness(t *testing.T, funcs *interceptor.Funcs, objects ...client.Object) *nexusHarness {
	t.Helper()
	nx := nexustest.New(t)
	nx.AddRepository("docker", testRepo)
	clock := newStepClock()
	b := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objects...).
		WithStatusSubresource(&nexusv1alpha1.NexusAccess{})
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	backoff := &NexusBackoff{Window: DefaultNexusRateLimitBackoff, Clock: clock}
	r := &NexusReconciler{
		Client:  b.Build(),
		Scheme:  testScheme,
		Nexus:   backoff.Wrap(nx.Client(t)),
		Backoff: backoff,
		Config:  testReconcilerConfig(),
		Clock:   clock,
	}
	return &nexusHarness{t: t, r: r, nx: nx, clock: clock, key: types.NamespacedName{Namespace: testNXANamespace, Name: testNXAName}}
}

func (h *nexusHarness) reconcile() (ctrl.Result, error) {
	h.t.Helper()
	return h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
}

func (h *nexusHarness) mustReconcile() ctrl.Result {
	h.t.Helper()
	res, err := h.reconcile()
	if err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (h *nexusHarness) object() *nexusv1alpha1.NexusAccess {
	h.t.Helper()
	nxa := &nexusv1alpha1.NexusAccess{}
	if err := h.r.Get(context.Background(), h.key, nxa); err != nil {
		h.t.Fatalf("get NexusAccess: %v", err)
	}
	return nxa
}

func (h *nexusHarness) edit(fn func(*nexusv1alpha1.NexusAccess)) {
	h.t.Helper()
	nxa := h.object()
	fn(nxa)
	nxa.Generation++
	if err := h.r.Update(context.Background(), nxa); err != nil {
		h.t.Fatalf("update NexusAccess: %v", err)
	}
}

// secret returns the NexusAccess's Secret; nil when absent.
func (h *nexusHarness) secret() *corev1.Secret {
	h.t.Helper()
	s := &corev1.Secret{}
	err := h.r.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: nexussecret.Name(testNXANamespace, testNXAName)}, s)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// storedAuthenticates reports whether the Secret holds credentials Nexus
// accepts, and returns the user id.
func (h *nexusHarness) storedAuthenticates() (string, bool) {
	h.t.Helper()
	s := h.secret()
	if s == nil {
		return "", false
	}
	user, pass := string(s.Data["username"]), string(s.Data["password"])
	return user, h.nx.Authenticate(user, pass)
}

func (h *nexusHarness) userIDs() []string {
	var ids []string
	for _, u := range h.nx.Users() {
		ids = append(ids, u.UserID)
	}
	return ids
}

func assertNexusCondition(t *testing.T, nxa *nexusv1alpha1.NexusAccess, typ string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(nxa.Status.Conditions, typ)
	if c == nil {
		t.Fatalf("condition %s missing; conditions: %+v", typ, nxa.Status.Conditions)
	}
	if c.Status != status || c.Reason != reason {
		t.Fatalf("condition %s = %s/%s (%s), want %s/%s", typ, c.Status, c.Reason, c.Message, status, reason)
	}
}

func TestNexusReconcile_CreatesRoleUserAndSecret(t *testing.T) {
	nxa := newNexusAccess()
	nxa.Finalizers = nil
	h := newNexusHarness(t, nil, nxa)
	res := h.mustReconcile()

	role, ok := h.nx.Role(testIdentity)
	if !ok {
		t.Fatalf("role %q not created; roles: %+v", testIdentity, h.nx.Roles())
	}
	if !slices.Equal(role.Privileges, []string{"nx-repository-view-docker-" + testRepo + "-read"}) {
		t.Errorf("role privileges = %v", role.Privileges)
	}
	if role.Description != NexusRoleDescription(testCluster, testNXANamespace, testNXAName) {
		t.Errorf("role description = %q", role.Description)
	}
	userID, ok := h.storedAuthenticates()
	if !ok {
		t.Fatal("the Secret does not hold credentials Nexus accepts")
	}
	if id, gen, ok := nexus.ParseUserID(userID); !ok || id != testIdentity || len(gen) != nexus.GenerationLen {
		t.Errorf("user id %q is not <identity>_<generation>", userID)
	}
	u, _ := h.nx.User(userID)
	if !slices.Equal(u.Roles, []string{testIdentity}) || u.Status != nexus.UserActive ||
		u.FirstName != NexusUserFirstName(testCluster) || u.LastName != NexusUserLastName(testNXANamespace, testNXAName) {
		t.Errorf("user = %+v", u)
	}

	s := h.secret()
	if !nexussecret.OwnedBy(s, testNXANamespace, testNXAName) || s.Labels[nexussecret.LabelCluster] != testCluster {
		t.Errorf("Secret labels = %v", s.Labels)
	}
	if nexussecret.UserID(s) != userID || nexussecret.PendingUserID(s) != "" {
		t.Errorf("Secret annotations = %v", s.Annotations)
	}
	if nb, ok := nexussecret.RotationNotBefore(s); !ok || !nb.Equal(h.clock.Now().Add(PasswordRotationInterval)) {
		t.Errorf("rotation-not-before = %v, %v", nb, ok)
	}
	if _, incomplete := nexussecret.GrantsIncomplete(s); incomplete {
		t.Error("grants-incomplete on a complete grant")
	}

	got := h.object()
	assertNexusCondition(t, got, nexusv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
	assertNexusCondition(t, got, nexusv1alpha1.ConditionUserProvisioned, metav1.ConditionTrue, ReasonReconcileSucceeded)
	assertNexusCondition(t, got, nexusv1alpha1.ConditionTrustPolicyApplied, metav1.ConditionTrue, ReasonEnforcedByBridge)
	if !slices.Contains(got.Finalizers, NexusFinalizerName) {
		t.Errorf("finalizers = %v", got.Finalizers)
	}
	if got.Status.ObservedGeneration != 1 || got.Status.User == nil || got.Status.User.UserID != userID ||
		got.Status.User.RoleID != testIdentity || got.Status.User.PasswordSecretRef != s.Name || got.Status.User.LastRotated == nil {
		t.Errorf("status = %+v, user %+v", got.Status, got.Status.User)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > ResyncInterval {
		t.Errorf("RequeueAfter = %s", res.RequeueAfter)
	}

	// A second pass changes nothing and creates no user.
	h.mustReconcile()
	if ids := h.userIDs(); len(ids) != 1 || ids[0] != userID {
		t.Errorf("users after a second pass = %v", ids)
	}
}

// Every refusal deletes the Secret and the users of this NexusAccess
// (ADR-0030 parity); a user an administrator disabled stays, and the role
// stays for the resume.
func TestNexusReconcile_RefusalRevokesUsersAndSecret(t *testing.T) {
	for name, edit := range map[string]func(*nexusv1alpha1.NexusAccess){
		ReasonAudienceMismatch: func(n *nexusv1alpha1.NexusAccess) { n.Spec.TrustPolicy.Audience = "another-bridge" },
		ReasonIssuerMismatch:   func(n *nexusv1alpha1.NexusAccess) { n.Spec.TrustPolicy.Issuer = "https://other.example.com" },
		ReasonInvalidSpec:      func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories[0].Name = "*" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newNexusHarness(t, nil, newNexusAccess())
			h.mustReconcile()
			first, _ := h.storedAuthenticates()
			h.edit(edit)
			res := h.mustReconcile()
			if h.secret() != nil {
				t.Error("the Secret of a refused NexusAccess survived")
			}
			if _, ok := h.nx.User(first); ok {
				t.Error("the user of a refused NexusAccess survived")
			}
			if _, ok := h.nx.Role(testIdentity); !ok {
				t.Error("the role went; it stays for the resume")
			}
			got := h.object()
			assertNexusCondition(t, got, nexusv1alpha1.ConditionReady, metav1.ConditionFalse, name)
			assertNexusCondition(t, got, nexusv1alpha1.ConditionUserProvisioned, metav1.ConditionFalse, name)
			if !slices.Contains(got.Finalizers, NexusFinalizerName) {
				t.Error("the finalizer went while the role exists")
			}
			if res.RequeueAfter <= 0 {
				t.Error("a refused NexusAccess is not re-checked")
			}
		})
	}
}

func TestNexusReconcile_ResumeCreatesANewUser(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	first, _ := h.storedAuthenticates()
	h.edit(func(n *nexusv1alpha1.NexusAccess) { n.Spec.TrustPolicy.Audience = "another-bridge" })
	h.mustReconcile()
	h.edit(func(n *nexusv1alpha1.NexusAccess) { n.Spec.TrustPolicy.Audience = "harbor.example.com" })
	h.mustReconcile()
	second, ok := h.storedAuthenticates()
	if !ok || second == first {
		t.Fatalf("resumed user %q (authenticates %v), first %q: want a new generation", second, ok, first)
	}
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
}

// A refused object this bridge never provisioned costs no Nexus call, and
// one that holds the finalizer without anything in Nexus is released
// (ADR-0032).
func TestNexusReconcile_RefusedWithoutUsers(t *testing.T) {
	nxa := newNexusAccess()
	nxa.Finalizers = nil
	nxa.Spec.TrustPolicy.Audience = "another-bridge"
	h := newNexusHarness(t, nil, nxa)
	h.mustReconcile()
	if n := len(h.nx.Requests()); n != 0 {
		t.Errorf("%d Nexus requests for an object without a finalizer of this bridge", n)
	}

	held := newNexusAccess()
	held.Spec.TrustPolicy.Audience = "another-bridge"
	h = newNexusHarness(t, nil, held)
	h.mustReconcile()
	if f := h.object().Finalizers; len(f) != 0 {
		t.Errorf("finalizers of a refused object without users = %v, want released", f)
	}
}

func TestNexusReconcile_RefusalKeepsAnAdministratorsDisable(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	first, _ := h.storedAuthenticates()
	h.nx.SetUserStatus(first, nexus.UserDisabled)
	h.edit(func(n *nexusv1alpha1.NexusAccess) { n.Spec.TrustPolicy.Audience = "another-bridge" })
	h.mustReconcile()
	if _, ok := h.nx.User(first); !ok {
		t.Fatal("a user an administrator disabled was deleted")
	}
	// Accepted again: the disabled user is neither replaced nor enabled.
	h.edit(func(n *nexusv1alpha1.NexusAccess) { n.Spec.TrustPolicy.Audience = "harbor.example.com" })
	h.mustReconcile()
	if ids := h.userIDs(); !slices.Equal(ids, []string{first}) {
		t.Errorf("users = %v, want only the disabled %q", ids, first)
	}
	if u, _ := h.nx.User(first); u.Status != nexus.UserDisabled {
		t.Errorf("status = %q, the bridge re-enabled it", u.Status)
	}
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonUserDisabled)
}

func TestNexusReconcile_GrantChangeWithoutRotation(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.nx.AddRepository("docker", "ci-images")
	h.mustReconcile()
	before := h.secret()
	h.edit(func(n *nexusv1alpha1.NexusAccess) {
		n.Spec.Repositories = append(n.Spec.Repositories, nexusv1alpha1.RepositoryGrant{Name: "ci-images", Access: nexusv1alpha1.AccessPullPush})
	})
	h.mustReconcile()
	role, _ := h.nx.Role(testIdentity)
	want := []string{
		"nx-repository-view-docker-ci-images-add", "nx-repository-view-docker-ci-images-edit",
		"nx-repository-view-docker-ci-images-read", "nx-repository-view-docker-" + testRepo + "-read",
	}
	if !slices.Equal(role.Privileges, want) {
		t.Errorf("privileges = %v, want %v", role.Privileges, want)
	}
	after := h.secret()
	if string(after.Data["password"]) != string(before.Data["password"]) || string(after.Data["username"]) != string(before.Data["username"]) {
		t.Error("a grant change replaced the user")
	}
	got := h.object()
	if got.Status.ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d", got.Status.ObservedGeneration)
	}
}

// ADR-0033 decision d: an edit that removes repository A and names a
// missing repository C revokes A in the same pass, adds nothing missing,
// marks the Secret and holds observedGeneration back; once C exists the
// full grant is written and the mark removed.
func TestNexusReconcile_MissingRepositoryHoldsBackNoRemoval(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.nx.AddRepository("docker", "b")
	h.edit(func(n *nexusv1alpha1.NexusAccess) {
		n.Spec.Repositories = append(n.Spec.Repositories, nexusv1alpha1.RepositoryGrant{Name: "b", Access: nexusv1alpha1.AccessPull})
	})
	h.mustReconcile()
	gen := h.object().Generation

	h.edit(func(n *nexusv1alpha1.NexusAccess) {
		n.Spec.Repositories = []nexusv1alpha1.RepositoryGrant{
			{Name: "b", Access: nexusv1alpha1.AccessPull},
			{Name: "c", Access: nexusv1alpha1.AccessPull},
		}
	})
	res := h.mustReconcile()
	role, _ := h.nx.Role(testIdentity)
	if !slices.Equal(role.Privileges, []string{"nx-repository-view-docker-b-read"}) {
		t.Errorf("privileges = %v, want only b's: %s's revoked, c's not added", role.Privileges, testRepo)
	}
	missing, incomplete := nexussecret.GrantsIncomplete(h.secret())
	if !incomplete || !slices.Equal(missing, []string{"c"}) {
		t.Errorf("grants-incomplete = %v, %v", missing, incomplete)
	}
	got := h.object()
	assertNexusCondition(t, got, nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRepositoryNotFound)
	if got.Status.ObservedGeneration != gen {
		t.Errorf("observedGeneration = %d, want %d (not advanced)", got.Status.ObservedGeneration, gen)
	}
	if res.RequeueAfter <= 0 {
		t.Error("RepositoryNotFound is not re-checked")
	}

	h.nx.AddRepository("docker", "c")
	h.mustReconcile()
	role, _ = h.nx.Role(testIdentity)
	if !slices.Equal(role.Privileges, []string{"nx-repository-view-docker-b-read", "nx-repository-view-docker-c-read"}) {
		t.Errorf("privileges = %v", role.Privileges)
	}
	if _, incomplete := nexussecret.GrantsIncomplete(h.secret()); incomplete {
		t.Error("grants-incomplete kept after the repository appeared")
	}
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
}

func TestNexusReconcile_MissingRepositoryOnCreate(t *testing.T) {
	nxa := newNexusAccess()
	nxa.Spec.Repositories = append(nxa.Spec.Repositories, nexusv1alpha1.RepositoryGrant{Name: "absent", Access: nexusv1alpha1.AccessPull})
	h := newNexusHarness(t, nil, nxa)
	h.mustReconcile()
	if _, ok := h.storedAuthenticates(); !ok {
		t.Fatal("no user: rotation and creation continue while a repository is missing")
	}
	if missing, incomplete := nexussecret.GrantsIncomplete(h.secret()); !incomplete || !slices.Equal(missing, []string{"absent"}) {
		t.Errorf("grants-incomplete = %v, %v", missing, incomplete)
	}
	got := h.object()
	assertNexusCondition(t, got, nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRepositoryNotFound)
	assertNexusCondition(t, got, nexusv1alpha1.ConditionUserProvisioned, metav1.ConditionTrue, ReasonReconcileSucceeded)
	if got.Status.ObservedGeneration != 0 {
		t.Errorf("observedGeneration = %d, want 0", got.Status.ObservedGeneration)
	}
}

// A repository deleted in Nexus strips its privileges; the object turns
// RepositoryNotFound, and a re-created repository is granted again.
func TestNexusReconcile_RepositoryDeletedOutOfBand(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	h.nx.DeleteRepository("docker", testRepo)
	h.mustReconcile()
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRepositoryNotFound)
	h.nx.AddRepository("docker", testRepo)
	h.mustReconcile()
	role, _ := h.nx.Role(testIdentity)
	if len(role.Privileges) != 1 {
		t.Errorf("privileges after the repository came back = %v", role.Privileges)
	}
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
}

// A repository that vanishes between the check and the role write: before
// Nexus 3.91 the write fails with 400 and is repeated once; from 3.91 on
// Nexus stores the role without it. Either way the object is
// RepositoryNotFound in the same pass.
func TestNexusReconcile_RepositoryVanishesDuringTheWrite(t *testing.T) {
	for _, dropOrphans := range []bool{false, true} {
		h := newNexusHarness(t, nil, newNexusAccess())
		h.nx.AddRepository("docker", "b")
		h.mustReconcile()
		h.nx.SetDropOrphanPrivileges(dropOrphans)
		var once sync.Once
		h.nx.SetHook(func(_ http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/v1/security/roles/") {
				once.Do(func() { h.nx.DeleteRepository("docker", "b") })
			}
			return false
		})
		h.edit(func(n *nexusv1alpha1.NexusAccess) {
			n.Spec.Repositories = append(n.Spec.Repositories, nexusv1alpha1.RepositoryGrant{Name: "b", Access: nexusv1alpha1.AccessPull})
		})
		h.mustReconcile()
		role, _ := h.nx.Role(testIdentity)
		if !slices.Equal(role.Privileges, []string{"nx-repository-view-docker-" + testRepo + "-read"}) {
			t.Errorf("3.91+ %v: privileges = %v", dropOrphans, role.Privileges)
		}
		assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRepositoryNotFound)
		if missing, _ := nexussecret.GrantsIncomplete(h.secret()); !slices.Equal(missing, []string{"b"}) {
			t.Errorf("3.91+ %v: grants-incomplete = %v", dropOrphans, missing)
		}
	}
}

// ADR-0033 decision c: a rotation creates the next generation, stores it,
// and deletes the previous user after the grace, never before the promise.
func TestNexusReconcile_ScheduledRotationReplacesTheUser(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	first, _ := h.storedAuthenticates()
	firstSecret := h.secret()

	h.clock.Advance(PasswordRotationInterval + RotationSafetyMargin - time.Second)
	h.mustReconcile()
	if got, _ := h.storedAuthenticates(); got != first {
		t.Fatalf("rotated before rotation-not-before plus the margin: %q", got)
	}

	h.clock.Advance(time.Second)
	res := h.mustReconcile()
	second, ok := h.storedAuthenticates()
	if !ok || second == first {
		t.Fatalf("after the promise: user %q (authenticates %v), want a new generation", second, ok)
	}
	s := h.secret()
	id, after, retiring := nexussecret.Retiring(s)
	if !retiring || id != first || !after.Equal(h.clock.Now().Add(NexusUserRetireGrace)) {
		t.Errorf("retiring = %q until %s (%v), want %q until now+grace", id, after, retiring, first)
	}
	if !h.nx.Authenticate(first, string(firstSecret.Data["password"])) {
		t.Error("the previous user is gone before its grace passed")
	}
	if res.RequeueAfter > NexusUserRetireGrace {
		t.Errorf("RequeueAfter = %s, want at most the grace", res.RequeueAfter)
	}
	got := h.object()
	if got.Status.User.UserID != second || !got.Status.User.LastRotated.Time.Equal(h.clock.Now()) {
		t.Errorf("status user = %+v", got.Status.User)
	}

	h.clock.Advance(NexusUserRetireGrace)
	h.mustReconcile()
	if _, ok := h.nx.User(first); ok {
		t.Error("the previous user survived its grace")
	}
	if _, _, retiring := nexussecret.Retiring(h.secret()); retiring {
		t.Error("the retiring record survived the retirement")
	}
	if ids := h.userIDs(); !slices.Equal(ids, []string{second}) {
		t.Errorf("users = %v", ids)
	}
}

// A deleted Secret is an emergency rotation: the previous user goes at
// once, not after a grace.
func TestNexusReconcile_DeletedSecretRevokesThePreviousUserAtOnce(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	first, _ := h.storedAuthenticates()
	if err := h.r.Delete(context.Background(), h.secret()); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile()
	second, ok := h.storedAuthenticates()
	if !ok || second == first {
		t.Fatalf("user after the Secret was deleted = %q (%v)", second, ok)
	}
	if _, ok := h.nx.User(first); ok {
		t.Error("the previous user survived an emergency rotation")
	}
}

func TestNexusReconcile_UserDeletedOutOfBandIsReplaced(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	first, _ := h.storedAuthenticates()
	h.nx.RemoveUser(first)
	h.mustReconcile()
	if second, ok := h.storedAuthenticates(); !ok || second == first {
		t.Errorf("user = %q (%v), want a new generation", second, ok)
	}
}

func TestNexusReconcile_AdministratorsDisableIsRespected(t *testing.T) {
	for _, status := range []nexus.UserStatus{nexus.UserDisabled, nexus.UserLocked} {
		h := newNexusHarness(t, nil, newNexusAccess())
		h.mustReconcile()
		first, _ := h.storedAuthenticates()
		h.nx.SetUserStatus(first, status)
		h.clock.Advance(2 * PasswordRotationInterval)
		h.mustReconcile()
		if ids := h.userIDs(); !slices.Equal(ids, []string{first}) {
			t.Errorf("%s: users = %v, want the %s user only (not rotated)", status, ids, status)
		}
		if u, _ := h.nx.User(first); u.Status != status {
			t.Errorf("%s: status changed to %q", status, u.Status)
		}
		got := h.object()
		assertNexusCondition(t, got, nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonUserDisabled)
		assertNexusCondition(t, got, nexusv1alpha1.ConditionUserProvisioned, metav1.ConditionFalse, ReasonUserDisabled)
	}
}

// A user in status changepassword authenticates (ADR-0033): it is rotated
// like an active one, and its replacement is active.
func TestNexusReconcile_ChangePasswordUserIsRotated(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	first, _ := h.storedAuthenticates()
	h.nx.SetUserStatus(first, nexus.UserChangePassword)
	h.mustReconcile()
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
	h.clock.Advance(PasswordRotationInterval + RotationSafetyMargin)
	h.mustReconcile()
	second, ok := h.storedAuthenticates()
	if u, _ := h.nx.User(second); !ok || second == first || u.Status != nexus.UserActive {
		t.Errorf("replacement %q (authenticates %v, status %q)", second, ok, u.Status)
	}
}

func TestNexusReconcile_ServiceAccountChangeRevokesTheOldIdentity(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	first, _ := h.storedAuthenticates()
	h.edit(func(n *nexusv1alpha1.NexusAccess) { n.Spec.ServiceAccountRef.Name = "new-sa" })
	h.mustReconcile()
	newIdentity := "bridge-" + testCluster + "." + testSANamespace + ".new-sa"
	second, ok := h.storedAuthenticates()
	if id, _, _ := nexus.ParseUserID(second); !ok || id != newIdentity {
		t.Fatalf("user after the SA change = %q (%v)", second, ok)
	}
	if _, ok := h.nx.User(first); ok {
		t.Error("the previous identity's user survived")
	}
	if _, ok := h.nx.Role(testIdentity); ok {
		t.Error("the previous identity's role survived")
	}
	if _, ok := h.nx.Role(newIdentity); !ok {
		t.Error("the new identity has no role")
	}
	if _, _, retiring := nexussecret.Retiring(h.secret()); retiring {
		t.Error("the previous identity's user was kept as retiring; an identity change revokes at once")
	}
}

func TestNexusReconcile_RoleConflicts(t *testing.T) {
	for name, role := range map[string]nexus.Role{
		"another NexusAccess": {ID: testIdentity, Name: testIdentity, Description: NexusRoleDescription(testCluster, "other", "app")},
		"not the bridge's":    {ID: testIdentity, Name: testIdentity, Description: "hand-made"},
		"another cluster":     {ID: testIdentity, Name: testIdentity, Description: NexusRoleDescription("prod", testNXANamespace, testNXAName)},
	} {
		t.Run(name, func(t *testing.T) {
			h := newNexusHarness(t, nil, newNexusAccess())
			h.nx.PutRole(role)
			h.mustReconcile()
			assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRoleConflict)
			if len(h.nx.Users()) != 0 || h.secret() != nil {
				t.Error("a user or Secret was created despite the conflict")
			}
			if got, _ := h.nx.Role(testIdentity); got.Description != role.Description {
				t.Error("the foreign role was modified")
			}
		})
	}
}

func TestNexusReconcile_SecretConflicts(t *testing.T) {
	name := nexussecret.Name(testNXANamespace, testNXAName)
	for what, s := range map[string]*corev1.Secret{
		"unmanaged": {ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}},
		"another NexusAccess": {ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS,
			Labels: nexussecret.Labels(testCluster, testNXANamespace, "other")}},
	} {
		t.Run(what, func(t *testing.T) {
			h := newNexusHarness(t, nil, newNexusAccess(), s)
			h.mustReconcile()
			assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonUserConflict)
			if len(h.nx.Users()) != 0 {
				t.Error("a user was created despite the conflict")
			}
		})
	}
}

// Drift made in Nexus is reverted: a role an administrator gave the user,
// a role contained in the bridge's role, a privilege added to it, a role
// deleted and re-created.
func TestNexusReconcile_RevertsDrift(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	userID, _ := h.storedAuthenticates()
	h.nx.PutRole(nexus.Role{ID: "nx-admin", Name: "nx-admin"})
	u, _ := h.nx.User(userID)
	u.Roles = append(u.Roles, "nx-admin")
	h.nx.PutUser(u, "unchanged")
	role, _ := h.nx.Role(testIdentity)
	role.Roles = []string{"nx-admin"}
	role.Privileges = append(role.Privileges, "nx-all")
	h.nx.PutRole(role)
	h.mustReconcile()
	if u, _ := h.nx.User(userID); !slices.Equal(u.Roles, []string{testIdentity}) {
		t.Errorf("user roles = %v", u.Roles)
	}
	if role, _ := h.nx.Role(testIdentity); len(role.Roles) != 0 || len(role.Privileges) != 1 {
		t.Errorf("role = %+v", role)
	}

	h.nx.RemoveRole(testIdentity)
	h.mustReconcile()
	if u, _ := h.nx.User(userID); !slices.Equal(u.Roles, []string{testIdentity}) {
		t.Errorf("user roles after the role was re-created = %v", u.Roles)
	}
}

// A user whose markers were changed out of band is neither used nor
// replaced: a new generation would leave it with a valid password the
// bridge can never revoke.
func TestNexusReconcile_ForeignMarkersOnTheCurrentUser(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	userID, _ := h.storedAuthenticates()
	u, _ := h.nx.User(userID)
	u.LastName = "someone else"
	h.nx.PutUser(u, "x")
	h.mustReconcile()
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonUserConflict)
	if ids := h.userIDs(); !slices.Equal(ids, []string{userID}) {
		t.Errorf("users = %v", ids)
	}
}

func TestNexusReconcile_DeletionRevokesEverything(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	first, _ := h.storedAuthenticates()
	h.nx.SetUserStatus(first, nexus.UserDisabled)
	// A user of a previous identity, left behind by a failed cleanup.
	h.nx.PutUser(nexus.User{UserID: "bridge-" + testCluster + ".old.sa_0123456789abcdef", Status: nexus.UserActive,
		FirstName: NexusUserFirstName(testCluster), LastName: NexusUserLastName(testNXANamespace, testNXAName)}, "pw")
	// Another NexusAccess's user, which must stay.
	foreign := "bridge-" + testCluster + ".other.sa_0123456789abcdef"
	h.nx.PutUser(nexus.User{UserID: foreign, Status: nexus.UserActive,
		FirstName: NexusUserFirstName(testCluster), LastName: NexusUserLastName("other", "app")}, "pw")

	if err := h.r.Delete(context.Background(), h.object()); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile()
	if ids := h.userIDs(); !slices.Equal(ids, []string{foreign}) {
		t.Errorf("users after deletion = %v, want only %q", ids, foreign)
	}
	if len(h.nx.Roles()) != 0 {
		t.Errorf("roles after deletion = %+v", h.nx.Roles())
	}
	if h.secret() != nil {
		t.Error("Secret survived the deletion")
	}
	if err := h.r.Get(context.Background(), h.key, &nexusv1alpha1.NexusAccess{}); !apierrors.IsNotFound(err) {
		t.Errorf("object after deletion: %v, want it gone", err)
	}
}

func TestNexusReconcile_DeletionBlockedWhileNexusIsDown(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	if err := h.r.Delete(context.Background(), h.object()); err != nil {
		t.Fatal(err)
	}
	h.nx.SetUnavailable(true)
	if _, err := h.reconcile(); err == nil {
		t.Fatal("deletion while Nexus is down returned no error")
	}
	got := h.object()
	assertNexusCondition(t, got, nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonDeletionBlocked)
	if !slices.Contains(got.Finalizers, NexusFinalizerName) {
		t.Error("the finalizer went while Nexus was down")
	}
	h.nx.SetUnavailable(false)
	h.mustReconcile()
	if len(h.nx.Users()) != 0 {
		t.Error("users survived the deletion once Nexus was back")
	}
}

// ADR-0033 decision j: a 429 stops every call for the backoff window,
// counted from the last 429, without probing.
func TestNexusReconcile_RateLimitStopsEveryCall(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	h.nx.SetRateLimited(true)
	h.clock.Advance(PasswordRotationInterval + RotationSafetyMargin)
	res, err := h.reconcile()
	if err != nil {
		t.Fatalf("a rate-limited pass returned an error (backoff would hammer the queue): %v", err)
	}
	if res.RequeueAfter < DefaultNexusRateLimitBackoff-time.Second || res.RequeueAfter > DefaultNexusRateLimitBackoff {
		t.Errorf("RequeueAfter = %s, want the backoff window", res.RequeueAfter)
	}
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonNexusRateLimited)
	if c := meta.FindStatusCondition(h.object().Status.Conditions, nexusv1alpha1.ConditionReady); !strings.Contains(c.Message, EnvNexusRateLimitBackoff) {
		t.Errorf("message %q does not explain the backoff", c.Message)
	}

	calls := len(h.nx.Requests())
	h.clock.Advance(DefaultNexusRateLimitBackoff - time.Second)
	h.mustReconcile()
	if n := len(h.nx.Requests()); n != calls {
		t.Errorf("%d Nexus calls during the backoff", n-calls)
	}
	if until, blocked := h.r.Backoff.Until(); !blocked || !until.After(h.clock.Now()) {
		t.Errorf("backoff until %s, blocked %v", until, blocked)
	}

	h.nx.SetRateLimited(false)
	h.clock.Advance(time.Second)
	h.mustReconcile()
	if n := len(h.nx.Requests()); n == calls {
		t.Error("no Nexus call after the backoff ended")
	}
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
}

func TestNexusReconcile_ErrorReasons(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.nx.SetHook(func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusUnauthorized)
		return true
	})
	if _, err := h.reconcile(); err == nil {
		t.Fatal("401 returned no error")
	}
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonNexusAuthFailed)

	h.nx.SetHook(nil)
	h.nx.SetPasswordPolicy(func(string) bool { return false })
	if _, err := h.reconcile(); err == nil {
		t.Fatal("a refused password returned no error")
	}
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonPasswordRejected)
	if len(h.nx.Users()) != 0 {
		t.Error("a user exists although its creation was refused")
	}

	h.nx.SetPasswordPolicy(nil)
	h.nx.SetUnavailable(true)
	if _, err := h.reconcile(); err == nil {
		t.Fatal("503 returned no error")
	}
	assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonNexusError)
}

// A pass that created a user and could not store its password leaves the
// pending record; the next pass creates another generation and deletes the
// user whose password was lost.
func TestNexusReconcile_LostPasswordIsRecovered(t *testing.T) {
	var fail bool
	var mu sync.Mutex
	funcs := &interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		mu.Lock()
		defer mu.Unlock()
		// The write that stores a new password: data, no pending record.
		if s, ok := obj.(*corev1.Secret); ok && fail && len(s.Data) > 0 && nexussecret.PendingUserID(s) == "" {
			return errors.New("simulated apiserver failure")
		}
		return c.Update(ctx, obj, opts...)
	}}
	h := newNexusHarness(t, funcs, newNexusAccess())
	h.mustReconcile()
	first, _ := h.storedAuthenticates()
	mu.Lock()
	fail = true
	mu.Unlock()
	h.clock.Advance(PasswordRotationInterval + RotationSafetyMargin)
	if _, err := h.reconcile(); err == nil {
		t.Fatal("a failed password write returned no error")
	}
	lost := nexussecret.PendingUserID(h.secret())
	if _, ok := h.nx.User(lost); !ok || lost == "" {
		t.Fatalf("pending %q not created", lost)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	h.mustReconcile()
	third, ok := h.storedAuthenticates()
	if !ok || third == lost || third == first {
		t.Fatalf("user = %q (%v); lost %q, first %q", third, ok, lost, first)
	}
	if _, ok := h.nx.User(lost); ok {
		t.Error("the user whose password was lost survived")
	}
}

// A pending record left by a failed pass outside a rotation is cleared, so
// the janitor no longer spares the user it names.
func TestNexusReconcile_ClearsAStalePendingRecord(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	s := h.secret()
	s.Annotations[nexussecret.AnnotationPendingUserID] = testIdentity + "_0123456789abcdef"
	if err := h.r.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	h.mustReconcile()
	if p := nexussecret.PendingUserID(h.secret()); p != "" {
		t.Errorf("pending record %q survived", p)
	}
}

func TestNexusReconcile_IgnoresUnselectedObjects(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	sel, err := labelsParse("bridge=eu")
	if err != nil {
		t.Fatal(err)
	}
	h.r.Config.HarborAccessSelector, h.r.Config.Instance = sel, "eu"
	h.mustReconcile()
	if len(h.nx.Requests()) != 0 || h.secret() != nil {
		t.Error("an unselected NexusAccess was reconciled")
	}
}

func TestNexusReconcile_InvalidSpecs(t *testing.T) {
	for name, edit := range map[string]func(*nexusv1alpha1.NexusAccess){
		"empty spec":       func(n *nexusv1alpha1.NexusAccess) { n.Spec = nexusv1alpha1.NexusAccessSpec{} },
		"long name":        func(n *nexusv1alpha1.NexusAccess) { n.Name = strings.Repeat("n", 64) },
		"oci format":       func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories[0].Format = "oci" },
		"unknown access":   func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories[0].Access = "delete" },
		"no repositories":  func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories = nil },
		"invalid tokenTTL": func(n *nexusv1alpha1.NexusAccess) { _ = n.Spec.TokenTTL.UnmarshalJSON([]byte(`"1d"`)) },
		"long namespace":   func(n *nexusv1alpha1.NexusAccess) { n.Spec.ServiceAccountRef.Namespace = strings.Repeat("a", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			nxa := newNexusAccess()
			edit(nxa)
			h := newNexusHarness(t, nil, nxa)
			h.key = types.NamespacedName{Namespace: nxa.Namespace, Name: nxa.Name}
			h.mustReconcile()
			assertNexusCondition(t, h.object(), nexusv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonInvalidSpec)
			if len(h.nx.Users()) != 0 {
				t.Error("a user was created for an invalid spec")
			}
		})
	}
}

func TestSecretToNexusAccess(t *testing.T) {
	r := &NexusReconciler{Config: testReconcilerConfig()}
	own := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: nexussecret.Name("tenant", "web"),
		Labels: nexussecret.Labels(testCluster, "tenant", "web")}}
	if got := r.secretToNexusAccess(context.Background(), own); len(got) != 1 || got[0].Name != "web" {
		t.Errorf("own Secret maps to %v", got)
	}
	squatter := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: nexussecret.Name("tenant", "web")}}
	if got := r.secretToNexusAccess(context.Background(), squatter); len(got) != 1 || got[0].Namespace != "tenant" {
		t.Errorf("Secret at the name maps to %v", got)
	}
	robot := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "robot-tenant.web"}}
	if got := r.secretToNexusAccess(context.Background(), robot); len(got) != 0 {
		t.Errorf("robot Secret maps to %v", got)
	}
	elsewhere := own.DeepCopy()
	elsewhere.Namespace = "other"
	if got := r.secretToNexusAccess(context.Background(), elsewhere); len(got) != 0 {
		t.Errorf("Secret outside the bridge namespace maps to %v", got)
	}
}

func TestNexusSameServiceAccount(t *testing.T) {
	a, b, c := newNexusAccess(), newNexusAccess(), newNexusAccess()
	b.Name = "web-v2"
	c.Name, c.Spec.ServiceAccountRef.Name = "other", "other-sa"
	h := newNexusHarness(t, nil, a, b, c)
	got := h.r.sameServiceAccount(context.Background(), a)
	if len(got) != 1 || got[0].Name != "web-v2" {
		t.Errorf("sameServiceAccount = %v", got)
	}
}

func labelsParse(s string) (labels.Selector, error) { return labels.Parse(s) }
