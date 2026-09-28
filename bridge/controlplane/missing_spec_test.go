// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// A HarborAccess stored without spec before the CRD required it is
// refused as a missing spec, not as an issuer mismatch, and gets no robot.
// Like every refused object (ADR-0030) it loses its robot Secret, is
// re-checked every resync, and, with no robot of it left in Harbor, loses
// the finalizer (ADR-0032).
func TestReconcile_ReportsMissingSpec(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec = harborv1alpha1.HarborAccessSpec{}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testSecretKey.Name,
			Labels: robotsecret.Labels(testCluster, testHANamespace, testHAName)},
		Data: map[string][]byte{"username": []byte(mockRobotPrefix + testRobotName), "password": []byte("from-before")},
	}
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha, secret)
	res, err := r.Reconcile(context.Background(), reqFor(ha))
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > ResyncInterval {
		t.Errorf("RequeueAfter = %s, want (0, %s]", res.RequeueAfter, ResyncInterval)
	}
	if len(mh.createCalls)+len(mh.updateCalls)+len(mh.deleteCalls) != 0 {
		t.Errorf("Harbor writes for a HarborAccess without spec: creates %d, updates %d, deletes %v",
			len(mh.createCalls), len(mh.updateCalls), mh.deleteCalls)
	}
	if !secretGone(t, r) {
		t.Error("robot Secret of a HarborAccess without spec survived")
	}
	got := getHA(t, r)
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonInvalidSpec)
	assertCondition(t, got, harborv1alpha1.ConditionRobotProvisioned, metav1.ConditionFalse, ReasonInvalidSpec)
	if c := meta.FindStatusCondition(got.Status.Conditions, harborv1alpha1.ConditionReady); c == nil ||
		!strings.Contains(c.Message, "spec is missing") {
		t.Errorf("Ready condition = %+v, want it to say the spec is missing", c)
	}
	if len(got.Finalizers) != 0 {
		t.Errorf("HarborAccess without spec and without a robot keeps finalizers %v", got.Finalizers)
	}
}

// A robot left from before the spec was removed is not one the refusal can
// suspend: the empty serviceAccountRef maps to no robot name. The janitor
// revokes it (its owner maps to no name Harbor accepts), and the
// finalizer, which promises that revocation, stays until then.
func TestReconcile_MissingSpecKeepsTheFinalizerUntilTheRobotIsRevoked(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec = harborv1alpha1.HarborAccessSpec{}
	mh := newMockHarbor()
	id := mh.preexisting(testRobotName, RobotDescription(testCluster, testHANamespace, testHAName),
		harbor.ProjectPermission{Project: "production", Action: "pull"})
	r := newReconciler(t, mh, fixedClock{testT0}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(getHA(t, r), FinalizerName) {
		t.Fatal("finalizer released while a robot of the HarborAccess is left")
	}

	j := &Janitor{Client: r.Client, Harbor: mh, Config: r.Config}
	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := mh.robots[id]; ok {
		t.Fatal("janitor kept the robot of a HarborAccess without spec")
	}
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if got := getHA(t, r); len(got.Finalizers) != 0 {
		t.Errorf("finalizers %v kept after the robot was revoked", got.Finalizers)
	}
}
