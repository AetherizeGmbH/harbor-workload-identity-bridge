// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"net/url"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

func newJanitor(t *testing.T, mh harbor.Client, objects ...client.Object) *Janitor {
	t.Helper()
	issuer, _ := url.Parse(testIssuer)
	harborURL, _ := url.Parse(testHarbor)
	c := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objects...).
		Build()
	return &Janitor{
		Client: c,
		Harbor: mh,
		Config: &Config{
			ClusterName:    testCluster,
			Namespace:      testNS,
			OIDCIssuer:     issuer,
			HarborURL:      harborURL,
			HarborAdminDir: "/dev/null",
		},
	}
}

func TestJanitor_DeletesOrphanRobot(t *testing.T) {
	// A robot owned by this bridge whose HarborAccess has been deleted.
	mh := newMockHarbor()
	orphanID := mh.preexisting(
		"bridge-prod-eu-west.flux-system.orphan",
		RobotDescription(testCluster, "harbor-bridge-system", "long-gone-cr"),
	)
	j := newJanitor(t, mh) // no HarborAccess objects in the cluster

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(mh.deleteCalls) != 1 || mh.deleteCalls[0] != orphanID {
		t.Errorf("expected one Delete call for id=%d; got %v", orphanID, mh.deleteCalls)
	}
	if _, ok := mh.robots[orphanID]; ok {
		t.Errorf("orphan robot still present in mock state")
	}
}

func TestJanitor_PreservesRobotWithLiveCR(t *testing.T) {
	ha := newHarborAccess() // namespace=harbor-bridge-system, name=flux-access
	mh := newMockHarbor()
	liveID := mh.preexisting(
		"bridge-prod-eu-west.flux-system.source-controller",
		RobotDescription(testCluster, ha.Namespace, ha.Name),
	)
	j := newJanitor(t, mh, ha)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mh.deleteCalls) != 0 {
		t.Errorf("Delete called on robot with live CR: %v", mh.deleteCalls)
	}
	if _, ok := mh.robots[liveID]; !ok {
		t.Errorf("live robot disappeared")
	}
}

func TestJanitor_PreservesForeignClusterRobot(t *testing.T) {
	// Robot whose name IS in our ownership prefix ("bridge-prod-eu-west.")
	// but whose description marks it as another cluster's. Layer 2
	// (RobotBelongsToCluster) must keep us from touching it even though its
	// owning HarborAccess doesn't exist in our cluster (it lives in the other
	// cluster's apiserver, which we cannot see).
	mh := newMockHarbor()
	foreignDesc := RobotDescription("prod-eu-west-other", "ns", "cr")
	foreignID := mh.preexisting("bridge-prod-eu-west.ns.cr", foreignDesc)
	j := newJanitor(t, mh) // empty cluster — but we still must not touch it

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mh.deleteCalls) != 0 {
		t.Errorf("foreign robot deleted! id=%d deleteCalls=%v", foreignID, mh.deleteCalls)
	}
}

func TestJanitor_PreservesUnmarkedRobotInOurPrefix(t *testing.T) {
	// A robot that happens to share our prefix but was not created by the
	// bridge (no managed-by marker). Must never be deleted.
	mh := newMockHarbor()
	id := mh.preexisting("bridge-prod-eu-west.handmade", "manually created by ops, do not touch")
	j := newJanitor(t, mh)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mh.deleteCalls) != 0 {
		t.Errorf("unmarked robot deleted: id=%d deleteCalls=%v", id, mh.deleteCalls)
	}
}

func TestJanitor_IgnoresRobotsOutsideOurPrefix(t *testing.T) {
	// Robots whose names don't start with our cluster prefix are not even
	// examined for deletion.
	mh := newMockHarbor()
	mh.preexisting("bridge-other-cluster-flux-system-source-controller", "some description")
	mh.preexisting("robot-named-by-someone-else", "")
	j := newJanitor(t, mh)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mh.deleteCalls) != 0 {
		t.Errorf("foreign-prefix robot deleted: %v", mh.deleteCalls)
	}
}

func TestJanitor_HandlesMixedRobotPopulation(t *testing.T) {
	// Realistic scenario: 1 live robot, 1 orphan (ours), 1 foreign-cluster
	// robot, 1 unrelated robot. Only the orphan should be deleted.
	ha := newHarborAccess()
	mh := newMockHarbor()
	live := mh.preexisting(
		"bridge-prod-eu-west.flux-system.source-controller",
		RobotDescription(testCluster, ha.Namespace, ha.Name),
	)
	orphan := mh.preexisting(
		"bridge-prod-eu-west.gone.cr",
		RobotDescription(testCluster, "some-ns", "deleted-cr"),
	)
	foreign := mh.preexisting(
		"bridge-prod-eu-west.impostor.cr",
		RobotDescription("not-our-cluster", "ns", "cr"),
	)
	unrelated := mh.preexisting("operator-handmade", "manually")
	j := newJanitor(t, mh, ha)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mh.deleteCalls) != 1 || mh.deleteCalls[0] != orphan {
		t.Errorf("expected exactly one Delete for id=%d (orphan); got %v", orphan, mh.deleteCalls)
	}
	for _, id := range []int64{live, foreign, unrelated} {
		if _, ok := mh.robots[id]; !ok {
			t.Errorf("robot id=%d wrongly deleted", id)
		}
	}
}

