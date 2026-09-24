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
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
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
// mockHarbor — in-memory harbor.Client for reconciler tests. It mirrors the
// real Harbor behaviours the reconciler depends on (verified against
// goharbor/harbor src/server/v2.0/handler/robot.go and
// src/controller/robot/controller.go): names stored without the "robot$"
// prefix but reported with it, no secrets on read paths, 409 on a duplicate
// name, and PUT rejected unless the on-wire name is echoed verbatim.
// ----------------------------------------------------------------------------

const mockRobotPrefix = "robot$"

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
	// errOnUpdate, if non-nil, is returned from Update.
	errOnUpdate error
	// errOnList, if non-nil, is returned from List.
	errOnList error
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
	WireName    string
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
func (m *mockHarbor) preexisting(name, description string, perms ...harbor.ProjectPermission) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID
	m.nextID++
	m.robots[id] = &harbor.Robot{
		ID:          id,
		Name:        name,
		WireName:    mockRobotPrefix + name,
		Description: description,
		ExpiresAt:   -1,
		Permissions: perms,
	}
	return id
}

// readView is what Harbor returns on read paths: everything but the secret.
func readView(r *harbor.Robot) harbor.Robot {
	out := *r
	out.Secret = ""
	out.Permissions = append([]harbor.ProjectPermission(nil), r.Permissions...)
	return out
}

func (m *mockHarbor) Create(_ context.Context, name, description string, perms []harbor.ProjectPermission) (*harbor.Robot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls = append(m.createCalls, mockCreateCall{Name: name, Description: description, Perms: perms})
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
		WireName:    mockRobotPrefix + name,
		Description: description,
		ExpiresAt:   -1,
		Permissions: append([]harbor.ProjectPermission(nil), perms...),
		Secret:      fmt.Sprintf("created-secret-%d", id),
	}
	m.robots[id] = r
	out := *r
	return &out, nil
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
	if m.errOnList != nil {
		return nil, m.errOnList
	}
	out := make([]harbor.Robot, 0, len(m.robots))
	for _, r := range m.robots {
		out = append(out, readView(r))
	}
	return out, nil
}

