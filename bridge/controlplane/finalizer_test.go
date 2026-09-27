// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// A HarborAccess the bridge refuses never gets a robot of its own, so it
// must not get the finalizer either: deleting it would otherwise wait for
// this bridge and its Harbor (with overlapping selectors, a bridge that
// never served it).
func TestReconcile_RefusedHarborAccessGetsNoFinalizer(t *testing.T) {
	for name, tc := range map[string]struct {
		edit    func(*harborv1alpha1.HarborAccess)
		objects []client.Object
	}{
		"AudienceMismatch": {edit: func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TrustPolicy.Audience = "another-bridge" }},
		"IssuerMismatch":   {edit: func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TrustPolicy.Issuer = "https://other.example.com" }},
		"InvalidSpec": {edit: func(ha *harborv1alpha1.HarborAccess) {
			ha.Spec.Permissions[0].Project = "*"
		}},
		"RobotConflict on the Secret": {
			edit: func(*harborv1alpha1.HarborAccess) {},
			objects: []client.Object{&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testSecretKey.Name},
				Data:       map[string][]byte{"username": []byte("someone-else"), "password": []byte("theirs")},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ha := newHarborAccess()
			ha.Finalizers = nil
			tc.edit(ha)
			mh := newMockHarbor()
			r := newReconciler(t, mh, fixedClock{time.Now()}, append(tc.objects, ha)...)
			if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
				t.Fatal(err)
			}
			if got := getHA(t, r); len(got.Finalizers) != 0 {
				t.Errorf("refused HarborAccess got finalizers %v", got.Finalizers)
			}
		})
	}
}

// A HarborAccess that holds the finalizer and becomes refused keeps it:
// its robot (suspended, ADR-0030) still has to be revoked on deletion.
func TestReconcile_RefusedHarborAccessKeepsItsFinalizer(t *testing.T) {
	mh := newMockHarbor()
	r, id := provisioned(t, mh)
	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TrustPolicy.Audience = "another-bridge" })
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}
	got := getHA(t, r)
	if !controllerutil.ContainsFinalizer(got, FinalizerName) {
		t.Fatalf("finalizer dropped: %v", got.Finalizers)
	}
	if err := r.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}
	if _, ok := mh.robots[id]; ok {
		t.Error("the suspended robot survived the deletion of its HarborAccess")
	}
}

// Deleting a HarborAccess deletes only a Secret that belongs to it, by the
// rule the reconciler writes by: never one the bridge refused to adopt.
func TestReconcile_Delete_DeletesOnlyASecretThatBelongsToTheHarborAccess(t *testing.T) {
	for name, tc := range map[string]struct {
		username string
		labels   map[string]string
		wantKept bool
	}{
		"someone else's unmanaged Secret":                   {username: "someone-else", wantKept: true},
		"unmanaged Secret holding this robot's credentials": {username: testRobotName, wantKept: false},
		"its Secret": {username: testRobotName, labels: robotsecret.Labels(testCluster, testHANamespace, testHAName), wantKept: false},
		// A bridge with another clusterName that shares the namespace
		// writes the same Secret names; the Secret is that bridge's.
		"a Secret stamped for another cluster": {username: testRobotName, labels: robotsecret.Labels("other-cluster", testHANamespace, testHAName), wantKept: true},
	} {
		t.Run(name, func(t *testing.T) {
			ha := newHarborAccess()
			now := metav1.Now()
			ha.DeletionTimestamp = &now
			s := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testSecretKey.Name, Labels: tc.labels},
				Data:       map[string][]byte{"username": []byte(tc.username), "password": []byte("pw")},
			}
			r := newReconciler(t, newMockHarbor(), fixedClock{time.Now()}, ha, s)
			if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
				t.Fatal(err)
			}
			if kept := !secretGone(t, r); kept != tc.wantKept {
				t.Errorf("Secret kept = %v, want %v", kept, tc.wantKept)
			}
			if err := r.Get(context.Background(), reqFor(ha).NamespacedName, &harborv1alpha1.HarborAccess{}); err == nil {
				t.Error("finalizer not released")
			}
		})
	}
}