// TestJanitor_RevokesRobotOfPreviousServiceAccount is the backstop half of
// audit H2: the owner still exists but its serviceAccountRef now maps to a
// different robot, so this one belongs to the previous identity.
func TestJanitor_RevokesRobotOfPreviousServiceAccount(t *testing.T) {
	ha := newHarborAccess() // SA flux-system/source-controller
	mh := newMockHarbor()
	desc := RobotDescription(testCluster, ha.Namespace, ha.Name)
	currentID := mh.preexisting("bridge-prod-eu-west.flux-system.source-controller", desc)
	staleID := mh.preexisting("bridge-prod-eu-west.flux-system.previous-sa", desc)
	j := newJanitor(t, mh, ha)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := mh.robots[staleID]; ok {
		t.Error("robot of the previous serviceAccountRef survived")
	}
	if _, ok := mh.robots[currentID]; !ok {
		t.Error("the robot the owner currently maps to was deleted")
	}
}

// TestJanitor_RevokesLegacyDashNamedRobots covers upgrades from 0.2.x,
// whose dash-named robots the dot-prefix ownership check never matched:
// they leaked with valid passwords. Recognised via the legacy prefix plus
// an exact cluster tag — and never another cluster's.
func TestJanitor_RevokesLegacyDashNamedRobots(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	liveOwnerLegacy := mh.preexisting("bridge-prod-eu-west-flux-system-source-controller",
		RobotDescription(testCluster, ha.Namespace, ha.Name))
	goneOwnerLegacy := mh.preexisting("bridge-prod-eu-west-team-gone",
		RobotDescription(testCluster, "team", "gone"))
	// Cluster "prod-eu-west-2" robots start with "bridge-prod-eu-west-"
	// too; the cluster tag must keep them out.
	otherCluster := mh.preexisting("bridge-prod-eu-west-2-team-x",
		RobotDescription("prod-eu-west-2", "team", "gone"))
	j := newJanitor(t, mh, ha)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := mh.robots[liveOwnerLegacy]; ok {
		t.Error("legacy robot of a live HarborAccess survived (it is superseded by the dot-named robot)")
	}
	if _, ok := mh.robots[goneOwnerLegacy]; ok {
		t.Error("legacy orphan survived")
	}
	if _, ok := mh.robots[otherCluster]; !ok {
		t.Error("deleted another cluster's robot that merely shares the legacy dash prefix")
	}
}

// TestJanitor_UsesLiveReaderForOwners pins the race fix: the owner is read
// through Reader (the uncached API reader in production), never the cache.
func TestJanitor_UsesLiveReaderForOwners(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.ServiceAccountRef.Name = "new-sa" // live spec: already moved on
	stale := newHarborAccess()                // cached spec: still the old SA
	mh := newMockHarbor()
	desc := RobotDescription(testCluster, ha.Namespace, ha.Name)
	newID := mh.preexisting("bridge-prod-eu-west.flux-system.new-sa", desc)

	j := newJanitor(t, mh, stale)
	j.Reader = fake.NewClientBuilder().WithScheme(testScheme).WithObjects(ha).Build()

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := mh.robots[newID]; !ok {
		t.Fatal("janitor acted on the stale cached spec and deleted the robot of the live serviceAccountRef")
	}
}

func TestJanitor_DeletesOrphanRobotSecrets(t *testing.T) {
	ha := newHarborAccess()
	live := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNS, Name: robotsecret.Name(ha.Namespace, ha.Name),
		Labels: robotsecret.Labels(testCluster, ha.Namespace, ha.Name)}}
	orphan := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNS, Name: robotsecret.Name("team", "gone"),
		Labels: robotsecret.Labels(testCluster, "team", "gone")}}
	otherCluster := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNS, Name: "robot-x.y",
		Labels: robotsecret.Labels("another-cluster", "team", "gone")}}
	unmanaged := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "harbor-admin"}}
	j := newJanitor(t, newMockHarbor(), ha, live, orphan, otherCluster, unmanaged)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	exists := func(s *corev1.Secret) bool {
		return j.Client.Get(context.Background(), client.ObjectKeyFromObject(s), &corev1.Secret{}) == nil
	}
	if exists(orphan) {
		t.Error("orphan robot Secret survived")
	}
	for _, s := range []*corev1.Secret{live, otherCluster, unmanaged} {
		if !exists(s) {
			t.Errorf("Secret %s must not be deleted", s.Name)
		}
	}
}
