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
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// provisionedMessage returns the RobotProvisioned condition's message.
func provisionedMessage(t *testing.T, r *Reconciler) string {
	t.Helper()
	c := meta.FindStatusCondition(getHA(t, r).Status.Conditions, harborv1alpha1.ConditionRobotProvisioned)
	if c == nil {
		t.Fatal("no RobotProvisioned condition")
	}
	return c.Message
}

func TestReconcile_RefusedHarborAccessSuspendsItsRobot(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		edit         func(*harborv1alpha1.HarborAccess)
	}{
		{"AudienceMismatch", ReasonAudienceMismatch, func(ha *harborv1alpha1.HarborAccess) {
			ha.Spec.TrustPolicy.Audience = "https://kubernetes.default.svc"
		}},
		{"IssuerMismatch", ReasonIssuerMismatch, func(ha *harborv1alpha1.HarborAccess) {
			ha.Spec.TrustPolicy.Issuer = "https://other-cluster.example.com"
		}},
		{"InvalidSpec", ReasonInvalidSpec, func(ha *harborv1alpha1.HarborAccess) {
			ha.Spec.Permissions = append(ha.Spec.Permissions, harborv1alpha1.ProjectPermission{Project: "*", Action: harborv1alpha1.ActionPullPush})
		}},
		// A tokenTTL an older CRD admitted and Go cannot parse.
		{"InvalidSpecTokenTTL", ReasonInvalidSpec, func(ha *harborv1alpha1.HarborAccess) {
			ha.Spec.TokenTTL = legacyTokenTTL(t, `"1d"`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if msg := provisionedMessage(t, r); !strings.Contains(msg, "is suspended (disabled)") {
				t.Errorf("RobotProvisioned message %q does not say the robot is suspended", msg)
			}

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

// revivalWatch fails a test that re-enables a robot while Harbor still
// holds the password it had before its suspension.
type revivalWatch struct {
	*mockHarbor
	oldSecret string
	revived   []int64
}

func (w *revivalWatch) Update(ctx context.Context, current *harbor.Robot, description string, perms []harbor.ProjectPermission) error {
	w.mu.Lock()
	stored, ok := w.robots[current.ID]
	if ok && stored.Disabled && !current.Disabled && stored.Secret == w.oldSecret {
		w.revived = append(w.revived, current.ID)
	}
	w.mu.Unlock()
	return w.mockHarbor.Update(ctx, current, description, perms)
}

// Resuming never re-enables the robot with the password it had before the
// suspension, whatever step a pass fails at: the password is rotated while
// the robot is still disabled. A Secret present when the robot is resumed
// (its deletion during the suspension failed, or someone restored it) is
// not trusted either.
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
	watch := &revivalWatch{mockHarbor: mh, oldSecret: "leaked-before-suspension"}
	r := newReconciler(t, watch, fixedClock{testT0}, ha, stale)

	stillSuspended := func(step string) {
		t.Helper()
		if robot := mh.robots[id]; !robot.Disabled || !RobotSuspended(robot.Description) {
			t.Errorf("%s: robot disabled=%v description=%q; a failed resume must leave it suspended", step, robot.Disabled, robot.Description)
		}
	}

	// The rotation fails: the robot stays suspended.
	mh.errOnRefresh = errors.New("harbor went away")
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err == nil {
		t.Fatal("the resume pass was expected to fail at the rotation")
	}
	stillSuspended("rotation failed")
	if !secretGone(t, r) {
		t.Error("the Secret holding the pre-suspension password survived the resume")
	}

	// The rotation succeeds, the enabling update fails: still suspended,
	// and the next pass resumes again.
	mh.errOnRefresh, mh.errOnUpdate = nil, errors.New("harbor went away")
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err == nil {
		t.Fatal("the resume pass was expected to fail at the enabling update")
	}
	stillSuspended("enabling update failed")

	mh.errOnUpdate = nil
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if mh.robots[id].Disabled {
		t.Fatal("robot not resumed")
	}
	if len(watch.revived) != 0 {
		t.Errorf("robot %v re-enabled while Harbor still accepted the password from before the suspension", watch.revived)
	}
	got := secretPassword(t, r)
	if got == "leaked-before-suspension" || got != mh.harborPassword(id) {
		t.Errorf("stored password %q, Harbor accepts %q: the pre-suspension password came back", got, mh.harborPassword(id))
	}
}

// A pass that re-enabled the robot and failed to store the new password
// leaves no Secret, so the next pass rotates again instead of trusting a
// Secret from before the suspension.
func TestReconcile_ResumeThatFailsToStoreThePasswordRotatesAgain(t *testing.T) {
	mh := newMockHarbor()
	r, id := provisioned(t, mh)
	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TrustPolicy.Audience = "other" })
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}
	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) { ha.Spec.TrustPolicy.Audience = r.Config.Audience })

	fail := true
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*corev1.Secret); ok && fail {
				return errors.New("apiserver went away")
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err == nil {
		t.Fatal("the resume pass was expected to fail at the Secret write")
	}
	if mh.robots[id].Disabled {
		t.Fatal("setup: the robot was expected to be re-enabled before the Secret write failed")
	}
	fail = false
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err != nil {
		t.Fatal(err)
	}
	if len(mh.refreshCalls) != 2 {
		t.Errorf("refresh calls %v, want two: the password of the failed pass was never stored", mh.refreshCalls)
	}
	if got := secretPassword(t, r); got != mh.harborPassword(id) {
		t.Errorf("stored password %q, Harbor accepts %q", got, mh.harborPassword(id))
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
	if msg := provisionedMessage(t, r); !strings.Contains(msg, "was deleted") {
		t.Errorf("RobotProvisioned message %q does not say the robot was deleted", msg)
	}
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
	if msg := provisionedMessage(t, r); !strings.Contains(msg, "by an administrator") {
		t.Errorf("RobotProvisioned message %q does not say an administrator disabled the robot", msg)
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
	if msg := provisionedMessage(t, r); !strings.Contains(msg, "has no robot") {
		t.Errorf("RobotProvisioned message %q does not say there is no robot", msg)
	}
}

// The robot Secret goes even when Harbor cannot be asked about the robot:
// deleting it needs only the apiserver, and the data plane still matches
// an InvalidSpec HarborAccess (such as one granting "*") and would keep
// handing the password out for as long as Harbor's API fails, for example
// with broken bridge admin credentials while robot logins still work.
func TestReconcile_RefusedHarborAccessDeletesItsSecretWhenHarborFails(t *testing.T) {
	mh := newMockHarbor()
	r, id := provisioned(t, mh)
	editHA(t, r, func(ha *harborv1alpha1.HarborAccess) {
		ha.Spec.Permissions = append(ha.Spec.Permissions, harborv1alpha1.ProjectPermission{Project: "*", Action: harborv1alpha1.ActionPullPush})
	})
	mh.errOnGetByName = map[string]error{testRobotName: errors.New("harbor 401")}
	if _, err := r.Reconcile(context.Background(), reqFor(newHarborAccess())); err == nil {
		t.Fatal("a failed suspension must be retried with backoff")
	}
	if !secretGone(t, r) {
		t.Error("robot Secret survived a failed Harbor lookup; the data plane could keep serving it")
	}
	if mh.robots[id].Disabled {
		t.Fatal("setup: the robot cannot have been disabled without a lookup")
	}
	if msg := provisionedMessage(t, r); !strings.Contains(msg, "suspending the robot failed") || strings.Contains(msg, "is suspended") {
		t.Errorf("RobotProvisioned message %q must say the suspension failed, not that the robot is suspended", msg)
	}
}

// Two bridges with different clusterName values that share a namespace
// write the same robot-Secret names. The bridge that refuses a HarborAccess
// (it names the other bridge's audience) must leave the other bridge's
// Secret alone: deleting it would make that bridge rotate and rebuild it,
// and the rebuilt Secret's event would bring the deletion back, one Harbor
// rotation per round.
func TestReconcile_RefusedHarborAccessLeavesAnotherClustersSecretAlone(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.TrustPolicy.Audience = "audience-b"
	mhA, mhB := newMockHarbor(), newMockHarbor()
	bridgeA := newReconciler(t, mhA, fixedClock{testT0}, ha)
	bridgeA.Config.ClusterName, bridgeA.Config.Audience = "cluster-a", "audience-a"
	cfgB := *bridgeA.Config
	cfgB.ClusterName, cfgB.Audience = "cluster-b", "audience-b"
	bridgeB := &Reconciler{Client: bridgeA.Client, Scheme: bridgeA.Scheme, Harbor: mhB, Config: &cfgB, Clock: bridgeA.Clock}

	if _, err := bridgeB.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	for round := range 3 {
		if _, err := bridgeA.Reconcile(context.Background(), reqFor(ha)); err != nil {
			t.Fatalf("round %d, bridge A: %v", round, err)
		}
		if secretGone(t, bridgeA) {
			t.Fatalf("round %d: the refusing bridge deleted the Secret the other bridge serves", round)
		}
		if _, err := bridgeB.Reconcile(context.Background(), reqFor(ha)); err != nil {
			t.Fatalf("round %d, bridge B: %v", round, err)
		}
	}
	if len(mhB.refreshCalls) != 0 {
		t.Errorf("bridge B rotated %v: its Secret was taken from it", mhB.refreshCalls)
	}
}

// Refused objects are re-checked on the per-object resync (resyncAfter),
// not all at once an interval after the bridge started.
func TestReconcile_RefusedHarborAccessResyncIsSpreadPerObject(t *testing.T) {
	requeue := func(uid types.UID) time.Duration {
		t.Helper()
		ha := newHarborAccess()
		ha.UID = uid
		ha.Spec.TrustPolicy.Audience = "other"
		r := newReconciler(t, newMockHarbor(), fixedClock{testT0}, ha)
		res, err := r.Reconcile(context.Background(), reqFor(ha))
		if err != nil {
			t.Fatal(err)
		}
		return res.RequeueAfter
	}
	if a, b := requeue("uid-a"), requeue("uid-b"); a == b || a < ResyncInterval*9/10 || b < ResyncInterval*9/10 {
		t.Errorf("RequeueAfter %s and %s: want two different values within the last tenth of %s", a, b, ResyncInterval)
	}
}
