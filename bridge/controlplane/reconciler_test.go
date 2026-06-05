// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
)

// ----------------------------------------------------------------------------
// Test fixtures
// ----------------------------------------------------------------------------

const (
	testCluster     = "prod-eu-west"
	testIssuer      = "https://kubernetes.default.svc"
	testHarbor      = "https://harbor.example.com"
	testNS          = "harbor-bridge-system"
	testHAName      = "flux-access"
	testHANamespace = "harbor-bridge-system"
	testSANamespace = "flux-system"
	testSAName      = "source-controller"
)

// testScheme is shared across tests; registers HarborAccess + corev1.
var testScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(harborv1alpha1.AddToScheme(s))
	return s
}()

// newReconciler wires up a Reconciler with a fake k8s client (with status
// subresource support for HarborAccess) and a mock Harbor client.
func newReconciler(t *testing.T, mockHarbor harbor.Client, clock Clock, objects ...client.Object) *Reconciler {
	t.Helper()
	issuer, _ := url.Parse(testIssuer)
	harborURL, _ := url.Parse(testHarbor)
	cfg := &Config{
		ClusterName:    testCluster,
		Namespace:      testNS,
		OIDCIssuer:     issuer,
		HarborURL:      harborURL,
		HarborAdminDir: "/dev/null",
		LogLevel:       "debug",
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objects...).
		WithStatusSubresource(&harborv1alpha1.HarborAccess{}).
		Build()
	return &Reconciler{
		Client: c,
		Scheme: testScheme,
		Harbor: mockHarbor,
		Config: cfg,
		Clock:  clock,
	}
}

// newHarborAccess builds a HarborAccess CR with reasonable defaults; tweak the
// returned object before passing it to newReconciler.
func newHarborAccess() *harborv1alpha1.HarborAccess {
	return &harborv1alpha1.HarborAccess{
		ObjectMeta: metav1.ObjectMeta{
			Name:       testHAName,
			Namespace:  testHANamespace,
			Generation: 1,
			Finalizers: []string{FinalizerName}, // skip the add-finalizer step in most tests
		},
		Spec: harborv1alpha1.HarborAccessSpec{
			ServiceAccountRef: harborv1alpha1.ServiceAccountRef{
				Namespace: testSANamespace,
				Name:      testSAName,
			},
			TrustPolicy: harborv1alpha1.TrustPolicy{
				Issuer:   testIssuer,
				Audience: "harbor.example.com",
			},
			Permissions: []harborv1alpha1.ProjectPermission{
				{Project: "production", Action: "pull"},
			},
			TokenTTL: metav1.Duration{Duration: time.Hour},
		},
	}
}

func reqFor(ha *harborv1alpha1.HarborAccess) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: ha.Namespace, Name: ha.Name,
	}}
}

// fixedClock is a Clock returning a fixed instant. Tests can rewind by
// constructing a new fixedClock with an earlier time.
type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

// ----------------------------------------------------------------------------
// mockHarbor — in-memory harbor.Client for reconciler tests.
// ----------------------------------------------------------------------------

type mockHarbor struct {
	mu     sync.Mutex
	robots map[int64]*harbor.Robot
	nextID int64

	createCalls  []mockCreateCall
	deleteCalls  []int64
	updateCalls  []mockUpdateCall
	refreshCalls []int64
	listCalls    int

	// errOnGetByName, if non-nil, is returned from GetByName for the
	// matching name (use to simulate Harbor errors mid-reconcile).
	errOnGetByName map[string]error
	// errOnRefresh, if non-nil, is returned from RefreshSecret.
	errOnRefresh error
	// hideFromGetByName, if non-empty, is the set of robot names
	// GetByName must report as ErrRobotNotFound on the NEXT lookup
	// only. The flag clears after that one miss so the recovery
	// path's re-fetch observes the robot normally — mirroring the
	// real "Harbor list was briefly inconsistent" scenario the
	// reconciler's 409 recovery is designed for.
	hideFromGetByName map[string]bool
}

type mockCreateCall struct {
	Name, Description string
	Perms             []harbor.ProjectPermission
}

type mockUpdateCall struct {
	ID          int64
	Description string
	Perms       []harbor.ProjectPermission
}

func newMockHarbor() *mockHarbor {
	return &mockHarbor{
		robots: map[int64]*harbor.Robot{},
		nextID: 100,
	}
}

