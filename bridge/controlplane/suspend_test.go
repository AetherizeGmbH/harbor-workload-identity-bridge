// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// ADR-0030: a HarborAccess the bridge refuses (issuer or audience
// mismatch, invalid spec) after it was served must not keep a usable
// robot, and fixing it must restore service with a new password.

const testRobotName = "bridge-prod-eu-west.flux-system.source-controller"

var testT0 = time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)

// provisioned reconciles ha to Ready and returns the reconciler and the
// robot's ID.
func provisioned(t *testing.T, mh *mockHarbor, objs ...client.Object) (*Reconciler, int64) {
	t.Helper()
	ha := newHarborAccess()
	r := newReconciler(t, mh, fixedClock{testT0}, append(objs, ha)...)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	return r, mh.createCalls0ID()
}

// editHA applies edit to the stored HarborAccess.
func editHA(t *testing.T, r *Reconciler, edit func(*harborv1alpha1.HarborAccess)) {
	t.Helper()
	cur := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(newHarborAccess()).NamespacedName, cur); err != nil {
		t.Fatal(err)
	}
	edit(cur)
	cur.Generation++
	if err := r.Update(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
}

func getHA(t *testing.T, r *Reconciler) *harborv1alpha1.HarborAccess {
	t.Helper()
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(newHarborAccess()).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	return got
}

func secretGone(t *testing.T, r *Reconciler) bool {
	t.Helper()
	err := r.Get(context.Background(), testSecretKey, &corev1.Secret{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return apierrors.IsNotFound(err)
}

func TestReconcile_RefusedHarborAccessSuspendsItsRobot(t *testing.T) {
	for _, tc := range []struct {
		reason string
		edit   func(*harborv1alpha1.HarborAccess)
	}{
		{ReasonAudienceMismatch, func(ha *harborv1alpha1.HarborAccess) {
			ha.Spec.TrustPolicy.Audience = "https://kubernetes.default.svc"
		}},
		{ReasonIssuerMismatch, func(ha *harborv1alpha1.HarborAccess) {
			ha.Spec.TrustPolicy.Issuer = "https://other-cluster.example.com"
		}},
		{ReasonInvalidSpec, func(ha *harborv1alpha1.HarborAccess) {
			ha.Spec.Permissions = append(ha.Spec.Permissions, harborv1alpha1.ProjectPermission{Project: "*", Action: harborv1alpha1.ActionPullPush})
		}},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			mh := newMockHarbor()
			r, id := provisioned(t, mh)
			editHA(t, r, tc.edit)

			for pass, at := range []time.Time{testT0.Add(time.Hour), testT0.Add(72 * time.Hour)} {
				r.Clock = fixedClock{at}
				res, err := r.Reconcile(context.Background(), reqFor(newHarborAccess()))
				if err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
				if res.RequeueAfter <= 0 || res.RequeueAfter > ResyncInterval {
					t.Errorf("pass %d: RequeueAfter = %s, want (0, %s]: a suspended robot re-enabled in Harbor must be disabled again", pass, res.RequeueAfter, ResyncInterval)
				}
			}

			robot, ok := mh.robots[id]
			if !ok {
				t.Fatal("robot deleted; a robot whose grants can be written back is disabled, not deleted")
			}
			if !robot.Disabled || !RobotSuspended(robot.Description) {
				t.Errorf("robot not suspended: disabled=%v description=%q", robot.Disabled, robot.Description)
			}
			if !harbor.PermissionsMatch(robot, []harbor.ProjectPermission{{Project: "production", Action: "pull"}}) {
				t.Errorf("suspension changed the grants: %+v", robot.Permissions)
			}
			if len(mh.refreshCalls) != 0 {
				t.Errorf("refused HarborAccess rotated: %v", mh.refreshCalls)
			}
			if !secretGone(t, r) {
				t.Error("robot Secret of a refused HarborAccess survived; the data plane could serve it")
			}
			got := getHA(t, r)
			assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, tc.reason)
			assertCondition(t, got, harborv1alpha1.ConditionRobotProvisioned, metav1.ConditionFalse, tc.reason)

			// The janitor keeps the suspended robot of a live, selected
			// HarborAccess: suspension is reversible.
			j := &Janitor{Client: r.Client, Harbor: mh, Config: r.Config}
			if err := j.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, ok := mh.robots[id]; !ok {
				t.Error("janitor deleted the suspended robot of a live HarborAccess")
			}
		})
	}
}

