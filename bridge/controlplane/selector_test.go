// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// ADR-0026: one audience and a selected set of HarborAccess objects.

func selective(cfg *Config) *Config {
	cfg.HarborAccessSelector = labels.SelectorFromSet(labels.Set{"harbor.aetherize.io/bridge": "a"})
	cfg.Instance = "bridge-a"
	return cfg
}

func TestConfig_FinalizerAndSelects(t *testing.T) {
	plain := &Config{}
	if got := plain.Finalizer(); got != FinalizerName {
		t.Errorf("no selector: finalizer %q, want %q", got, FinalizerName)
	}
	if !plain.Selects(newHarborAccess()) {
		t.Error("no selector must select every HarborAccess")
	}
	sel := selective(&Config{})
	if got := sel.Finalizer(); got != FinalizerName+"-bridge-a" {
		t.Errorf("selector: finalizer %q", got)
	}
	ha := newHarborAccess()
	if sel.Selects(ha) {
		t.Error("unlabelled HarborAccess selected")
	}
	ha.Labels = map[string]string{"harbor.aetherize.io/bridge": "a"}
	if !sel.Selects(ha) {
		t.Error("labelled HarborAccess not selected")
	}
}

func TestLoadFromEnv_SelectorRequiresInstance(t *testing.T) {
	for _, tc := range []struct {
		name, selector, instance string
		wantErr                  bool
	}{
		{"no selector", "", "", false},
		{"selector with instance", "harbor.aetherize.io/bridge=a", "bridge-a", false},
		{"selector without instance", "harbor.aetherize.io/bridge=a", "", true},
		{"invalid selector", "a==b==c", "bridge-a", true},
		{"invalid instance", "harbor.aetherize.io/bridge=a", "Bridge_A", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAllEnv(t)
			setEnv(t, map[string]string{
				EnvClusterName:          "prod",
				EnvNamespace:            "harbor-bridge-system",
				EnvOIDCIssuer:           "https://kubernetes.default.svc",
				EnvHarborURL:            "https://harbor.example.com",
				EnvHarborAdminDir:       "/var/run/secrets/harbor-admin",
				EnvAudience:             "harbor-bridge-prod",
				EnvHarborAccessSelector: tc.selector,
				EnvInstance:             tc.instance,
			})
			_, err := LoadFromEnv()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestReconcile_RejectsForeignAudience(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.TrustPolicy.Audience = "https://kubernetes.default.svc"
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.createCalls) != 0 {
		t.Error("robot created for a CR naming another audience")
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonAudienceMismatch)
}

func TestReconcile_SelectorScopesObjectsAndFinalizer(t *testing.T) {
	unselected := newHarborAccess()
	unselected.Finalizers = nil
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, unselected)
	selective(r.Config)
	if _, err := r.Reconcile(context.Background(), reqFor(unselected)); err != nil {
		t.Fatal(err)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(unselected).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if len(mh.createCalls) != 0 || len(got.Finalizers) != 0 {
		t.Fatalf("unselected CR touched: creates=%d finalizers=%v", len(mh.createCalls), got.Finalizers)
	}

	selected := newHarborAccess()
	selected.Finalizers = nil
	selected.Labels = map[string]string{"harbor.aetherize.io/bridge": "a"}
	mh = newMockHarbor()
	r = newReconciler(t, mh, fixedClock{time.Now()}, selected)
	selective(r.Config)
	if _, err := r.Reconcile(context.Background(), reqFor(selected)); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), reqFor(selected).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if len(mh.createCalls) != 1 {
		t.Errorf("selected CR: %d creates, want 1", len(mh.createCalls))
	}
	if !controllerutil.ContainsFinalizer(got, FinalizerName+"-bridge-a") || controllerutil.ContainsFinalizer(got, FinalizerName) {
		t.Errorf("finalizers %v, want only the instance finalizer", got.Finalizers)
	}
}

// A CR created before the selector existed carries the shared finalizer.
// Deleting it must release that one too, or the deletion hangs.
func TestReconcileDelete_SelectiveBridgeReleasesLegacyFinalizer(t *testing.T) {
	ha := newHarborAccess()
	ha.Labels = map[string]string{"harbor.aetherize.io/bridge": "a"}
	ha.Finalizers = []string{FinalizerName}
	now := metav1.Now()
	ha.DeletionTimestamp = &now
	mh := newMockHarbor()
	id := mh.preexisting("bridge-prod-eu-west.flux-system.source-controller", RobotDescription(testCluster, ha.Namespace, ha.Name))
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	selective(r.Config)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.deleteCalls) != 1 || mh.deleteCalls[0] != id {
		t.Errorf("robot not revoked: deletes %v", mh.deleteCalls)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err == nil && len(got.Finalizers) != 0 {
		t.Errorf("finalizers left: %v", got.Finalizers)
	}
}

func TestJanitor_ReleasesHarborAccessNoLongerSelected(t *testing.T) {
	moved := newHarborAccess() // no bridge label: moved away from bridge-a
	moved.Finalizers = []string{FinalizerName + "-bridge-a", "example.com/other"}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNS, Name: robotsecret.Name(moved.Namespace, moved.Name),
		Labels: robotsecret.Labels(testCluster, moved.Namespace, moved.Name),
	}}
	mh := newMockHarbor()
	id := mh.preexisting("bridge-prod-eu-west.flux-system.source-controller", RobotDescription(testCluster, moved.Namespace, moved.Name))
	j := newJanitor(t, mh, moved, secret)
	selective(j.Config)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mh.deleteCalls) != 1 || mh.deleteCalls[0] != id {
		t.Errorf("robot of the moved CR not revoked: deletes %v", mh.deleteCalls)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := j.Client.Get(context.Background(), reqFor(moved).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 || got.Finalizers[0] != "example.com/other" {
		t.Errorf("finalizers %v, want only the foreign one left", got.Finalizers)
	}
	if err := j.Client.Get(context.Background(), clientKey(secret), &corev1.Secret{}); err == nil {
		t.Error("robot Secret of the moved CR not deleted")
	}
}

func TestJanitor_KeepsFinalizerWhileRevocationFails(t *testing.T) {
	moved := newHarborAccess()
	moved.Finalizers = []string{FinalizerName + "-bridge-a"}
	mh := newMockHarbor()
	mh.preexisting("bridge-prod-eu-west.flux-system.source-controller", RobotDescription(testCluster, moved.Namespace, moved.Name))
	mh.errOnDelete = errors.New("harbor down")
	j := newJanitor(t, mh, moved)
	selective(j.Config)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := j.Client.Get(context.Background(), reqFor(moved).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(got, FinalizerName+"-bridge-a") {
		t.Error("finalizer released although the robot could not be revoked")
	}
}

func TestJanitor_LeavesSelectedHarborAccessAlone(t *testing.T) {
	ha := newHarborAccess()
	ha.Labels = map[string]string{"harbor.aetherize.io/bridge": "a"}
	ha.Finalizers = []string{FinalizerName + "-bridge-a"}
	mh := newMockHarbor()
	mh.preexisting("bridge-prod-eu-west.flux-system.source-controller", RobotDescription(testCluster, ha.Namespace, ha.Name))
	j := newJanitor(t, mh, ha)
	selective(j.Config)
	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mh.deleteCalls) != 0 {
		t.Errorf("robot of a selected CR deleted: %v", mh.deleteCalls)
	}
}

func clientKey(s *corev1.Secret) types.NamespacedName {
	return types.NamespacedName{Namespace: s.Namespace, Name: s.Name}
}