// preexisting adds a robot to the mock as if it already existed in Harbor
// before this bridge ever ran. Returns the assigned ID.
func (m *mockHarbor) preexisting(name, description string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID
	m.nextID++
	m.robots[id] = &harbor.Robot{
		ID:          id,
		Name:        name,
		Description: description,
	}
	return id
}

func (m *mockHarbor) Create(_ context.Context, name, description string, perms []harbor.ProjectPermission) (*harbor.Robot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls = append(m.createCalls, mockCreateCall{Name: name, Description: description, Perms: perms})
	// Mirror real Harbor: a name collision returns 409, surfaced as
	// ErrRobotAlreadyExists. The reconciler's 409 recovery path keys
	// off this exact sentinel.
	for _, r := range m.robots {
		if r.Name == name {
			return nil, harbor.ErrRobotAlreadyExists
		}
	}
	id := m.nextID
	m.nextID++
	r := &harbor.Robot{
		ID:          id,
		Name:        name,
		Description: description,
		Secret:      fmt.Sprintf("created-secret-%d", id),
	}
	m.robots[id] = r
	return &harbor.Robot{ID: r.ID, Name: r.Name, Description: r.Description, Secret: r.Secret}, nil
}

func (m *mockHarbor) Delete(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls = append(m.deleteCalls, id)
	delete(m.robots, id)
	return nil
}

func (m *mockHarbor) List(_ context.Context) ([]harbor.Robot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalls++
	out := make([]harbor.Robot, 0, len(m.robots))
	for _, r := range m.robots {
		out = append(out, *r)
	}
	return out, nil
}

func (m *mockHarbor) GetByID(_ context.Context, id int64) (*harbor.Robot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.robots[id]; ok {
		// Harbor never returns the secret on read paths — secrets are only
		// exposed in the Create and RefreshSec response payloads.
		return &harbor.Robot{ID: r.ID, Name: r.Name, Description: r.Description}, nil
	}
	return nil, harbor.ErrRobotNotFound
}

func (m *mockHarbor) GetByName(_ context.Context, name string) (*harbor.Robot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.errOnGetByName[name]; ok && err != nil {
		return nil, err
	}
	if m.hideFromGetByName[name] {
		// One-shot: clears after the first miss so the recovery-path
		// GetByName observes the robot normally.
		delete(m.hideFromGetByName, name)
		return nil, harbor.ErrRobotNotFound
	}
	for _, r := range m.robots {
		if r.Name == name {
			// Match real Harbor: no Secret on read paths.
			return &harbor.Robot{ID: r.ID, Name: r.Name, Description: r.Description}, nil
		}
	}
	return nil, harbor.ErrRobotNotFound
}

func (m *mockHarbor) RefreshSecret(_ context.Context, id int64) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refreshCalls = append(m.refreshCalls, id)
	if m.errOnRefresh != nil {
		return "", m.errOnRefresh
	}
	r, ok := m.robots[id]
	if !ok {
		return "", harbor.ErrRobotNotFound
	}
	r.Secret = fmt.Sprintf("refreshed-secret-%d-call-%d", id, len(m.refreshCalls))
	return r.Secret, nil
}

func (m *mockHarbor) UpdatePermissions(_ context.Context, id int64, description string, perms []harbor.ProjectPermission) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls = append(m.updateCalls, mockUpdateCall{ID: id, Description: description, Perms: perms})
	if r, ok := m.robots[id]; ok {
		r.Description = description
	}
	return nil
}

// ----------------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------------

