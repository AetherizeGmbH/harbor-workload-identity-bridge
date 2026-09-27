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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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
// the Secret watch, a serviceAccountRef change revoking the old robot, a
// refusal suspending the robot and its end resuming it (ADR-0030), a
// rename (a second HarborAccess for the same ServiceAccount takes over when
// the first is deleted), and deletion revoking every robot before the
// finalizer is released. It also
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
	if rec.APIReader != mgr.GetAPIReader() {
		t.Fatal("SetupWithManager did not wire the uncached API reader for the robot Secret")
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
	// storedMatchesHarbor: the Secret holds exactly the password Harbor
	// accepts for the named robot, not merely some password.
	storedMatchesHarbor := func(robot string) bool {
		s := &corev1.Secret{}
		if k8s.Get(ctx, secretKey, s) != nil || s.DeletionTimestamp != nil {
			return false
		}
		mh.mu.Lock()
		defer mh.mu.Unlock()
		for _, r := range mh.robots {
			if r.Name == robot {
				return r.Secret != "" && string(s.Data["password"]) == r.Secret &&
					string(s.Data["username"]) == r.WireName
			}
		}
		return false
	}

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
		return robotNamed(firstRobot) && storedMatchesHarbor(firstRobot) &&
			k8s.Get(ctx, client.ObjectKeyFromObject(ha), got) == nil && readyTrue(got)
	})

	// A deleted Secret is rebuilt through the Secret watch (not a resync).
	if err := k8s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: secretKey.Namespace, Name: secretKey.Name}}); err != nil {
		t.Fatal(err)
	}
	eventually("Secret rebuilt after deletion with the password Harbor accepts", func() bool {
		return storedMatchesHarbor(firstRobot)
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
	eventually("old robot revoked, new robot present, its password stored", func() bool {
		return robotNamed(secondRobot) && !robotNamed(firstRobot) && storedMatchesHarbor(secondRobot)
	})

	// Suspension (ADR-0030): naming another audience disables the robot
	// and deletes its Secret; restoring the audience rotates the password
	// while the robot is still disabled, then re-enables it.
	editSpec := func(key client.ObjectKey, edit func(*harborv1alpha1.HarborAccess)) {
		t.Helper()
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			got := &harborv1alpha1.HarborAccess{}
			if err := k8s.Get(ctx, key, got); err != nil {
				return err
			}
			edit(got)
			return k8s.Update(ctx, got)
		}); err != nil {
			t.Fatal(err)
		}
	}
	robotState := func(name string) (disabled, suspended, found bool) {
		mh.mu.Lock()
		defer mh.mu.Unlock()
		for _, r := range mh.robots {
			if r.Name == name {
				return r.Disabled, RobotSuspended(r.Description), true
			}
		}
		return false, false, false
	}
	beforeSuspension := &corev1.Secret{}
	if err := k8s.Get(ctx, secretKey, beforeSuspension); err != nil {
		t.Fatal(err)
	}
	editSpec(client.ObjectKeyFromObject(ha), func(h *harborv1alpha1.HarborAccess) { h.Spec.TrustPolicy.Audience = "another-bridge" })
	eventually("robot suspended, its Secret deleted", func() bool {
		disabled, suspended, found := robotState(secondRobot)
		return found && disabled && suspended && apierrors.IsNotFound(k8s.Get(ctx, secretKey, &corev1.Secret{}))
	})
	editSpec(client.ObjectKeyFromObject(ha), func(h *harborv1alpha1.HarborAccess) { h.Spec.TrustPolicy.Audience = testReconcilerConfig().Audience })
	eventually("robot resumed with a new password, stored", func() bool {
		disabled, suspended, found := robotState(secondRobot)
		s := &corev1.Secret{}
		return found && !disabled && !suspended && storedMatchesHarbor(secondRobot) &&
			k8s.Get(ctx, secretKey, s) == nil && string(s.Data["password"]) != string(beforeSuspension.Data["password"])
	})
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(ha), cur); err != nil {
		t.Fatal(err)
	}

	// Rename: a second HarborAccess for the same ServiceAccount is refused
	// while the first owns the robot, and takes over once the first is
	// deleted, through the deletion event rather than a resync.
	v2 := newHarborAccess()
	v2.Namespace, v2.Name, v2.Finalizers = "tenant", "app-v2", nil
	v2.Spec.ServiceAccountRef.Name = "new-sa"
	if err := k8s.Create(ctx, v2); err != nil {
		t.Fatal(err)
	}
	eventually("second HarborAccess for the same ServiceAccount reports RobotConflict", func() bool {
		got := &harborv1alpha1.HarborAccess{}
		if k8s.Get(ctx, client.ObjectKeyFromObject(v2), got) != nil {
			return false
		}
		c := meta.FindStatusCondition(got.Status.Conditions, harborv1alpha1.ConditionReady)
		return c != nil && c.Reason == ReasonRobotConflict
	})

	// Deletion revokes every robot and the Secret, then releases the CR.
	if err := k8s.Delete(ctx, cur); err != nil {
		t.Fatal(err)
	}
	eventually("HarborAccess gone, its Secret gone", func() bool {
		return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKeyFromObject(ha), &harborv1alpha1.HarborAccess{})) &&
			apierrors.IsNotFound(k8s.Get(ctx, secretKey, &corev1.Secret{}))
	})
	v2SecretKey := types.NamespacedName{Namespace: testNS, Name: robotsecret.Name("tenant", "app-v2")}
	eventually("the renamed HarborAccess took the robot over", func() bool {
		got := &harborv1alpha1.HarborAccess{}
		s := &corev1.Secret{}
		return k8s.Get(ctx, client.ObjectKeyFromObject(v2), got) == nil && readyTrue(got) &&
			robotNamed(secondRobot) && k8s.Get(ctx, v2SecretKey, s) == nil && len(s.Data["password"]) > 0
	})

	if err := k8s.Delete(ctx, v2); err != nil {
		t.Fatal(err)
	}
	eventually("both HarborAccess objects gone, no robots, no Secrets", func() bool {
		return apierrors.IsNotFound(k8s.Get(ctx, client.ObjectKeyFromObject(v2), &harborv1alpha1.HarborAccess{})) &&
			!robotNamed(secondRobot) &&
			apierrors.IsNotFound(k8s.Get(ctx, v2SecretKey, &corev1.Secret{}))
	})
}
