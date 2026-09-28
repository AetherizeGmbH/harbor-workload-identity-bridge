// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/nexussecret"
)

// janitorFor returns a NexusJanitor sharing the harness's apiserver,
// Nexus client and clock.
func (h *nexusHarness) janitor() *NexusJanitor {
	return &NexusJanitor{Client: h.r.Client, Nexus: h.r.Nexus, Config: h.r.Config, Clock: h.clock}
}

// putOwnedUser stores a user carrying the markers of the NexusAccess
// ns/name in this cluster.
func (h *nexusHarness) putOwnedUser(id, ns, name string, status nexus.UserStatus) {
	h.nx.PutUser(nexus.User{UserID: id, Status: status, EmailAddress: nexusUserEmail,
		FirstName: NexusUserFirstName(testCluster), LastName: NexusUserLastName(ns, name)}, "pw")
}

func TestNexusJanitor_SweepsUsersAndRolesNobodyUses(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	current, _ := h.storedAuthenticates()

	gone := "bridge-" + testCluster + ".gone.sa_0123456789abcdef"
	h.putOwnedUser(gone, "tenant", "gone", nexus.UserActive)
	h.nx.PutRole(nexus.Role{ID: "bridge-" + testCluster + ".gone.sa", Name: "x", Description: NexusRoleDescription(testCluster, "tenant", "gone")})
	oldIdentity := "bridge-" + testCluster + ".old.sa_0123456789abcdef"
	h.putOwnedUser(oldIdentity, testNXANamespace, testNXAName, nexus.UserDisabled)
	h.nx.PutRole(nexus.Role{ID: "bridge-" + testCluster + ".old.sa", Name: "x", Description: NexusRoleDescription(testCluster, testNXANamespace, testNXAName)})
	unnamed := testIdentity + "_00000000000000aa"
	h.putOwnedUser(unnamed, testNXANamespace, testNXAName, nexus.UserActive)

	// Never touched: another cluster whose name extends ours, another
	// cluster's tag on our prefix, a user without markers, a user whose id
	// is not one the bridge builds.
	otherCluster := "bridge-" + testCluster + "-x.a.b_0123456789abcdef"
	h.nx.PutUser(nexus.User{UserID: otherCluster, Status: nexus.UserActive,
		FirstName: NexusUserFirstName(testCluster + "-x"), LastName: NexusUserLastName("tenant", "gone")}, "pw")
	foreignTag := "bridge-" + testCluster + ".c.d_0123456789abcdef"
	h.nx.PutUser(nexus.User{UserID: foreignTag, Status: nexus.UserActive,
		FirstName: NexusUserFirstName("prod"), LastName: NexusUserLastName("tenant", "gone")}, "pw")
	handMade := "bridge-" + testCluster + ".e.f_0123456789abcdef"
	h.nx.PutUser(nexus.User{UserID: handMade, Status: nexus.UserActive, FirstName: "Jane", LastName: "Doe"}, "pw")
	notOurShape := "bridge-" + testCluster + ".g.h"
	h.putOwnedUser(notOurShape, "tenant", "gone", nexus.UserActive)

	if err := h.janitor().Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{otherCluster, current, foreignTag, handMade, notOurShape}
	slices.Sort(want)
	if got := h.userIDs(); !slices.Equal(got, want) {
		t.Errorf("users after the sweep = %v, want %v", got, want)
	}
	var roles []string
	for _, r := range h.nx.Roles() {
		roles = append(roles, r.ID)
	}
	if !slices.Equal(roles, []string{testIdentity}) {
		t.Errorf("roles after the sweep = %v", roles)
	}
}

// The janitor spares the user being created (pending), the current user
// and a retiring user within its grace, and deletes a retiring user after
// it (ADR-0036 decision c).
func TestNexusJanitor_RespectsPendingAndRetiringUsers(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	current, _ := h.storedAuthenticates()
	pending := testIdentity + "_00000000000000bb"
	retiringID := testIdentity + "_00000000000000cc"
	h.putOwnedUser(pending, testNXANamespace, testNXAName, nexus.UserActive)
	h.putOwnedUser(retiringID, testNXANamespace, testNXAName, nexus.UserActive)
	s := h.secret()
	s.Annotations[nexussecret.AnnotationPendingUserID] = pending
	s.Annotations[nexussecret.AnnotationRetiringUserID] = retiringID
	s.Annotations[nexussecret.AnnotationRetireAfter] = nexussecret.FormatTime(h.clock.Now().Add(time.Minute))
	if err := h.r.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	j := h.janitor()
	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{current, pending, retiringID}
	slices.Sort(want)
	if got := h.userIDs(); !slices.Equal(got, want) {
		t.Errorf("users = %v, want %v", got, want)
	}
	h.clock.Advance(time.Minute)
	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.nx.User(retiringID); ok {
		t.Error("a retiring user survived its grace")
	}
}

