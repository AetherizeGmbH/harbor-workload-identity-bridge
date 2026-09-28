// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// legacyTokenTTL decodes raw the way the bridge reads a tokenTTL the CRD
// admitted before it pinned Go duration syntax.
func legacyTokenTTL(t *testing.T, raw string) harborv1alpha1.Duration {
	t.Helper()
	var d harborv1alpha1.Duration
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	if d.Err() == nil {
		t.Fatalf("tokenTTL %s decoded as valid; the test needs an invalid one", raw)
	}
	return d
}

// A tokenTTL the CRD admitted before it pinned Go duration syntax (e.g.
// "1d") decodes without an error; the reconciler refuses it as InvalidSpec
// instead of provisioning a robot, and re-checks it every resync like any
// refused object (ADR-0030).
func TestReconcile_ReportsTokenTTLGoCannotParse(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.TokenTTL = legacyTokenTTL(t, `"1d"`)
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Now()}, ha)
	res, err := r.Reconcile(context.Background(), reqFor(ha))
	if err != nil {
		t.Fatal(err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > ResyncInterval {
		t.Errorf("RequeueAfter = %s, want (0, %s]", res.RequeueAfter, ResyncInterval)
	}
	if len(mh.createCalls) != 0 {
		t.Error("robot created for a HarborAccess with an unparseable tokenTTL")
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := r.Get(context.Background(), reqFor(ha).NamespacedName, got); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, got, harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonInvalidSpec)
	assertCondition(t, got, harborv1alpha1.ConditionRobotProvisioned, metav1.ConditionFalse, ReasonInvalidSpec)
	if c := meta.FindStatusCondition(got.Status.Conditions, harborv1alpha1.ConditionReady); c == nil ||
		!strings.Contains(c.Message, `spec.tokenTTL: "1d"`) {
		t.Errorf("Ready condition = %+v, want its message to name spec.tokenTTL and the value", c)
	}
}

// A served HarborAccess whose tokenTTL became a value Go cannot parse (the
// CRD before 0.10.1 admitted "1d" on update) has its robot suspended and
// its Secret deleted (ADR-0030), so the password handed out before stops
// working. Correcting the value resumes the robot with a new password.
func TestReconcile_FixedTokenTTLResumesTheSuspendedRobot(t *testing.T) {
	mh := newMockHarbor()
	r, id := provisioned(t, mh)
	before := secretPassword(t, r)

	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TokenTTL = legacyTokenTTL(t, `"1d"`) })
	r.Clock = fixedClock{testT0.Add(time.Hour)}
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}
	if robot := mh.robots[id]; robot == nil || !robot.Disabled || !RobotSuspended(robot.Description) {
		t.Fatalf("robot of a HarborAccess refused for its tokenTTL not suspended: %+v", robot)
	}
	if !secretGone(t, r) {
		t.Error("robot Secret of a HarborAccess refused for its tokenTTL survived")
	}
	if len(mh.refreshCalls) != 0 {
		t.Errorf("refused HarborAccess rotated: %v", mh.refreshCalls)
	}
	if msg := provisionedMessage(t, r); !strings.Contains(msg, "is suspended (disabled)") {
		t.Errorf("RobotProvisioned message %q does not say the robot is suspended", msg)
	}

	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TokenTTL = harborv1alpha1.Duration{Duration: time.Hour} })
	r.Clock = fixedClock{testT0.Add(2 * time.Hour)}
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}
	robot := mh.robots[id]
	if robot.Disabled || RobotSuspended(robot.Description) {
		t.Errorf("robot not resumed after the tokenTTL fix: disabled=%v description=%q", robot.Disabled, robot.Description)
	}
	if len(mh.refreshCalls) != 1 {
		t.Errorf("refresh calls %v, want exactly one rotation on resume", mh.refreshCalls)
	}
	if after := secretPassword(t, r); after == before || after != mh.harborPassword(id) {
		t.Errorf("stored password %q (before the suspension %q), Harbor accepts %q", after, before, mh.harborPassword(id))
	}
	assertCondition(t, getHA(t, r), harborv1alpha1.ConditionReady, metav1.ConditionTrue, ReasonReconcileSucceeded)
}

// A robot whose grants the bridge cannot write back (a pre-0.5.5 "*" that
// was never converged) cannot be disabled through Harbor's update, which
// replaces the grants with the ones sent: the tokenTTL refusal deletes it
// instead (ADR-0030 §2), and its Secret with it.
func TestReconcile_LegacyTokenTTLRevokesARobotItCannotDisable(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.TokenTTL = legacyTokenTTL(t, `"1d"`)
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
		t.Error("every-project robot of a HarborAccess refused for its tokenTTL survived")
	}
	if !secretGone(t, r) {
		t.Error("the every-project robot's Secret survived")
	}
	assertCondition(t, getHA(t, r), harborv1alpha1.ConditionReady, metav1.ConditionFalse, ReasonInvalidSpec)
	if msg := provisionedMessage(t, r); !strings.Contains(msg, "was deleted") {
		t.Errorf("RobotProvisioned message %q does not say the robot was deleted", msg)
	}
}