// A selective bridge holds the per-instance finalizer and, on an object
// created before the selector, the shared one. When Harbor is down the
// condition must name every finalizer to remove by hand, and only those
// still present.
func TestReconcile_Delete_BlockedMessageNamesEveryHeldFinalizer(t *testing.T) {
	ha := newHarborAccess()
	ha.Labels = map[string]string{"harbor.aetherize.io/bridge": "a"}
	ha.Finalizers = []string{FinalizerName + "-bridge-a", FinalizerName}
	now := metav1.Now()
	ha.DeletionTimestamp = &now
	mh := newMockHarbor()
	mh.errOnList = errors.New("harbor unreachable")
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	selective(r.Config)

	message := func() string {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), reqFor(ha)); err == nil {
			t.Fatal("expected the deletion to be retried")
		}
		c := meta.FindStatusCondition(getHA(t, r).Status.Conditions, harborv1alpha1.ConditionReady)
		if c == nil || c.Reason != ReasonDeletionBlocked {
			t.Fatalf("condition %+v, want DeletionBlocked", c)
		}
		return c.Message
	}
	if msg := message(); !strings.Contains(msg, `"harbor.aetherize.io/robot-bridge-a"`) || !strings.Contains(msg, `"harbor.aetherize.io/robot"`) {
		t.Errorf("message does not name both finalizers: %s", msg)
	}

	got := getHA(t, r)
	patch := client.MergeFrom(got.DeepCopy())
	controllerutil.RemoveFinalizer(got, FinalizerName+"-bridge-a")
	if err := r.Patch(context.Background(), got, patch); err != nil {
		t.Fatal(err)
	}
	if msg := message(); strings.Contains(msg, "robot-bridge-a") || !strings.Contains(msg, `"harbor.aetherize.io/robot"`) {
		t.Errorf("message after removing the instance finalizer by hand: %s", msg)
	}
}

// Removing the selector: objects keep the per-instance finalizer the
// bridge set while it had one. As long as BRIDGE_INSTANCE still names the
// instance, the bridge releases it on deletion.
func TestReconcileDelete_ReleasesTheInstanceFinalizerAfterTheSelectorIsRemoved(t *testing.T) {
	ha := newHarborAccess()
	ha.Finalizers = []string{FinalizerName + "-bridge-a", FinalizerName}
	now := metav1.Now()
	ha.DeletionTimestamp = &now
	mh := newMockHarbor()
	id := mh.preexisting(testRobotName, RobotDescription(testCluster, ha.Namespace, ha.Name))
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	r.Config.Instance = "bridge-a" // no selector
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if _, ok := mh.robots[id]; ok {
		t.Error("robot not revoked")
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err == nil {
		t.Errorf("deletion hangs on finalizers %v", got.Finalizers)
	}
}

func TestConfig_ReleasedFinalizers(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  *Config
		want []string
	}{
		"no selector":                     {&Config{}, []string{FinalizerName}},
		"no selector, instance known":     {&Config{Instance: "bridge-a"}, []string{FinalizerName, FinalizerName + "-bridge-a"}},
		"selector: instance, then shared": {selective(&Config{}), []string{FinalizerName + "-bridge-a", FinalizerName}},
	} {
		if got := tc.cfg.ReleasedFinalizers(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// Adding a selector: an object that stops matching still carries the
// shared finalizer the bridge set before. Once its robot is revoked the
// janitor releases that finalizer too, but only on objects this bridge
// served last (their status names one of this cluster's robots), so a
// shared finalizer another bridge set is left alone.
func TestJanitor_ReleasesTheSharedFinalizerOfObjectsItServed(t *testing.T) {
	servedHere := newHarborAccess()
	servedHere.Finalizers = []string{FinalizerName}
	servedHere.Status.Robot = &harborv1alpha1.RobotRef{Name: mockRobotPrefix + testRobotName}
	servedElsewhere := newHarborAccess()
	servedElsewhere.Name = "served-elsewhere"
	servedElsewhere.Finalizers = []string{FinalizerName}
	servedElsewhere.Status.Robot = &harborv1alpha1.RobotRef{Name: mockRobotPrefix + "bridge-other-cluster.flux-system.x"}
	mh := newMockHarbor()
	id := mh.preexisting(testRobotName, RobotDescription(testCluster, servedHere.Namespace, servedHere.Name))
	j := newJanitor(t, mh, servedHere, servedElsewhere)
	selective(j.Config)
	j.Config.HarborRobotPrefix = mockRobotPrefix

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := mh.robots[id]; ok {
		t.Fatal("robot of the unselected object not revoked")
	}
	for _, tc := range []struct {
		ha   *harborv1alpha1.HarborAccess
		want []string
	}{
		{servedHere, nil},
		{servedElsewhere, []string{FinalizerName}},
	} {
		got := &harborv1alpha1.HarborAccess{}
		if err := j.Client.Get(context.Background(), client.ObjectKeyFromObject(tc.ha), got); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Finalizers, tc.want) {
			t.Errorf("%s: finalizers %v, want %v", tc.ha.Name, got.Finalizers, tc.want)
		}
	}
}

// Bridges before ADR-0032 added the finalizer to every object they
// selected, also to those they refused and never gave a robot. Such an
// object must not keep depending on this bridge and its Harbor to be
// deleted: once no robot of it is left, the finalizer goes.
func TestReconcile_RefusedHarborAccessReleasesTheFinalizerOfAnOlderBridge(t *testing.T) {
	ha := newHarborAccess() // holds FinalizerName
	ha.Spec.TrustPolicy.Audience = "another-bridge"
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	got := getHA(t, r)
	if len(got.Finalizers) != 0 {
		t.Fatalf("refused HarborAccess without a robot keeps finalizers %v", got.Finalizers)
	}

	// Later passes do not ask Harbor at all, and the deletion does not
	// wait for Harbor.
	mh.getByName, mh.listCalls = 0, 0
	mh.errOnList = errors.New("harbor unreachable")
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if mh.getByName+mh.listCalls != 0 {
		t.Errorf("a pass over a refused HarborAccess without a finalizer of this bridge asked Harbor %d times", mh.getByName+mh.listCalls)
	}
	if err := r.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, &harborv1alpha1.HarborAccess{}); err == nil {
		t.Error("deleting the refused HarborAccess waits for this bridge")
	}
}

// A refused object that never held a finalizer of this bridge has no
// robot of it: the pass asks Harbor nothing (overlapping selectors and
// fleet-wide GitOps can make many such objects).
func TestReconcile_RefusedHarborAccessWithoutAFinalizerAsksHarborNothing(t *testing.T) {
	ha := newHarborAccess()
	ha.Finalizers = nil
	ha.Spec.TrustPolicy.Issuer = "https://other-cluster.example.com"
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	for range 3 {
		if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
			t.Fatal(err)
		}
	}
	if mh.getByName+mh.listCalls != 0 {
		t.Errorf("%d Harbor lookups for a refused HarborAccess this bridge never provisioned", mh.getByName+mh.listCalls)
	}
}