func (m *mockHarbor) GetByName(_ context.Context, name string) (*harbor.Robot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.errOnGetByName[name]; ok && err != nil {
		return nil, err
	}
	if m.hideFromGetByName[name] {
		delete(m.hideFromGetByName, name)
		return nil, harbor.ErrRobotNotFound
	}
	for _, r := range m.robots {
		if r.Name == name {
			v := readView(r)
			return &v, nil
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

func (m *mockHarbor) Update(_ context.Context, current *harbor.Robot, description string, perms []harbor.ProjectPermission) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCalls = append(m.updateCalls, mockUpdateCall{ID: current.ID, WireName: current.WireName, Description: description, Perms: perms})
	if m.errOnUpdate != nil {
		return m.errOnUpdate
	}
	r, ok := m.robots[current.ID]
	if !ok {
		return fmt.Errorf("update robot %d: 404", current.ID)
	}
	if current.WireName != r.WireName {
		return fmt.Errorf("update robot %d: 400 cannot update the level or name of robot", current.ID)
	}
	r.Description = description
	r.Permissions = append([]harbor.ProjectPermission(nil), perms...)
	r.Disabled = current.Disabled
	r.ExpiresAt = -1
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

	res, err := r.Reconcile(context.Background(), reqFor(ha))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Robot created exactly once.
	if len(mh.createCalls) != 1 {
		t.Fatalf("create calls: got %d, want 1", len(mh.createCalls))
	}
	expectedName := "bridge-prod-eu-west.flux-system.source-controller"
	if mh.createCalls[0].Name != expectedName {
		t.Errorf("created robot name = %q, want %q", mh.createCalls[0].Name, expectedName)
	}

	// Status: Ready, RobotProvisioned, TrustPolicyApplied all true.
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Robot == nil || got.Status.Robot.Name != mockRobotPrefix+expectedName {
		t.Errorf("status.robot.name = %v, want the on-wire name %q", got.Status.Robot, mockRobotPrefix+expectedName)
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
		Namespace: testNS, Name: robotsecret.Name(testHANamespace, testHAName),
	}, secret); err != nil {
		t.Fatalf("password Secret missing: %v", err)
	}
	// The username is the ON-WIRE name: it is what registry clients must
	// present as Basic Auth user.
	if string(secret.Data["username"]) != mockRobotPrefix+expectedName {
		t.Errorf("Secret.username = %q, want %q", secret.Data["username"], mockRobotPrefix+expectedName)
	}
	if len(secret.Data["password"]) == 0 {
		t.Errorf("Secret.password is empty")
	}
	if id, ok := robotsecret.RobotID(secret); !ok || id != mh.createCalls0ID() {
		t.Errorf("robot-id annotation = %d %v", id, ok)
	}
	nb, ok := robotsecret.RotationNotBefore(secret)
	if !ok || !nb.Equal(clock.t.Add(PasswordRotationInterval)) {
		t.Errorf("rotation-not-before = %s %v, want %s", nb, ok, clock.t.Add(PasswordRotationInterval))
	}
	if ns, name, ok := robotsecret.Owner(secret); !ok || ns != testHANamespace || name != testHAName {
		t.Errorf("Secret owner labels = %q/%q %v", ns, name, ok)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > ResyncInterval {
		t.Errorf("RequeueAfter = %s, want (0, %s]", res.RequeueAfter, ResyncInterval)
	}
}

func TestReconcile_AddsFinalizerAndProvisionsInOnePass(t *testing.T) {
	ha := newHarborAccess()
	ha.Finalizers = nil // no finalizer yet
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if !containsFinalizer(got, FinalizerName) {
		t.Errorf("finalizer not added: %v", got.Finalizers)
	}
	// The finalizer is persisted BEFORE the robot is created, so a robot
	// can never exist without the finalizer that deletes it.
	if len(mh.createCalls) != 1 {
		t.Errorf("create calls = %d, want 1", len(mh.createCalls))
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
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
	name := "bridge-prod-eu-west.flux-system.source-controller"
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
		t.Errorf("Update called on a foreign robot: %+v", mh.updateCalls)
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

// AUDIT.md F2 (secret-name collision): defense-in-depth. Since ADR-0018 the
// per-CR Secret name "robot-<haNs>.<haName>" is dot-joined and injective, so
// distinct CRs no longer collapse onto one Secret in normal operation. This
// test forces the foreign-owner state directly to prove that, if that ever
// regressed, the reconciler refuses to overwrite rather than cross-wire two
// workloads' credentials.
func TestReconcile_SecretNameCollision_RefusesToOverwrite(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	// A bridge-managed Secret already occupies this CR's Secret name but is
	// stamped for a DIFFERENT HarborAccess.
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS,
			Name:      robotsecret.Name(testHANamespace, testHAName),
			Labels:    robotsecret.Labels(testCluster, "other-ns", "other-ha"),
		},
		Data: map[string][]byte{"username": []byte("robot$other"), "password": []byte("foreign-pw")},
	}
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha, foreign)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.createCalls) != 0 {
		t.Errorf("Create called despite Secret-name collision: %+v", mh.createCalls)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRobotConflict)
	// The foreign Secret's credentials must be untouched.
	s := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: foreign.Name}, s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["password"]) != "foreign-pw" {
		t.Errorf("foreign Secret password overwritten: %q", s.Data["password"])
	}
}

// AUDIT.md F2 (robot-name collision): the robot name
// "bridge-<cluster>-<saNs>-<saName>" is dash-joined and ambiguous, so two
// distinct SA refs can collapse onto one robot. When the existing robot's
// description names a different HarborAccess, the reconciler must refuse to
// adopt it — no permission overwrite, no password rotation that would break
// the rightful owner's stored Secret.
func TestReconcile_RobotNameCollision_RefusesForeignHarborAccess(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	name := "bridge-prod-eu-west.flux-system.source-controller"
	// In our prefix AND tagged for our cluster, but owned by another CR.
	mh.preexisting(name, RobotDescription(testCluster, "other-ns", "other-ha"))
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.updateCalls) != 0 || len(mh.refreshCalls) != 0 {
		t.Errorf("adopted a robot owned by another HarborAccess: updates=%+v refresh=%+v",
			mh.updateCalls, mh.refreshCalls)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRobotConflict)
}

