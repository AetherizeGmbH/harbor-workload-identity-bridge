// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexussecret

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// TestName_ContractPinned pins the Secret name (ADR-0033 decision e). The
// data plane reads the Secret at exactly this name.
func TestName_ContractPinned(t *testing.T) {
	if got, want := Name("team-a", "web"), "nexususer-team-a.web"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
}

// TestName_DotDelimiterIsInjective: under a '-' join these two distinct
// NexusAccess objects would share one Secret (ADR-0018, audit F2).
func TestName_DotDelimiterIsInjective(t *testing.T) {
	if Name("team-a", "svc") == Name("team", "a-svc") {
		t.Fatal("distinct NexusAccess objects share a Secret name")
	}
}

// TestName_DisjointFromRobotSecrets is ADR-0033 Context 9: a prefix like
// "robot-nexus." would make the Secret of NexusAccess team/app equal the
// robot Secret of a HarborAccess named "team.app" in namespace "nexus".
// Neither prefix is a prefix of the other.
func TestName_DisjointFromRobotSecrets(t *testing.T) {
	if strings.HasPrefix(NamePrefix, robotsecret.NamePrefix) || strings.HasPrefix(robotsecret.NamePrefix, NamePrefix) {
		t.Fatalf("prefixes %q and %q overlap", NamePrefix, robotsecret.NamePrefix)
	}
	for _, c := range [][4]string{
		{"team", "app", "nexususer", "team.app"},
		{"nexus", "team.app", "team", "app"},
		{"a", "b", "a", "b"},
	} {
		if Name(c[0], c[1]) == robotsecret.Name(c[2], c[3]) {
			t.Errorf("NexusAccess %s/%s and HarborAccess %s/%s share Secret %q", c[0], c[1], c[2], c[3], Name(c[0], c[1]))
		}
	}
	if _, _, ok := ParseName(robotsecret.Name("team", "app")); ok {
		t.Error("a robot Secret name parsed as a Nexus Secret name")
	}
	if _, _, ok := robotsecret.ParseName(Name("team", "app")); ok {
		t.Error("a Nexus Secret name parsed as a robot Secret name")
	}
}

func TestParseName(t *testing.T) {
	for name, want := range map[string][3]string{
		"nexususer-team-a.web": {"team-a", "web", "true"},
		"nexususer-team.a.b":   {"team", "a.b", "true"},
		"nexususer-team":       {"", "", "false"},
		"nexususer-.x":         {"", "", "false"},
		"nexususer-x.":         {"", "", "false"},
		"robot-team.web":       {"", "", "false"},
		"nexususerteam.web":    {"", "", "false"},
	} {
		ns, n, ok := ParseName(name)
		if ns != want[0] || n != want[1] || ok != (want[2] == "true") {
			t.Errorf("ParseName(%q) = %q, %q, %v, want %v", name, ns, n, ok, want)
		}
	}
}

func TestName_Overflow(t *testing.T) {
	long := strings.Repeat("z", 300)
	got := Name("team", long)
	if len(got) > nameMax || !strings.HasPrefix(got, NamePrefix) {
		t.Errorf("truncated name %q: length %d, prefix kept %v", got, len(got), strings.HasPrefix(got, NamePrefix))
	}
	if Name("team", strings.Repeat("a", 300)) == Name("team", strings.Repeat("b", 300)) {
		t.Error("distinct long inputs produced identical Secret names")
	}
}