func TestReconcile_HappyPath_CreatesRobotAndSecret(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	clock := fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}
	r := newReconciler(t, mh, clock, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Robot created exactly once.
	if len(mh.createCalls) != 1 {
		t.Fatalf("create calls: got %d, want 1", len(mh.createCalls))
	}
	expectedName := "bridge-prod-eu-west-flux-system-source-controller"
	if mh.createCalls[0].Name != expectedName {
		t.Errorf("created robot name = %q, want %q", mh.createCalls[0].Name, expectedName)
	}

	// Status: Ready, RobotProvisioned, TrustPolicyApplied all true.
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Robot == nil || got.Status.Robot.Name != expectedName {
		t.Errorf("status.robot.name = %v, want %q", got.Status.Robot, expectedName)
	}
	if got.Status.TrustPolicyEnforcedBy != harborv1alpha1.EnforcedByBridge {
		t.Errorf("trustPolicyEnforcedBy = %q, want %q", got.Status.TrustPolicyEnforcedBy, harborv1alpha1.EnforcedByBridge)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
	assertCondition(t, got, harborv1alpha1.ConditionRobotProvisioned, metav1.ConditionTrue, ReasonReconcileSucceeded)
	assertCondition(t, got, harborv1alpha1.ConditionTrustPolicyApplied, metav1.ConditionTrue, ReasonEnforcedByBridge)

	// Secret created in bridge namespace, contains username + password.
	secret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{
		Namespace: testNS, Name: SecretNamePrefix + testHANamespace + "-" + testHAName,
	}, secret); err != nil {
		t.Fatalf("password Secret missing: %v", err)
	}
	if string(secret.Data["username"]) != expectedName {
		t.Errorf("Secret.username = %q, want %q", secret.Data["username"], expectedName)
	}
	if len(secret.Data["password"]) == 0 {
		t.Errorf("Secret.password is empty")
	}
}

func TestReconcile_AddsFinalizerOnFirstReconcile(t *testing.T) {
	ha := newHarborAccess()
	ha.Finalizers = nil // no finalizer yet
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)

	res, err := r.Reconcile(context.Background(), reqFor(ha))
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter == 0 {
		t.Errorf("expected RequeueAfter>0 after adding finalizer; got %+v", res)
	}
	// No Harbor calls on this pass: the reconciler requeues itself to
	// pick up the real work on the next iteration.
	if len(mh.createCalls) != 0 {
		t.Errorf("Create called before finalizer was set: %+v", mh.createCalls)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if !containsFinalizer(got, FinalizerName) {
		t.Errorf("finalizer not added: %v", got.Finalizers)
	}
}

func TestReconcile_IssuerMismatch(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.TrustPolicy.Issuer = "https://different.example.com"
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}

	if len(mh.createCalls) != 0 {
		t.Errorf("Harbor.Create called despite issuer mismatch: %+v", mh.createCalls)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonIssuerMismatch)
}

func TestReconcile_AdoptionDiscipline_RefusesForeignDescription(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	// Robot with our name but no managed-by marker — someone else's robot.
	name := "bridge-prod-eu-west-flux-system-source-controller"
	mh.preexisting(name, "manually created by ops, do not delete")
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}

	if len(mh.createCalls) != 0 {
		t.Errorf("Create called on a name we don't own: %+v", mh.createCalls)
	}
	if len(mh.deleteCalls) != 0 {
		t.Errorf("Delete called on a foreign robot: %+v", mh.deleteCalls)
	}
	if len(mh.updateCalls) != 0 {
		t.Errorf("UpdatePermissions called on a foreign robot: %+v", mh.updateCalls)
	}
	if len(mh.refreshCalls) != 0 {
		t.Errorf("RefreshSecret called on a foreign robot: %+v", mh.refreshCalls)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRobotConflict)
}

func TestReconcile_DefenseInDepth_RejectsPrefixCollisionRobot(t *testing.T) {
	// Cluster "prod" reconciling, but the robot's name (which would prefix-
	// match cluster=prod by ADR-0009's known limitation) carries a
	// description marking it as cluster=prod-eu. The description-check must
	// catch this even though the name prefix would let OwnsRobot through.
	ha := newHarborAccess()
	// Override cluster to make the prefix-collision class concrete.
	mh := newMockHarbor()
	otherClusterDesc := RobotDescription("prod-eu", "harbor-bridge-system", "different-access")
	// This robot's name does start with "bridge-prod-eu-..." which OwnsRobot("prod", ...) reports as true.
	mh.preexisting("bridge-prod-eu-someone-else", otherClusterDesc)

	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	// Switch our cluster to "prod" for this test.
	r.Config.ClusterName = "prod"
	// And switch the SA fields so RobotName produces "bridge-prod-eu-someone-else"
	// — that's the prefix-collision setup.
	ha.Spec.ServiceAccountRef.Namespace = "eu"
	ha.Spec.ServiceAccountRef.Name = "someone-else"
	if err := r.Update(context.Background(), ha); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}

	// We must NOT touch the foreign robot.
	if len(mh.deleteCalls) != 0 || len(mh.updateCalls) != 0 || len(mh.refreshCalls) != 0 || len(mh.createCalls) != 0 {
		t.Errorf(
			"prefix-collision robot was modified: create=%d delete=%d update=%d refresh=%d",
			len(mh.createCalls), len(mh.deleteCalls), len(mh.updateCalls), len(mh.refreshCalls),
		)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRobotConflict)
}

