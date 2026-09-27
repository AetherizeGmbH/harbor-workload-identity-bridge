// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
)

// A HarborAccess stored without spec before the CRD required it is
// reported as a missing spec, not as an issuer mismatch, and gets no robot.
func TestReconcile_ReportsMissingSpec(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec = harborv1alpha1.HarborAccessSpec{}
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.createCalls) != 0 {
		t.Error("robot created for a HarborAccess without spec")
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonInvalidSpec)
	if c := meta.FindStatusCondition(got.Status.Conditions, harborv1alpha1.ConditionReady); c == nil ||
		!strings.Contains(c.Message, "spec is missing") {
		t.Errorf("Ready condition = %+v, want it to say the spec is missing", c)
	}
}