// A user an administrator disabled while no Secret names an existing user
// stays: the reconciler reports it instead of replacing it.
func TestNexusJanitor_KeepsTheAdministratorsDisabledUser(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	current, _ := h.storedAuthenticates()
	h.nx.SetUserStatus(current, nexus.UserDisabled)
	if err := h.r.Delete(context.Background(), h.secret()); err != nil {
		t.Fatal(err)
	}
	if err := h.janitor().Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.nx.User(current); !ok {
		t.Fatal("the janitor deleted a user an administrator disabled")
	}
	// Active and unnamed, it goes.
	h.nx.SetUserStatus(current, nexus.UserActive)
	if err := h.janitor().Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.nx.User(current); ok {
		t.Error("an active user no Secret names survived")
	}
}

func TestNexusJanitor_SweepsOrphanSecrets(t *testing.T) {
	orphan := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: nexussecret.Name("tenant", "gone"),
		Labels: nexussecret.Labels(testCluster, "tenant", "gone")}}
	misplaced := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "nexususer-elsewhere",
		Labels: nexussecret.Labels(testCluster, testNXANamespace, testNXAName)}}
	otherCluster := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: nexussecret.Name("tenant", "x"),
		Labels: nexussecret.Labels("prod", "tenant", "x")}}
	h := newNexusHarness(t, nil, newNexusAccess(), orphan, misplaced, otherCluster)
	h.mustReconcile()
	if err := h.janitor().Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	var left []string
	var list corev1.SecretList
	if err := h.r.List(context.Background(), &list, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Items {
		left = append(left, s.Name)
	}
	slices.Sort(left)
	want := []string{nexussecret.Name("tenant", "x"), nexussecret.Name(testNXANamespace, testNXAName)}
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Errorf("Secrets after the sweep = %v, want %v", left, want)
	}
}

// ADR-0026 parity: an object that stopped matching the selector loses this
// bridge's users and role, then its finalizers.
func TestNexusJanitor_ReleasesUnselected(t *testing.T) {
	nxa := newNexusAccess()
	h := newNexusHarness(t, nil, nxa)
	h.mustReconcile()
	sel, err := labels.Parse("bridge=eu")
	if err != nil {
		t.Fatal(err)
	}
	h.r.Config.HarborAccessSelector, h.r.Config.Instance = sel, "eu"
	obj := h.object()
	obj.Finalizers = append(obj.Finalizers, h.r.Config.NexusFinalizer())
	if err := h.r.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	j := h.janitor()
	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.nx.Users()) != 0 || len(h.nx.Roles()) != 0 {
		t.Errorf("users %v and roles %v of an unselected object survived", h.userIDs(), h.nx.Roles())
	}
	// The first sweep deletes; the release re-lists and releases in the
	// same sweep.
	if f := h.object().Finalizers; len(f) != 0 {
		t.Errorf("finalizers = %v, want released", f)
	}
}

func TestNexusJanitor_StopsWhileRateLimited(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	h.nx.SetRateLimited(true)
	j := h.janitor()
	if err := j.Sweep(context.Background()); err == nil {
		t.Fatal("a rate-limited sweep returned no error")
	}
	calls := len(h.nx.Requests())
	if err := j.Sweep(context.Background()); err == nil {
		t.Fatal("a sweep during the backoff returned no error")
	}
	if n := len(h.nx.Requests()); n != calls {
		t.Errorf("%d Nexus calls during the backoff", n-calls)
	}
}

func TestNexusJanitor_UserVerdictReadsTheOwnerLive(t *testing.T) {
	h := newNexusHarness(t, nil, newNexusAccess())
	h.mustReconcile()
	current, _ := h.storedAuthenticates()
	u, _ := h.nx.User(current)
	j := h.janitor()
	// A listing that no longer has the owner (stale) must not delete a
	// user whose owner exists when read live.
	j.sweepUsers(context.Background(), []nexus.User{u}, map[string]*corev1.Secret{}, true, map[types.NamespacedName]*nexusv1alpha1.NexusAccess{})
	if _, ok := h.nx.User(current); !ok {
		t.Error("the janitor deleted a user whose owner exists")
	}
}