func TestReconcile_UpdatesPermissionsOnGenerationChange(t *testing.T) {
	// Setup: reconcile once, then bump generation and reconcile again.
	ha := newHarborAccess()
	mh := newMockHarbor()
	clock := fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}
	r := newReconciler(t, mh, clock, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.updateCalls) != 0 {
		t.Errorf("UpdatePermissions called on first reconcile: %+v", mh.updateCalls)
	}

	// Now bump the spec's generation (simulating a permissions edit).
	current := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Permissions = append(current.Spec.Permissions, harborv1alpha1.ProjectPermission{
		Project: "shared", Action: "pull,push",
	})
	current.Generation = 2
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.updateCalls) != 1 {
		t.Errorf("UpdatePermissions calls after generation bump: got %d, want 1", len(mh.updateCalls))
	}
	if len(mh.refreshCalls) != 1 {
		t.Errorf("RefreshSecret should also fire on generation change; got %d", len(mh.refreshCalls))
	}
}

func TestReconcile_RotatesPasswordOnStaleAge(t *testing.T) {
	// First reconcile establishes LastRotated.
	ha := newHarborAccess()
	mh := newMockHarbor()
	t0 := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	clock := fixedClock{t0}
	r := newReconciler(t, mh, clock, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.refreshCalls) != 0 {
		t.Errorf("RefreshSecret called on initial create: %+v", mh.refreshCalls)
	}

	// Re-reconcile a few minutes later — no rotation expected.
	r.Clock = fixedClock{t0.Add(10 * time.Minute)}
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.refreshCalls) != 0 {
		t.Errorf("RefreshSecret fired too early: %+v", mh.refreshCalls)
	}

	// Skip past the rotation interval — rotation expected.
	r.Clock = fixedClock{t0.Add(PasswordRotationInterval + time.Minute)}
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.refreshCalls) != 1 {
		t.Errorf("RefreshSecret calls after stale age: got %d, want 1", len(mh.refreshCalls))
	}

	// Status.LastRotated should advance.
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Robot == nil || got.Status.Robot.LastRotated == nil {
		t.Fatal("LastRotated nil after rotation")
	}
	if !got.Status.Robot.LastRotated.Time.Equal(t0.Add(PasswordRotationInterval + time.Minute)) {
		t.Errorf("LastRotated = %v, want %v", got.Status.Robot.LastRotated.Time, t0.Add(PasswordRotationInterval+time.Minute))
	}
}

func TestReconcile_DeleteWithFinalizer_RemovesRobotAndSecret(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	clock := fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}
	r := newReconciler(t, mh, clock, ha)

	// Create first.
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.robots) != 1 {
		t.Fatalf("expected 1 robot after create, got %d", len(mh.robots))
	}

	// Mark the CR for deletion (DeletionTimestamp triggers the delete path
	// when a finalizer is present).
	current := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}

	if len(mh.deleteCalls) != 1 {
		t.Errorf("Harbor.Delete calls: got %d, want 1", len(mh.deleteCalls))
	}
	if len(mh.robots) != 0 {
		t.Errorf("robot not removed from Harbor: %+v", mh.robots)
	}
	// The Secret must be gone.
	secret := &corev1.Secret{}
	err := r.Get(context.Background(), client.ObjectKey{
		Namespace: testNS, Name: SecretNamePrefix + testHANamespace + "-" + testHAName,
	}, secret)
	if err == nil {
		t.Errorf("Secret should be deleted")
	} else if !apierrors.IsNotFound(err) {
		t.Errorf("unexpected error checking Secret: %v", err)
	}
	// The CR must be gone (finalizer removed → fake client garbage-collects it).
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, &harborv1alpha1.HarborAccess{}); !apierrors.IsNotFound(err) {
		t.Errorf("HarborAccess still exists after finalizer release: %v", err)
	}
}

