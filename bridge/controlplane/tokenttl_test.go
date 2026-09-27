// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
)

// A tokenTTL the CRD admitted before it pinned Go duration syntax (e.g.
// "1d") decodes without an error; the reconciler reports it as InvalidSpec
// instead of provisioning a robot.
func TestReconcile_ReportsTokenTTLGoCannotParse(t *testing.T) {
	ha := newHarborAccess()
	if err := json.Unmarshal([]byte(`"1d"`), &ha.Spec.TokenTTL); err != nil {
		t.Fatal(err)
	}
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.createCalls) != 0 {
		t.Error("robot created for a HarborAccess with an unparseable tokenTTL")
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonInvalidSpec)
	if c := meta.FindStatusCondition(got.Status.Conditions, harborv1alpha1.ConditionReady); c == nil ||
		!strings.Contains(c.Message, `spec.tokenTTL: "1d"`) {
		t.Errorf("Ready condition = %+v, want its message to name spec.tokenTTL and the value", c)
	}
}
