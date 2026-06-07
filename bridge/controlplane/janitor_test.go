// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"net/url"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
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