func TestReconcile_HarborErrorTriggersRetry(t *testing.T) {
	// A 5xx (or any non-NotFound error) from Harbor must:
	//   - set Ready=False with reason HarborError
	//   - return a non-nil error from Reconcile so controller-runtime
	//     requeues with exponential backoff (otherwise transient failures
	//     leave the CR Ready=False until the controller's resync, default 10h)
	ha := newHarborAccess()
	mh := newMockHarbor()
	mh.errOnGetByName = map[string]error{
		"bridge-prod-eu-west-flux-system-source-controller": fmt.Errorf("simulated harbor 503"),
	}
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)

	_, err := r.Reconcile(context.Background(), reqFor(ha))
	if err == nil {
		t.Fatal("expected non-nil error so controller-runtime retries with backoff")
	}
	if !strings.Contains(err.Error(), "simulated harbor 503") {
		t.Errorf("returned error did not wrap the underlying cause: %v", err)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonHarborError)

	// No Harbor-modifying calls should have happened.
	if len(mh.createCalls)+len(mh.updateCalls)+len(mh.deleteCalls)+len(mh.refreshCalls) != 0 {
		t.Errorf("unexpected Harbor writes during error path: create=%d update=%d delete=%d refresh=%d",
			len(mh.createCalls), len(mh.updateCalls), len(mh.deleteCalls), len(mh.refreshCalls))
	}
}

func TestReconcile_RebuildsMissingSecret(t *testing.T) {
	// Scenario: bridge created robot+Secret successfully on a previous run;
	// then an operator (or a misbehaving controller) deleted the Secret.
	// On the next reconcile the reconciler must detect the missing Secret
	// and force a rotation (RefreshSecret + writeRobotSecret) — otherwise
	// Status would falsely report Ready while the data plane has no creds.
	ha := newHarborAccess()
	mh := newMockHarbor()
	t0 := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	r := newReconciler(t, mh, fixedClock{t0}, ha)

	// First reconcile: creates robot, writes Secret.
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.createCalls) != 1 {
		t.Fatalf("setup: expected 1 create, got %d", len(mh.createCalls))
	}

	// Operator deletes the Secret out of band.
	secretName := SecretNamePrefix + testHANamespace + "-" + testHAName
	if err := r.Delete(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: secretName},
	}); err != nil {
		t.Fatalf("setup: deleting Secret: %v", err)
	}

	// Tick forward a short time (well below rotation interval, so the
	// trigger must be the missing-Secret check, not the staleness check).
	r.Clock = fixedClock{t0.Add(5 * time.Minute)}

	// Second reconcile: should detect missing Secret, refresh, and write.
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.refreshCalls) != 1 {
		t.Errorf("expected exactly one RefreshSecret call to rebuild Secret; got %d", len(mh.refreshCalls))
	}
	// Secret must exist again, with a non-empty password.
	secret := &corev1.Secret{}
	if err := r.Get(context.Background(),
		client.ObjectKey{Namespace: testNS, Name: secretName},
		secret); err != nil {
		t.Fatalf("Secret was not rebuilt: %v", err)
	}
	if len(secret.Data["password"]) == 0 {
		t.Errorf("rebuilt Secret has empty password")
	}
}

