// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// countingReader counts the janitor's reads of HarborAccess objects and
// can make Get see a different state than List, as happens when an object
// changes between two reads.
type countingReader struct {
	client.Reader
	mu          sync.Mutex
	gets, lists int
	onGet       func(obj client.Object)
}

func (c *countingReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	if err := c.Reader.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if c.onGet != nil {
		c.onGet(obj)
	}
	return nil
}

func (c *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.mu.Lock()
	c.lists++
	c.mu.Unlock()
	return c.Reader.List(ctx, list, opts...)
}

func managedSecret(name, haNS, haName string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: name,
		Labels: robotsecret.Labels(testCluster, haNS, haName)}}
}

// One uncached List of the owners serves the whole sweep; an owner is read
// individually only right before something of it is deleted.
func TestJanitor_ListsOwnersOnceAndReadsLiveOnlyBeforeDeleting(t *testing.T) {
	mh := newMockHarbor()
	var objs []client.Object
	for _, name := range []string{"a", "b", "c"} {
		ha := newHarborAccess()
		ha.Name = name
		ha.Spec.ServiceAccountRef.Name = "sa-" + name
		objs = append(objs, ha, managedSecret(robotsecret.Name(ha.Namespace, name), ha.Namespace, name))
		mh.preexisting("bridge-prod-eu-west.flux-system.sa-"+name, RobotDescription(testCluster, ha.Namespace, name))
	}
	gone := mh.preexisting("bridge-prod-eu-west.flux-system.gone", RobotDescription(testCluster, testHANamespace, "gone"))
	j := newJanitor(t, mh, objs...)
	reader := &countingReader{Reader: j.Client}
	j.Reader = reader

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := mh.robots[gone]; ok {
		t.Error("robot of a deleted HarborAccess survived")
	}
	if len(mh.robots) != 3 {
		t.Errorf("robots left %d, want the 3 in use", len(mh.robots))
	}
	if reader.lists != 1 || reader.gets != 1 {
		t.Errorf("owner reads: %d lists, %d gets; want 1 list and 1 get (before the one deletion)", reader.lists, reader.gets)
	}
}

// The owner list can be older than a change that makes a robot current
// again; the live read right before the deletion saves it.
func TestJanitor_ReReadsTheOwnerBeforeDeleting(t *testing.T) {
	ha := newHarborAccess()
	ha.Spec.ServiceAccountRef.Name = "old-sa" // listed: already moved on
	mh := newMockHarbor()
	id := mh.preexisting(testRobotName, RobotDescription(testCluster, ha.Namespace, ha.Name))
	j := newJanitor(t, mh, ha)
	j.Reader = &countingReader{Reader: j.Client, onGet: func(obj client.Object) {
		// live: moved back to the SA the robot belongs to
		obj.(*harborv1alpha1.HarborAccess).Spec.ServiceAccountRef.Name = testSAName
	}}
	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := mh.robots[id]; !ok {
		t.Error("deleted a robot its owner uses again, judging from a stale list")
	}
}

// The Secret sweep needs no Harbor: while Harbor is unreachable it still
// removes the Secrets of deleted HarborAccess objects. Finalizers are not
// released, because nobody can tell whether a robot is left.
func TestJanitor_SweepsSecretsWhileHarborIsDown(t *testing.T) {
	moved := newHarborAccess()
	moved.Finalizers = []string{FinalizerName + "-bridge-a"}
	orphan := managedSecret(robotsecret.Name("team", "gone"), "team", "gone")
	mh := newMockHarbor()
	mh.errOnList = errors.New("harbor unreachable")
	j := newJanitor(t, mh, moved, orphan)
	selective(j.Config)

	if err := j.Sweep(context.Background()); err == nil {
		t.Error("an unreachable Harbor must be reported")
	}
	if err := j.Client.Get(context.Background(), client.ObjectKeyFromObject(orphan), &corev1.Secret{}); err == nil {
		t.Error("orphan robot Secret survived while Harbor was down")
	}
	got := &harborv1alpha1.HarborAccess{}
	if err := j.Client.Get(context.Background(), client.ObjectKeyFromObject(moved), got); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(got, FinalizerName+"-bridge-a") {
		t.Error("finalizer released without knowing whether a robot is left")
	}
}