// A robot of the refused object that the suspension does not cover (one of
// an earlier serviceAccountRef, which the janitor deletes) keeps the
// finalizer: its revocation on deletion is still due.
func TestReconcile_RefusedHarborAccessKeepsTheFinalizerWhileARobotIsLeft(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.TrustPolicy.Audience = "another-bridge"
	mh := newMockHarbor()
	earlier, err := harbor.RobotName(testCluster, testSANamespace, "previous-sa")
	if err != nil {
		t.Fatal(err)
	}
	mh.preexisting(earlier, RobotDescription(testCluster, ha.Namespace, ha.Name), harbor.ProjectPermission{Project: "production", Action: "pull"})
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if got := getHA(t, r); !controllerutil.ContainsFinalizer(got, FinalizerName) {
		t.Errorf("finalizer released while a robot of the HarborAccess is left: %v", got.Finalizers)
	}
}

// A robot the bridge's description claims for the refused object but whose
// name the ownership prefix does not cover (configured robot prefix shorter
// than Harbor's) can be neither suspended nor taken for absent: releasing
// the finalizer would let a deletion skip a robot with a valid password.
func TestReconcile_RefusedHarborAccessKeepsTheFinalizerForARobotItCannotRecogniseByName(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.TrustPolicy.Audience = "another-bridge"
	mh := newMockHarbor()
	mh.preexisting("ci-bridge-prod-eu-west.flux-system.source-controller", RobotDescription(testCluster, ha.Namespace, ha.Name),
		harbor.ProjectPermission{Project: "production", Action: "pull"})
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); !errors.Is(err, harbor.ErrRobotPrefixMismatch) {
		t.Fatalf("Reconcile err = %v, want ErrRobotPrefixMismatch so the pass is retried", err)
	}
	if len(mh.deleteCalls)+len(mh.updateCalls) != 0 {
		t.Errorf("robot outside the ownership prefix touched: deletes %v, updates %d", mh.deleteCalls, len(mh.updateCalls))
	}
	if got := getHA(t, r); !controllerutil.ContainsFinalizer(got, FinalizerName) {
		t.Errorf("finalizer released while a robot of the HarborAccess survives: %v", got.Finalizers)
	}
}

// With a selector the bridge releases only its per-instance finalizer from
// a refused object: a bridge without a selector that serves the object
// sets the shared one too, and would add it back at once.
func TestReconcile_SelectiveBridgeReleasesOnlyItsInstanceFinalizerFromARefusedObject(t *testing.T) {
	ha := newHarborAccess()
	ha.Labels = map[string]string{"harbor.aetherize.io/bridge": "a"}
	ha.Finalizers = []string{FinalizerName + "-bridge-a", FinalizerName}
	ha.Spec.TrustPolicy.Audience = "another-bridge"
	r := newReconciler(t, newMockHarbor(), fixedClock{time.Now()}, ha)
	selective(r.Config)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if got := getHA(t, r).Finalizers; !slices.Equal(got, []string{FinalizerName}) {
		t.Errorf("finalizers %v, want only the shared one left", got)
	}
}