func TestReconcile_DefenseInDepth_RejectsForeignDescriptionRobot(t *testing.T) {
	// ADR-0018's dot delimiter removes the ADR-0009 hyphen-prefix false
	// positive, but the description-tag check (RobotBelongsToCluster) is still
	// load-bearing defense-in-depth: if a robot exists with OUR exact computed
	// name but a description marking it as another cluster's, we must refuse to
	// touch it. Here cluster "prod" with SA eu/someone-else computes
	// "bridge-prod.eu.someone-else"; a robot of that exact name exists but is
	// tagged cluster=prod-eu.
	ha := newHarborAccess()
	mh := newMockHarbor()
	otherClusterDesc := RobotDescription("prod-eu", "harbor-bridge-system", "different-access")
	mh.preexisting("bridge-prod.eu.someone-else", otherClusterDesc)

	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	// Switch our cluster to "prod" and SA fields so RobotName produces the
	// exact name of the foreign-tagged robot above.
	r.Config.ClusterName = "prod"
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

// TestReconcile_PermissionChange_UpdatesInPlaceWithoutRotation pins two
// fixes. (1) The update actually reaches Harbor with the on-wire name
// (audit C1: Harbor 400s any other name, so revocations never applied).
// (2) A spec change keeps the password: rotating it would invalidate the
// credentials every kubelet still has cached and fail pulls for up to the
// cache duration.
func TestReconcile_PermissionChange_UpdatesInPlaceWithoutRotation(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	clock := fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}
	r := newReconciler(t, mh, clock, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.updateCalls) != 0 {
		t.Errorf("Update called on first reconcile: %+v", mh.updateCalls)
	}
	pwBefore := secretPassword(t, r)

	current := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Permissions = []harborv1alpha1.ProjectPermission{{Project: "shared", Action: "pull,push"}}
	current.Generation = 2
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.updateCalls) != 1 {
		t.Fatalf("Update calls after spec change: got %d, want 1", len(mh.updateCalls))
	}
	if mh.updateCalls[0].WireName != mockRobotPrefix+"bridge-prod-eu-west.flux-system.source-controller" {
		t.Errorf("Update sent name %q, want the on-wire name", mh.updateCalls[0].WireName)
	}
	stored := mh.robots[mh.createCalls0ID()]
	if !harbor.PermissionsMatch(stored, []harbor.ProjectPermission{{Project: "shared", Action: "pull,push"}}) {
		t.Errorf("Harbor robot permissions = %+v, want only shared:pull,push (production:pull revoked)", stored.Permissions)
	}
	if len(mh.refreshCalls) != 0 {
		t.Errorf("RefreshSecret fired on a spec change (%d calls); kubelet-cached passwords would break", len(mh.refreshCalls))
	}
	if got := secretPassword(t, r); got != pwBefore {
		t.Errorf("password changed on a spec change")
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ObservedGeneration != 2 {
		t.Errorf("observedGeneration = %d, want 2", got.Status.ObservedGeneration)
	}
}

// TestReconcile_FailedUpdate_NeverReportsReadyEarly is the regression test
// for the silent half of audit C1: the old reconciler advanced
// status.observedGeneration on the ERROR path, so the retry after a failed
// permission update saw "generation unchanged", skipped the update, and
// reported Ready=True with the old grants still live in Harbor.
func TestReconcile_FailedUpdate_NeverReportsReadyEarly(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	current := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Permissions = []harborv1alpha1.ProjectPermission{{Project: "other", Action: "pull"}}
	current.Generation = 2
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}

	mh.errOnUpdate = fmt.Errorf("simulated harbor 500")
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), reqFor(ha)); err == nil {
			t.Fatalf("pass %d: expected the update error to be returned for retry", i)
		}
		got := &harborv1alpha1.HarborAccess{}
		if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
			t.Fatal(err)
		}
		assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonHarborError)
		if got.Status.ObservedGeneration == 2 {
			t.Fatalf("pass %d: observedGeneration advanced to 2 while the update is failing", i)
		}
	}

	mh.errOnUpdate = nil
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if !harbor.PermissionsMatch(mh.robots[mh.createCalls0ID()], []harbor.ProjectPermission{{Project: "other", Action: "pull"}}) {
		t.Errorf("permissions not applied after Harbor recovered: %+v", mh.robots[mh.createCalls0ID()].Permissions)
	}
}