// Fixing the HarborAccess resumes the robot, with a new password: one
// handed out before the suspension must never work again.
func TestReconcile_ResumesSuspendedRobotWithANewPassword(t *testing.T) {
	mh := newMockHarbor()
	r, id := provisioned(t, mh)
	before := secretPassword(t, r)
	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TrustPolicy.Audience = "other" })
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}

	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) {
		ha.Spec.TrustPolicy.Audience = r.Config.Audience
		ha.Spec.Permissions[0].Project = "staging"
	})
	r.Clock = fixedClock{testT0.Add(2 * time.Hour)}
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}

	robot := mh.robots[id]
	if robot.Disabled || RobotSuspended(robot.Description) || robot.Description != RobotDescription(testCluster, testHANamespace, testHAName) {
		t.Errorf("robot not resumed: disabled=%v description=%q", robot.Disabled, robot.Description)
	}
	if !harbor.PermissionsMatch(robot, []harbor.ProjectPermission{{Project: "staging", Action: "pull"}}) {
		t.Errorf("grants not converged on resume: %+v", robot.Permissions)
	}
	if len(mh.refreshCalls) != 1 {
		t.Errorf("refresh calls %v, want exactly one rotation on resume", mh.refreshCalls)
	}
	after := secretPassword(t, r)
	if after == before || after != mh.harborPassword(id) {
		t.Errorf("stored password %q (before suspension %q), Harbor accepts %q", after, before, mh.harborPassword(id))
	}
	assertCondition(t, getHA(t, r), harborv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
}

// A Secret present when the robot is resumed (its deletion during the
// suspension failed, or someone restored it) is not trusted: even when the
// pass that re-enables the robot fails before rotating, the next pass
// rotates, so a password from before the suspension never works again.
func TestReconcile_ResumeNeverRevivesAPasswordFromBeforeTheSuspension(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	id := mh.preexisting(testRobotName, SuspendedRobotDescription(testCluster, testHANamespace, testHAName),
		harbor.ProjectPermission{Project: "production", Action: "pull"})
	mh.robots[id].Disabled = true
	mh.robots[id].Secret = "leaked-before-suspension"
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testSecretKey.Name,
			Labels: robotsecret.Labels(testCluster, testHANamespace, testHAName),
			Annotations: map[string]string{
				robotsecret.AnnotationRobotID:           robotsecret.FormatRobotID(id),
				robotsecret.AnnotationRotationNotBefore: robotsecret.FormatRotationNotBefore(testT0.Add(20 * time.Hour)),
			}},
		Data: map[string][]byte{"username": []byte(mockRobotPrefix + testRobotName), "password": []byte("leaked-before-suspension")},
	}
	r := newReconciler(t, mh, fixedClock{testT0}, ha, stale)
	mh.errOnRefresh = errors.New("harbor went away")
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err == nil {
		t.Fatal("setup: the resume pass was expected to fail at the rotation")
	}
	if mh.robots[id].Disabled {
		t.Fatal("setup: the robot was not re-enabled before the rotation failed")
	}

	mh.errOnRefresh = nil
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	got := secretPassword(t, r)
	if got == "leaked-before-suspension" || got != mh.harborPassword(id) {
		t.Errorf("stored password %q, Harbor accepts %q: the pre-suspension password came back", got, mh.harborPassword(id))
	}
}

// A robot created with "*" before 0.5.5 cannot be disabled through
// Harbor's update: the update would have to send the "*" grant back. It
// is revoked instead, and its Secret is not served any more.
func TestReconcile_RefusedHarborAccessWithLegacyWildcardRobotRevokesIt(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.Permissions = []harborv1alpha1.ProjectPermission{{Project: "*", Action: harborv1alpha1.ActionPullPush}}
	mh := newMockHarbor()
	id := mh.preexisting(testRobotName, RobotDescription(testCluster, testHANamespace, testHAName),
		harbor.ProjectPermission{Project: "*", Action: "pull,push"})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testSecretKey.Name,
			Labels: robotsecret.Labels(testCluster, testHANamespace, testHAName)},
		Data: map[string][]byte{"username": []byte(mockRobotPrefix + testRobotName), "password": []byte("pre-0.5.5")},
	}
	r := newReconciler(t, mh, fixedClock{testT0}, ha, secret)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if _, ok := mh.robots[id]; ok {
		t.Error("every-project robot of a refused HarborAccess survived")
	}
	if !secretGone(t, r) {
		t.Error("the every-project robot's Secret survived")
	}
	assertCondition(t, getHA(t, r), harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonInvalidSpec)
}

