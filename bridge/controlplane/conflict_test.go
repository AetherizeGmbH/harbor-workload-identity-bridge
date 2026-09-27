// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// RobotConflict is resolved outside the conflicted HarborAccess: in Harbor,
// in a Secret the bridge does not own, or by deleting another HarborAccess.
// None of that is an event on the conflicted object, so it must be
// re-checked on its own.
func TestReconcile_RobotConflictIsRechecked(t *testing.T) {
	robotName := "bridge-prod-eu-west.flux-system.source-controller"
	for _, tc := range []struct {
		name  string
		setup func(mh *mockHarbor) []client.Object
	}{
		{"Secret stamped for another HarborAccess", func(*mockHarbor) []client.Object {
			return []client.Object{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testSecretKey.Name,
				Labels: robotsecret.Labels(testCluster, "other-ns", "other-ha")}}}
		}},
		{"Secret the bridge does not own", func(*mockHarbor) []client.Object {
			return []client.Object{&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testSecretKey.Name},
				Data: map[string][]byte{"username": []byte("someone-else"), "password": []byte("theirs")}}}
		}},
		{"robot not marked as this cluster's", func(mh *mockHarbor) []client.Object {
			mh.preexisting(robotName, "created by hand")
			return nil
		}},
		{"robot of another HarborAccess", func(mh *mockHarbor) []client.Object {
			mh.preexisting(robotName, RobotDescription(testCluster, "other-ns", "other-ha"))
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mh := newMockHarbor()
			ha := newHarborAccess()
			r := newReconciler(t, mh, fixedClock{time.Now()}, append(tc.setup(mh), ha)...)
			res, err := r.Reconcile(context.Background(), reqFor(ha))
			if err != nil {
				t.Fatal(err)
			}
			got := &harborv1alpha1.HarborAccess{}
			if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
				t.Fatal(err)
			}
			assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRobotConflict)
			if res.RequeueAfter <= 0 || res.RequeueAfter > ResyncInterval {
				t.Errorf("RequeueAfter = %s, want (0, %s]", res.RequeueAfter, ResyncInterval)
			}
		})
	}
}

// Renaming a HarborAccess means creating the new one and deleting the old
// one. The new one reports RobotConflict while the old one owns the
// ServiceAccount's robot; deleting the old one must hand the robot name
// over at once, not after the next resync.
func TestReconcile_DeletingTheRobotOwnerRechecksItsSiblings(t *testing.T) {
	ctx := context.Background()
	mh := newMockHarbor()
	oldHA := newHarborAccess()
	newHA := newHarborAccess()
	newHA.Name = "flux-access-v2"
	unrelated := newHarborAccess()
	unrelated.Name = "other-sa"
	unrelated.Spec.ServiceAccountRef.Name = "other"
	r := newReconciler(t, mh, fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}, oldHA, newHA, unrelated)

	if _, err := r.Reconcile(ctx, reqFor(oldHA)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reqFor(newHA)); err != nil {
		t.Fatal(err)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(ctx, reqFor(newHA).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRobotConflict)

	deleted := &harborv1alpha1.HarborAccess{}
	if err := r.Get(ctx, reqFor(oldHA).NamespacedName, deleted); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, reqFor(oldHA)); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, reqFor(oldHA).NamespacedName, &harborv1alpha1.HarborAccess{}); !apierrors.IsNotFound(err) {
		t.Fatalf("setup: old HarborAccess not deleted: %v", err)
	}

	reqs := r.sameServiceAccount(ctx, deleted)
	if want := (reconcile.Request{NamespacedName: reqFor(newHA).NamespacedName}); !slices.Equal(reqs, []reconcile.Request{want}) {
		t.Fatalf("deleting the robot owner enqueues %v, want only %v", reqs, want)
	}
	if _, err := r.Reconcile(ctx, reqs[0]); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, reqFor(newHA).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
	s := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: testNS, Name: robotsecret.Name(newHA.Namespace, newHA.Name)}, s); err != nil {
		t.Fatalf("new HarborAccess got no robot Secret: %v", err)
	}
}

// A Secret event reaches the HarborAccess its labels name and the one
// whose robot-Secret name it occupies: renaming or deleting a Secret that
// blocks a HarborAccess (RobotConflict) re-checks that HarborAccess.
func TestSecretToHarborAccess_MapsByNameToo(t *testing.T) {
	r := newReconciler(t, newMockHarbor(), fixedClock{time.Now()})
	key := func(ns, name string) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
	}
	for name, tc := range map[string]struct {
		secret *corev1.Secret
		want   []reconcile.Request
	}{
		"own Secret, mapped once": {
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "robot-a.b", Labels: robotsecret.Labels(testCluster, "a", "b")}},
			[]reconcile.Request{key("a", "b")},
		},
		"unlabelled Secret at a robot-Secret name": {
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "robot-a.b"}},
			[]reconcile.Request{key("a", "b")},
		},
		"Secret stamped for another HarborAccess": {
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "robot-a.b", Labels: robotsecret.Labels(testCluster, "x", "y")}},
			[]reconcile.Request{key("x", "y"), key("a", "b")},
		},
		"another cluster's labels": {
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "robot-a.b", Labels: robotsecret.Labels("other", "x", "y")}},
			[]reconcile.Request{key("a", "b")},
		},
		"unrelated Secret": {
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "harbor-admin"}},
			nil,
		},
		"another namespace": {
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "elsewhere", Name: "robot-a.b", Labels: robotsecret.Labels(testCluster, "a", "b")}},
			nil,
		},
	} {
		if got := r.secretToHarborAccess(context.Background(), tc.secret); !slices.Equal(got, tc.want) {
			t.Errorf("%s: mapped to %v, want %v", name, got, tc.want)
		}
	}
}

// Objects re-checked because of a condition outside the CR are spread over
// the last tenth of the resync interval by their UID, like Ready ones:
// every object is reconciled when the bridge starts, and a fixed interval
// would send all of their Harbor lookups at once an interval later.
func TestReconcile_ResyncRequeueIsSpreadPerObject(t *testing.T) {
	robotName := "bridge-prod-eu-west.flux-system.source-controller"
	requeue := func(uid types.UID) time.Duration {
		t.Helper()
		mh := newMockHarbor()
		mh.preexisting(robotName, RobotDescription(testCluster, "other-ns", "other-ha"))
		ha := newHarborAccess()
		ha.UID = uid
		r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
		res, err := r.Reconcile(context.Background(), reqFor(ha))
		if err != nil {
			t.Fatal(err)
		}
		return res.RequeueAfter
	}
	a, b := requeue("uid-a"), requeue("uid-b")
	for _, got := range []time.Duration{a, b} {
		if got < ResyncInterval*9/10 || got > ResyncInterval {
			t.Errorf("RequeueAfter = %s, want within [%s, %s]", got, ResyncInterval*9/10, ResyncInterval)
		}
	}
	if a == b {
		t.Errorf("two objects requeue after the same %s; the resyncs are not spread", a)
	}
}