// TestReconcile_RepairsDriftMadeInHarbor: the reconcile is level-triggered,
// so a grant widened in the Harbor UI is reverted on the next pass even
// though the CR never changed.
func TestReconcile_RepairsDriftMadeInHarbor(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	id := mh.createCalls0ID()
	mh.robots[id].Permissions = []harbor.ProjectPermission{{Project: "production", Action: "pull,push"}, {Project: "secret", Action: "pull"}}

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if !harbor.PermissionsMatch(mh.robots[id], []harbor.ProjectPermission{{Project: "production", Action: "pull"}}) {
		t.Errorf("drift not repaired: %+v", mh.robots[id].Permissions)
	}
	if len(mh.refreshCalls) != 0 {
		t.Errorf("drift repair rotated the password")
	}
}

// TestReconcile_DisabledRobot_ReportsNotReadyAndStaysDisabled: an admin's
// disable in Harbor is respected (never silently re-enabled) and surfaced.
func TestReconcile_DisabledRobot_ReportsNotReadyAndStaysDisabled(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	id := mh.createCalls0ID()
	mh.robots[id].Disabled = true
	mh.robots[id].Permissions = nil // drift too, so an Update is sent while disabled

	res, err := r.Reconcile(context.Background(), reqFor(ha))
	if err != nil {
		t.Fatal(err)
	}
	if len(mh.updateCalls) != 1 {
		t.Fatalf("Update calls = %d, want 1 (drift repair)", len(mh.updateCalls))
	}
	if !mh.robots[id].Disabled {
		t.Error("the reconciler re-enabled a robot an administrator disabled")
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRobotDisabled)
	if res.RequeueAfter == 0 {
		t.Error("a disabled robot must be re-checked periodically (no CR event announces re-enabling)")
	}
}

// TestReconcile_ServiceAccountChange_RevokesOldRobot is the regression test
// for audit H2: pointing a HarborAccess at a different ServiceAccount must
// revoke the previous identity's robot, whose password nodes may still
// hold. Previously the old robot survived with a valid password forever.
func TestReconcile_ServiceAccountChange_RevokesOldRobot(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	oldID := mh.createCalls0ID()
	// An unrelated HarborAccess's robot must survive the cleanup.
	otherID := mh.preexisting("bridge-prod-eu-west.team.other", RobotDescription(testCluster, "team", "other"))

	current := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	current.Spec.ServiceAccountRef.Name = "renamed-controller"
	current.Generation = 2
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}

	if _, ok := mh.robots[oldID]; ok {
		t.Error("old robot survived the serviceAccountRef change")
	}
	if _, ok := mh.robots[otherID]; !ok {
		t.Error("stale-robot cleanup deleted another HarborAccess's robot")
	}
	newName := "bridge-prod-eu-west.flux-system.renamed-controller"
	var newRobot *harbor.Robot
	for _, rb := range mh.robots {
		if rb.Name == newName {
			newRobot = rb
		}
	}
	if newRobot == nil {
		t.Fatalf("new robot %q not created", newName)
	}
	s := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: robotsecret.Name(testHANamespace, testHAName)}, s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["username"]) != newRobot.WireName || string(s.Data["password"]) != newRobot.Secret {
		t.Errorf("Secret not switched to the new robot: user=%q", s.Data["username"])
	}
}

// TestReconcile_RobotRecreatedOutOfBand_Rotates: a robot deleted and
// re-created in Harbor under the same name has a new ID and a password the
// Secret does not hold; the reconciler must notice and rotate.
func TestReconcile_RobotRecreatedOutOfBand_Rotates(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	oldID := mh.createCalls0ID()
	name := mh.robots[oldID].Name
	delete(mh.robots, oldID)
	newID := mh.preexisting(name, RobotDescription(testCluster, testHANamespace, testHAName),
		harbor.ProjectPermission{Project: "production", Action: "pull"})

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.refreshCalls) != 1 || mh.refreshCalls[0] != newID {
		t.Fatalf("refresh calls = %v, want one for the re-created robot %d", mh.refreshCalls, newID)
	}
	s := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: robotsecret.Name(testHANamespace, testHAName)}, s); err != nil {
		t.Fatal(err)
	}
	if id, _ := robotsecret.RobotID(s); id != newID {
		t.Errorf("robot-id annotation = %d, want %d", id, newID)
	}
}