// A managed Secret that is not at the name its HarborAccess uses is never
// read (0.2.x wrote dash-named ones with the same labels) and goes; the
// Secret at the right name and unmanaged Secrets stay.
func TestJanitor_DeletesRobotSecretsAtANameTheirOwnerDoesNotUse(t *testing.T) {
	ha := newHarborAccess()
	current := managedSecret(robotsecret.Name(ha.Namespace, ha.Name), ha.Namespace, ha.Name)
	legacy := managedSecret(robotsecret.NamePrefix+ha.Namespace+"-"+ha.Name, ha.Namespace, ha.Name)
	unmanaged := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: robotsecret.NamePrefix + "team-x"}}
	j := newJanitor(t, newMockHarbor(), ha, current, legacy, unmanaged)

	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	exists := func(s *corev1.Secret) bool {
		return j.Client.Get(context.Background(), client.ObjectKeyFromObject(s), &corev1.Secret{}) == nil
	}
	if exists(legacy) {
		t.Error("dash-named robot Secret of a live HarborAccess survived")
	}
	if !exists(current) || !exists(unmanaged) {
		t.Errorf("deleted a Secret in use or not the bridge's: current=%v unmanaged=%v", exists(current), exists(unmanaged))
	}
}

// harborListSequence serves successive robot listings from a script, so a
// robot can appear between two listings of one sweep.
type harborListSequence struct {
	*mockHarbor
	lists [][]harbor.Robot
}

func (h *harborListSequence) List(ctx context.Context) ([]harbor.Robot, error) {
	if len(h.lists) == 0 {
		return h.mockHarbor.List(ctx)
	}
	next := h.lists[0]
	h.lists = h.lists[1:]
	return next, nil
}

// A finalizer promises that the robot is revoked before the object goes.
// The janitor must not release it while a robot of the object is left:
// neither one it kept because the object was still selected when it was
// judged, nor one created after the first robot listing.
func TestJanitor_KeepsTheFinalizerWhileARobotIsLeft(t *testing.T) {
	newMoved := func() *harborv1alpha1.HarborAccess {
		ha := newHarborAccess()
		ha.Finalizers = []string{FinalizerName + "-bridge-a"}
		return ha
	}
	robotOf := func(ha *harborv1alpha1.HarborAccess) harbor.Robot {
		return harbor.Robot{ID: 100, Name: testRobotName, WireName: mockRobotPrefix + testRobotName,
			Description: RobotDescription(testCluster, ha.Namespace, ha.Name)}
	}
	finalizerKept := func(t *testing.T, j *Janitor, ha *harborv1alpha1.HarborAccess) {
		t.Helper()
		got := &harborv1alpha1.HarborAccess{}
		if err := j.Client.Get(context.Background(), client.ObjectKeyFromObject(ha), got); err != nil {
			t.Fatal(err)
		}
		if !controllerutil.ContainsFinalizer(got, FinalizerName+"-bridge-a") {
			t.Error("finalizer released while a robot of the object is left in Harbor")
		}
	}

	t.Run("relabelled between the reads", func(t *testing.T) {
		moved := newMoved() // listed unselected
		mh := newMockHarbor()
		mh.preexisting(testRobotName, RobotDescription(testCluster, moved.Namespace, moved.Name))
		j := newJanitor(t, mh, moved)
		selective(j.Config)
		j.Reader = &countingReader{Reader: j.Client, onGet: func(obj client.Object) {
			obj.SetLabels(map[string]string{"harbor.aetherize.io/bridge": "a"}) // live: still selected
		}}
		if err := j.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(mh.deleteCalls) != 0 {
			t.Fatalf("setup: robot deleted although its owner reads as selected: %v", mh.deleteCalls)
		}
		finalizerKept(t, j, moved)
	})

	t.Run("robot created after the first listing", func(t *testing.T) {
		moved := newMoved()
		mh := &harborListSequence{mockHarbor: newMockHarbor(), lists: [][]harbor.Robot{nil, {robotOf(moved)}}}
		j := newJanitor(t, mh, moved)
		selective(j.Config)
		if err := j.Sweep(context.Background()); err != nil {
			t.Fatal(err)
		}
		finalizerKept(t, j, moved)
	})
}