// An administrator's disable is not the bridge's to undo (ADR-0023): a
// refused HarborAccess leaves it unmarked, so fixing the HarborAccess
// does not re-enable the robot.
func TestReconcile_RefusedHarborAccessLeavesAnAdministratorsDisableAlone(t *testing.T) {
	mh := newMockHarbor()
	r, id := provisioned(t, mh)
	mh.robots[id].Disabled = true
	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TrustPolicy.Audience = "other" })
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}
	if len(mh.updateCalls) != 0 || RobotSuspended(mh.robots[id].Description) {
		t.Fatalf("administrator-disabled robot marked as suspended: updates %+v", mh.updateCalls)
	}

	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TrustPolicy.Audience = r.Config.Audience })
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}
	if !mh.robots[id].Disabled {
		t.Error("the bridge re-enabled a robot an administrator disabled")
	}
	assertCondition(t, getHA(t, r), harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonRobotDisabled)
}

// A suspended robot re-enabled in Harbor while its HarborAccess is still
// refused is disabled again on the next pass.
func TestReconcile_RefusedHarborAccessDisablesARobotEnabledOutOfBand(t *testing.T) {
	mh := newMockHarbor()
	r, id := provisioned(t, mh)
	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TrustPolicy.Audience = "other" })
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}
	mh.robots[id].Disabled = false
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}
	if !mh.robots[id].Disabled {
		t.Error("suspended robot re-enabled in Harbor stays enabled")
	}
}

// Suspension touches only what the refused HarborAccess owns: the robot of
// another HarborAccess for the same ServiceAccount, and a Secret the bridge
// does not own, are left alone.
func TestReconcile_RefusedHarborAccessSuspendsOnlyWhatItOwns(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.TrustPolicy.Audience = "other"
	mh := newMockHarbor()
	id := mh.preexisting(testRobotName, RobotDescription(testCluster, "team", "owner"),
		harbor.ProjectPermission{Project: "production", Action: "pull"})
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testSecretKey.Name},
		Data:       map[string][]byte{"username": []byte("someone-else"), "password": []byte("theirs")},
	}
	r := newReconciler(t, mh, fixedClock{testT0}, ha, foreign)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.updateCalls)+len(mh.deleteCalls) != 0 || mh.robots[id].Disabled {
		t.Errorf("another HarborAccess's robot touched: updates %+v deletes %v", mh.updateCalls, mh.deleteCalls)
	}
	if secretGone(t, r) {
		t.Error("deleted a Secret the bridge does not own")
	}
}

// When Harbor cannot be reached, the condition still names the refusal,
// says the suspension failed, and the pass is retried.
func TestReconcile_RefusedHarborAccessRetriesAFailedSuspension(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.TrustPolicy.Audience = "other"
	mh := newMockHarbor()
	mh.errOnGetByName = map[string]error{testRobotName: errors.New("harbor unreachable")}
	r := newReconciler(t, mh, fixedClock{testT0}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err == nil {
		t.Fatal("a failed suspension must be retried with backoff")
	}
	got := getHA(t, r)
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonAudienceMismatch)
	if c := meta.FindStatusCondition(got.Status.Conditions, harborv1alpha1.ConditionReady); c == nil || !strings.Contains(c.Message, "suspending its robot failed") {
		t.Errorf("condition does not say the suspension failed: %+v", c)
	}
}

// A HarborAccess refused before it ever had a robot causes no Harbor write.
func TestReconcile_RefusedHarborAccessWithoutARobotWritesNothing(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.TrustPolicy.Audience = "other"
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{testT0}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if n := len(mh.createCalls) + len(mh.updateCalls) + len(mh.deleteCalls) + len(mh.refreshCalls); n != 0 {
		t.Errorf("%d Harbor writes for a HarborAccess that never had a robot", n)
	}
}