// TestReconcile_BackfillsRotationPromiseWithoutRotating covers upgrades: a
// Secret written by an older bridge lacks the annotations. They are
// backfilled from status (no rotation, so cached credentials stay valid).
func TestReconcile_BackfillsRotationPromiseWithoutRotating(t *testing.T) {
	t0 := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ha := newHarborAccess()
	last := metav1.NewTime(t0.Add(-2 * time.Hour))
	ha.Status.Robot = &harborv1alpha1.RobotRef{ID: 100, LastRotated: &last}
	ha.Status.ObservedGeneration = ha.Generation
	mh := newMockHarbor()
	id := mh.preexisting("bridge-prod-eu-west.flux-system.source-controller",
		RobotDescription(testCluster, testHANamespace, testHAName),
		harbor.ProjectPermission{Project: "production", Action: "pull"})
	old := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: robotsecret.Name(testHANamespace, testHAName),
			Labels: robotsecret.Labels(testCluster, testHANamespace, testHAName)},
		Data: map[string][]byte{"username": []byte(mockRobotPrefix + "bridge-prod-eu-west.flux-system.source-controller"), "password": []byte("old-pw")},
	}
	r := newReconciler(t, mh, fixedClock{t0}, ha, old)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.refreshCalls) != 0 {
		t.Fatalf("backfill rotated the password")
	}
	s := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: old.Name}, s); err != nil {
		t.Fatal(err)
	}
	if string(s.Data["password"]) != "old-pw" {
		t.Error("password changed during backfill")
	}
	if got, _ := robotsecret.RobotID(s); got != id {
		t.Errorf("robot-id not backfilled: %d", got)
	}
	nb, ok := robotsecret.RotationNotBefore(s)
	if !ok || !nb.Equal(last.Add(PasswordRotationInterval)) {
		t.Errorf("rotation-not-before = %s %v, want %s", nb, ok, last.Add(PasswordRotationInterval))
	}
}

func TestReconcile_RejectsOverlongName(t *testing.T) {
	ha := newHarborAccess()
	ha.Name = strings.Repeat("n", HarborAccessNameMaxLen+1)
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.createCalls) != 0 {
		t.Error("robot created for an HA whose Secret can never be labelled")
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonInvalidSpec)
}