func TestReconcile_409OnCreate_RecoversByAdoptingExistingRobot(t *testing.T) {
	// Scenario reproduces what happened during the first manual e2e:
	// Harbor returned 409 from POST /robots ("already exists") while our
	// GetByName was reporting NotFound. The robot was real (left over
	// from a previous reconcile's partial success); the reconciler must
	// adopt it, rotate its password, write the Secret, and mark Ready —
	// not loop on 409 indefinitely.
	ha := newHarborAccess()
	mh := newMockHarbor()
	t0 := time.Date(2026, 5, 30, 22, 28, 0, 0, time.UTC)
	r := newReconciler(t, mh, fixedClock{t0}, ha)

	// Pre-existing robot in Harbor with our cluster's description tag,
	// matching what a stranded prior reconcile would have created.
	robotName := "bridge-" + testCluster + "-" + testSANamespace + "-" + testSAName
	desc := RobotDescription(testCluster, testHANamespace, testHAName)
	mh.preexisting(robotName, desc)

	// Make GetByName miss it so we hit the create branch and Harbor
	// returns 409. mockHarbor.Create returns ErrRobotAlreadyExists when
	// the name is already taken — same shape as the real client.
	mh.hideFromGetByName = map[string]bool{robotName: true}

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(mh.createCalls) != 1 {
		t.Errorf("expected exactly one Create attempt before recovery, got %d", len(mh.createCalls))
	}
	if len(mh.refreshCalls) != 1 {
		t.Errorf("expected RefreshSecret on the recovered robot, got %d call(s)", len(mh.refreshCalls))
	}

	// Secret must now exist with the rotated password.
	got := &corev1.Secret{}
	secretName := SecretNamePrefix + testHANamespace + "-" + testHAName
	if err := r.Get(context.Background(),
		client.ObjectKey{Namespace: testNS, Name: secretName},
		got); err != nil {
		t.Fatalf("recovered Secret missing: %v", err)
	}
	if len(got.Data["password"]) == 0 {
		t.Errorf("recovered Secret has empty password")
	}
	if string(got.Data["username"]) != robotName {
		t.Errorf("recovered Secret username = %q, want %q", got.Data["username"], robotName)
	}

	// CR status must be Ready=True after recovery.
	final := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(),
		client.ObjectKey{Namespace: ha.Namespace, Name: ha.Name},
		final); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(final.Status.Conditions, harborv1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("Ready condition after recovery = %#v, want True", cond)
	}
}

func TestReconcile_409OnCreate_RefusesToAdoptForeignRobot(t *testing.T) {
	// Defense-in-depth (ADR-0009): the recovery path must NOT adopt a
	// robot whose description does not mark it as belonging to this
	// cluster, even when names collide. This protects against the
	// hyphen-prefix class of operator-misconfigurations.
	ha := newHarborAccess()
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)

	robotName := "bridge-" + testCluster + "-" + testSANamespace + "-" + testSAName
	foreignDesc := RobotDescription("some-other-cluster", testHANamespace, testHAName)
	mh.preexisting(robotName, foreignDesc)
	mh.hideFromGetByName = map[string]bool{robotName: true}

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(mh.refreshCalls) != 0 {
		t.Errorf("recovery rotated the secret of a robot we don't own; got %d RefreshSecret call(s)", len(mh.refreshCalls))
	}
	final := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(),
		client.ObjectKey{Namespace: ha.Namespace, Name: ha.Name},
		final); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(final.Status.Conditions, harborv1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonRobotConflict {
		t.Errorf("Ready condition = %#v, want False/RobotConflict", cond)
	}
}

func TestReconcile_DeleteSkipsForeignRobot(t *testing.T) {
	// Setup: a robot with our name exists but its description marks it as
	// cluster=other. The reconcile-delete path must NOT delete it but must
	// still drop the finalizer so the CR cleanup proceeds.
	ha := newHarborAccess()
	now := metav1.NewTime(time.Now())
	ha.DeletionTimestamp = &now

	mh := newMockHarbor()
	robotName := "bridge-prod-eu-west-flux-system-source-controller"
	foreignDesc := RobotDescription("prod-us-east", "harbor-bridge-system", "different-cr")
	id := mh.preexisting(robotName, foreignDesc)

	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}

	if len(mh.deleteCalls) != 0 {
		t.Errorf("foreign robot deleted! id=%d, deleteCalls=%v", id, mh.deleteCalls)
	}
	if _, ok := mh.robots[id]; !ok {
		t.Errorf("foreign robot disappeared from mock state")
	}
}

// ----------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------

func containsFinalizer(obj *harborv1alpha1.HarborAccess, fin string) bool {
	for _, f := range obj.Finalizers {
		if f == fin {
			return true
		}
	}
	return false
}

func assertCondition(t *testing.T, obj *harborv1alpha1.HarborAccess, typ string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	c := meta.FindStatusCondition(obj.Status.Conditions, typ)
	if c == nil {
		t.Errorf("condition %q missing", typ)
		return
	}
	if c.Status != status {
		t.Errorf("condition %q status = %q, want %q (reason=%q msg=%q)", typ, c.Status, status, c.Reason, c.Message)
	}
	if c.Reason != reason {
		t.Errorf("condition %q reason = %q, want %q (msg=%q)", typ, c.Reason, reason, c.Message)
	}
}
