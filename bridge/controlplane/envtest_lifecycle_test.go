// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// TestEnvtest_Lifecycle runs the reconciler against a real apiserver
// through a HarborAccess's whole life: create, a deleted Secret rebuilt via
// the Secret watch, a serviceAccountRef change revoking the old robot, and
// deletion revoking every robot before the finalizer is released. It also
// proves the CRD rejects names that could never be reconciled.
func TestEnvtest_Lifecycle(t *testing.T) {
	cfg := setupEnvtest(t)
	ctx := context.Background()

	k8s, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{testNS, "tenant"} {
		if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: testScheme, Metrics: metricsserver.Options{BindAddress: "0"},
		// Several envtest managers share this process; the controller
		// name is registered globally for metrics.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)}})
	if err != nil {
		t.Fatal(err)
	}
	mh := newMockHarbor()
	rec := &Reconciler{Client: mgr.GetClient(), Scheme: testScheme, Harbor: mh, Config: testReconcilerConfig(), Clock: RealClock{}}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	mgrCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() { cancel(); <-done })

	eventually := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for: %s", what)
	}
	robotNamed := func(name string) bool {
		mh.mu.Lock()
		defer mh.mu.Unlock()
		for _, r := range mh.robots {
			if r.Name == name {
				return true
			}
		}
		return false
	}
	secretKey := types.NamespacedName{Namespace: testNS, Name: robotsecret.Name("tenant", "app")}

	// The CRD rejects a name longer than 63 characters.
	long := newHarborAccess()
	long.Namespace, long.Name, long.Finalizers = "tenant", strings.Repeat("n", 64), nil
	if err := k8s.Create(ctx, long); err == nil || !strings.Contains(err.Error(), "63") {
		t.Fatalf("64-character name: got %v, want a CRD validation error", err)
	}

	// Create.
	ha := newHarborAccess()
	ha.Namespace, ha.Name, ha.Finalizers = "tenant", "app", nil
	if err := k8s.Create(ctx, ha); err != nil {
		t.Fatal(err)
	}
	firstRobot := "bridge-" + testCluster + ".flux-system.source-controller"
	eventually("robot and Secret created, Ready", func() bool {
		got := &harborv1alpha1.HarborAccess{}
		return robotNamed(firstRobot) &&
			k8s.Get(ctx, secretKey, &corev1.Secret{}) == nil &&
			k8s.Get(ctx, client.ObjectKeyFromObject(ha), got) == nil && readyTrue(got)
	})

	// A deleted Secret is rebuilt through the Secret watch (not a resync).
	if err := k8s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: secretKey.Namespace, Name: secretKey.Name}}); err != nil {
		t.Fatal(err)
	}
	eventually("Secret rebuilt after deletion", func() bool {
		s := &corev1.Secret{}
		return k8s.Get(ctx, secretKey, s) == nil && len(s.Data["password"]) > 0 && s.DeletionTimestamp == nil
	})

	// serviceAccountRef change: new robot, old robot revoked.
	cur := &harborv1alpha1.HarborAccess{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(ha), cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.ServiceAccountRef.Name = "new-sa"
	if err := k8s.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	secondRobot := "bridge-" + testCluster + ".flux-system.new-sa"
	eventually("old robot revoked, new robot present", func() bool {
		return robotNamed(secondRobot) && !robotNamed(firstRobot)
	})

	// Deletion revokes every robot and the Secret, then releases the CR.
	if err := k8s.Delete(ctx, cur); err != nil {
		t.Fatal(err)
	}
	eventually("HarborAccess gone, no robots, no Secret", func() bool {
		return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKeyFromObject(ha), &harborv1alpha1.HarborAccess{})) &&
			!robotNamed(secondRobot) &&
			apierrors.IsNotFound(k8s.Get(ctx, secretKey, &corev1.Secret{}))
	})
}