// TestReconcile_RotatesOnlyAfterThePromisedInstant pins the rotation
// schedule to the Secret's rotation-not-before promise (ADR-0023): the data
// plane never lets kubelet cache past that instant, so rotating earlier
// would break cached credentials.
func TestReconcile_RotatesOnlyAfterThePromisedInstant(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	t0 := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	r := newReconciler(t, mh, fixedClock{t0}, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}

	// Right at the promise, and inside the safety margin: no rotation.
	for _, at := range []time.Time{t0.Add(10 * time.Minute), t0.Add(PasswordRotationInterval), t0.Add(PasswordRotationInterval + RotationSafetyMargin - time.Second)} {
		r.Clock = fixedClock{at}
		res, err := r.Reconcile(context.Background(), reqFor(ha))
		if err != nil {
			t.Fatal(err)
		}
		if len(mh.refreshCalls) != 0 {
			t.Fatalf("RefreshSecret fired at %s, before promise+margin", at)
		}
		want := t0.Add(PasswordRotationInterval + RotationSafetyMargin).Sub(at)
		if want < time.Second {
			want = time.Second
		}
		if res.RequeueAfter > want {
			t.Errorf("at %s RequeueAfter = %s, want <= %s (next pass must not miss the rotation)", at, res.RequeueAfter, want)
		}
	}

	rotateAt := t0.Add(PasswordRotationInterval + RotationSafetyMargin)
	r.Clock = fixedClock{rotateAt}
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.refreshCalls) != 1 {
		t.Fatalf("RefreshSecret calls after promise+margin: got %d, want 1", len(mh.refreshCalls))
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Robot == nil || got.Status.Robot.LastRotated == nil || !got.Status.Robot.LastRotated.Time.Equal(rotateAt) {
		t.Errorf("LastRotated = %v, want %v", got.Status.Robot, rotateAt)
	}
	s := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: robotsecret.Name(testHANamespace, testHAName)}, s); err != nil {
		t.Fatal(err)
	}
	if nb, _ := robotsecret.RotationNotBefore(s); !nb.Equal(rotateAt.Add(PasswordRotationInterval)) {
		t.Errorf("new promise = %s, want %s", nb, rotateAt.Add(PasswordRotationInterval))
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
		Namespace: testNS, Name: robotsecret.Name(testHANamespace, testHAName),
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
		"bridge-prod-eu-west.flux-system.source-controller": fmt.Errorf("simulated harbor 503"),
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
	secretName := robotsecret.Name(testHANamespace, testHAName)
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
	robotName := "bridge-" + testCluster + "." + testSANamespace + "." + testSAName
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
	secretName := robotsecret.Name(testHANamespace, testHAName)
	if err := r.Get(context.Background(),
		client.ObjectKey{Namespace: testNS, Name: secretName},
		got); err != nil {
		t.Fatalf("recovered Secret missing: %v", err)
	}
	if len(got.Data["password"]) == 0 {
		t.Errorf("recovered Secret has empty password")
	}
	if string(got.Data["username"]) != mockRobotPrefix+robotName {
		t.Errorf("recovered Secret username = %q, want %q", got.Data["username"], mockRobotPrefix+robotName)
	}
	// The adopted robot is converged to the CR's permissions (audit M4):
	// recovery used to mark Ready with whatever grants the robot had.
	if len(mh.updateCalls) != 1 {
		t.Errorf("adopted robot's permissions not converged: %d Update calls", len(mh.updateCalls))
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
	// cluster, even when a robot of our exact computed name already exists.
	ha := newHarborAccess()
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)

	robotName := "bridge-" + testCluster + "." + testSANamespace + "." + testSAName
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
	robotName := "bridge-prod-eu-west.flux-system.source-controller"
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

// createCalls0ID returns the ID of the robot created by the first Create.
func (m *mockHarbor) createCalls0ID() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.robots {
		if len(m.createCalls) > 0 && r.Name == m.createCalls[0].Name {
			return id
		}
	}
	return -1
}

func secretPassword(t *testing.T, r *Reconciler) string {
	t.Helper()
	s := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: testNS, Name: robotsecret.Name(testHANamespace, testHAName)}, s); err != nil {
		t.Fatal(err)
	}
	return string(s.Data["password"])
}

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

// TestReconcile_Delete_RevokesEveryRobotOfTheHarborAccess is the finalizer
// cleanup fix: deletion used to revoke only the robot the CURRENT spec maps
// to, so a robot from an earlier serviceAccountRef or a pre-ADR-0018
// dash-named robot survived the CR with a valid password.
func TestReconcile_Delete_RevokesEveryRobotOfTheHarborAccess(t *testing.T) {
	ha := newHarborAccess()
	now := metav1.NewTime(time.Now())
	ha.DeletionTimestamp = &now
	mh := newMockHarbor()
	desc := RobotDescription(testCluster, testHANamespace, testHAName)
	current := mh.preexisting("bridge-prod-eu-west.flux-system.source-controller", desc)
	previous := mh.preexisting("bridge-prod-eu-west.flux-system.old-sa", desc)
	legacy := mh.preexisting("bridge-prod-eu-west-flux-system-source-controller", desc)
	sibling := mh.preexisting("bridge-prod-eu-west.team.x", RobotDescription(testCluster, "team", "x"))
	foreign := mh.preexisting("bridge-prod-eu-west.flux-system.y", RobotDescription("other-cluster", testHANamespace, testHAName))
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNS, Name: robotsecret.Name(testHANamespace, testHAName),
		Labels: robotsecret.Labels(testCluster, testHANamespace, testHAName)}}
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha, secret)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{current, previous, legacy} {
		if _, ok := mh.robots[id]; ok {
			t.Errorf("robot %d of the deleted HarborAccess survived", id)
		}
	}
	for _, id := range []int64{sibling, foreign} {
		if _, ok := mh.robots[id]; !ok {
			t.Errorf("robot %d that does not belong to this HarborAccess was deleted", id)
		}
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("robot Secret survived: %v", err)
	}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, &harborv1alpha1.HarborAccess{}); !apierrors.IsNotFound(err) {
		t.Errorf("finalizer not released: %v", err)
	}
}