func TestOwnership(t *testing.T) {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: Labels("dev", "ns", "nxa")}}
	if !IsManaged(s) {
		t.Fatal("Labels() output not recognised as managed")
	}
	if ns, name, ok := Owner(s); !ok || ns != "ns" || name != "nxa" {
		t.Errorf("Owner = %q/%q %v", ns, name, ok)
	}
	if !OwnedBy(s, "ns", "nxa") || StampedForOther(s, "ns", "nxa") {
		t.Error("own Secret not recognised as owned")
	}
	if OwnedBy(s, "ns", "other") || !StampedForOther(s, "other", "nxa") {
		t.Error("foreign owner not detected")
	}

	// A robot Secret of a HarborAccess with the same namespace and name is
	// neither managed as a Nexus Secret nor owned by the NexusAccess.
	robot := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: robotsecret.Labels("dev", "ns", "nxa")}}
	if IsManaged(robot) || OwnedBy(robot, "ns", "nxa") {
		t.Error("a robot Secret passes as a Nexus Secret")
	}
	// ...and a Nexus Secret is no robot Secret.
	if robotsecret.IsManaged(s) {
		t.Error("a Nexus Secret passes as a robot Secret")
	}
	if _, _, ok := robotsecret.Owner(s); ok {
		t.Error("a Nexus Secret has a HarborAccess owner")
	}

	withoutKind := s.DeepCopy()
	delete(withoutKind.Labels, LabelAccessKind)
	if IsManaged(withoutKind) || OwnedBy(withoutKind, "ns", "nxa") {
		t.Error("a Secret without the access-kind label passes as a Nexus Secret")
	}
	if StampedForOther(&corev1.Secret{}, "ns", "nxa") {
		t.Error("an unmanaged Secret is not stamped for anyone")
	}
}

func TestAnnotations(t *testing.T) {
	at := time.Date(2026, 9, 28, 3, 4, 5, 600_000_000, time.UTC)
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		AnnotationUserID:            "u_1",
		AnnotationPendingUserID:     "u_2",
		AnnotationRetiringUserID:    "u_0",
		AnnotationRetireAfter:       FormatTime(at),
		AnnotationRotationNotBefore: FormatTime(at),
		AnnotationGrantsIncomplete:  FormatGrantsIncomplete([]string{"b", "a", "b"}),
	}}}
	if UserID(s) != "u_1" || PendingUserID(s) != "u_2" {
		t.Errorf("UserID, PendingUserID = %q, %q", UserID(s), PendingUserID(s))
	}
	id, after, ok := Retiring(s)
	if !ok || id != "u_0" || after.Before(at) || after.Sub(at) >= time.Second {
		t.Errorf("Retiring = %q, %s, %v; want u_0 at %s rounded up", id, after, ok, at)
	}
	if nb, ok := RotationNotBefore(s); !ok || !nb.Equal(after) {
		t.Errorf("RotationNotBefore = %s, %v", nb, ok)
	}
	if s.Annotations[AnnotationRotationNotBefore] == "" || AnnotationRotationNotBefore != robotsecret.AnnotationRotationNotBefore {
		t.Error("the rotation promise must use the robot Secret's annotation, which the data plane reads")
	}
	missing, incomplete := GrantsIncomplete(s)
	if !incomplete || strings.Join(missing, ",") != "a,b" {
		t.Errorf("GrantsIncomplete = %v, %v", missing, incomplete)
	}

	// Present with an empty value still refuses.
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationGrantsIncomplete: ""}}}
	if _, incomplete := GrantsIncomplete(empty); !incomplete {
		t.Error("an empty grants-incomplete annotation reads as complete")
	}
	if _, incomplete := GrantsIncomplete(&corev1.Secret{}); incomplete {
		t.Error("a Secret without the annotation reads as incomplete")
	}

	bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		AnnotationRetiringUserID: "u_0", AnnotationRetireAfter: "soon",
	}}}
	if id, after, ok := Retiring(bad); !ok || id != "u_0" || !after.IsZero() {
		t.Errorf("Retiring with an unparseable instant = %q, %s, %v; want the user with the zero time", id, after, ok)
	}
	if _, _, ok := Retiring(&corev1.Secret{}); ok {
		t.Error("no retiring user reported as one")
	}
}

func TestCredentials(t *testing.T) {
	s := &corev1.Secret{Data: map[string][]byte{KeyUsername: []byte("u"), KeyPassword: []byte("p")}}
	if u, p, err := Credentials(s); err != nil || u != "u" || p != "p" {
		t.Errorf("Credentials = %q %q %v", u, p, err)
	}
	if _, _, err := Credentials(&corev1.Secret{}); err == nil {
		t.Error("a Secret without data yields credentials")
	}
}