// TestReconcile_Delete_BlocksAndExplainsWhenHarborIsDown: the finalizer is
// the revocation, so it must hold while Harbor is unreachable — but the
// user must see why the object hangs and how to force it.
func TestReconcile_Delete_BlocksAndExplainsWhenHarborIsDown(t *testing.T) {
	ha := newHarborAccess()
	now := metav1.NewTime(time.Now())
	ha.DeletionTimestamp = &now
	mh := newMockHarbor()
	mh.errOnList = fmt.Errorf("dial tcp: connection refused")
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)

	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err == nil {
		t.Fatal("expected an error so the deletion is retried with backoff")
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatalf("HarborAccess vanished although its robot could not be revoked: %v", err)
	}
	if !containsFinalizer(got, FinalizerName) {
		t.Error("finalizer released while Harbor was unreachable")
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonDeletionBlocked)
	c := meta.FindStatusCondition(got.Status.Conditions, harborv1alpha1.ConditionReady)
	if c == nil || !strings.Contains(c.Message, FinalizerName) {
		t.Errorf("condition message does not tell the user how to force deletion: %+v", c)
	}
}

// TestReconcile_Delete_KeepsForeignStampedSecret: the collision backstop
// also applies on deletion — never delete another HarborAccess's Secret.
func TestReconcile_Delete_KeepsForeignStampedSecret(t *testing.T) {
	ha := newHarborAccess()
	now := metav1.NewTime(time.Now())
	ha.DeletionTimestamp = &now
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNS, Name: robotsecret.Name(testHANamespace, testHAName),
		Labels: robotsecret.Labels(testCluster, "other-ns", "other-ha")}}
	r := newReconciler(t, newMockHarbor(), fixedClock{time.Now()}, ha, foreign)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(foreign), &corev1.Secret{}); err != nil {
		t.Errorf("deleted a Secret stamped for another HarborAccess: %v", err)
	}
}

func TestSecretToHarborAccess(t *testing.T) {
	r := newReconciler(t, newMockHarbor(), fixedClock{time.Now()})
	own := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "robot-a.b",
		Labels: robotsecret.Labels(testCluster, "a", "b")}}
	if got := r.secretToHarborAccess(context.Background(), own); len(got) != 1 || got[0].Namespace != "a" || got[0].Name != "b" {
		t.Errorf("own Secret mapped to %v", got)
	}
	for name, s := range map[string]*corev1.Secret{
		"other cluster":   {ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Labels: robotsecret.Labels("x", "a", "b")}},
		"other namespace": {ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Labels: robotsecret.Labels(testCluster, "a", "b")}},
		"unmanaged":       {ObjectMeta: metav1.ObjectMeta{Namespace: testNS}},
	} {
		if got := r.secretToHarborAccess(context.Background(), s); len(got) != 0 {
			t.Errorf("%s: mapped to %v, want nothing", name, got)
		}
	}
}

func TestRequeueAfter(t *testing.T) {
	r := &Reconciler{}
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ha := newHarborAccess()
	ha.UID = "0b2f6a4e-uid"
	// Far rotation: bounded by the resync interval (minus <=10% jitter).
	got := r.requeueAfter(ha, now.Add(20*time.Hour), now)
	if got > ResyncInterval || got < ResyncInterval*9/10 {
		t.Errorf("far rotation: %s, want within [%s, %s]", got, ResyncInterval*9/10, ResyncInterval)
	}
	// Near rotation: exactly promise + margin.
	if got := r.requeueAfter(ha, now.Add(10*time.Minute), now); got != 10*time.Minute+RotationSafetyMargin {
		t.Errorf("near rotation: %s", got)
	}
	// Overdue: floor of one second, never zero (zero means "no requeue").
	if got := r.requeueAfter(ha, now.Add(-time.Hour), now); got != time.Second {
		t.Errorf("overdue: %s", got)
	}
}
