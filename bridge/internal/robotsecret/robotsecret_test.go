// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package robotsecret

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestName_ContractPinned pins the Secret name to the dot-delimited
// contract (ADR-0018). Existing clusters hold Secrets under exactly this
// name; changing it orphans every one of them and 503s every pull until
// the control plane rebuilds them.
func TestName_ContractPinned(t *testing.T) {
	if got, want := Name("team-a", "flux-access"), "robot-team-a.flux-access"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
}

// TestName_DotDelimiterIsInjective is the ADR-0018 / audit-F2 guard: under a
// '-' join these two distinct HarborAccess objects collapse to one Secret
// name, letting one workload read the other's robot password.
func TestName_DotDelimiterIsInjective(t *testing.T) {
	a, b := Name("team-a", "svc"), Name("team", "a-svc")
	if a == b {
		t.Fatalf("distinct HarborAccess objects share Secret name %q", a)
	}
}

func TestName_Overflow(t *testing.T) {
	long := strings.Repeat("z", 300)
	got := Name("team", long)
	if len(got) > nameMax {
		t.Errorf("len(%q) = %d, want <= %d", got, len(got), nameMax)
	}
	if !strings.HasPrefix(got, NamePrefix) {
		t.Errorf("truncated name lost its %q prefix: %q", NamePrefix, got)
	}
	if got2 := Name("team", long); got != got2 {
		t.Errorf("not deterministic: %q vs %q", got, got2)
	}
	if Name("team", strings.Repeat("a", 300)) == Name("team", strings.Repeat("b", 300)) {
		t.Error("distinct long inputs produced identical Secret names")
	}
}

func TestOwnership(t *testing.T) {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: Labels("dev", "ns", "ha")}}
	if !IsManaged(s) {
		t.Fatal("Labels() output not recognised as managed")
	}
	if ns, name, ok := Owner(s); !ok || ns != "ns" || name != "ha" {
		t.Errorf("Owner = %q/%q %v", ns, name, ok)
	}
	if StampedForOther(s, "ns", "ha") {
		t.Error("own Secret reported as stamped for another HarborAccess")
	}
	if !StampedForOther(s, "ns", "other") || !StampedForOther(s, "other", "ha") {
		t.Error("foreign owner not detected")
	}
	unmanaged := &corev1.Secret{}
	if StampedForOther(unmanaged, "ns", "ha") {
		t.Error("an unmanaged Secret must be adoptable, not stamped for other")
	}
	if _, _, ok := Owner(unmanaged); ok {
		t.Error("unmanaged Secret has no owner")
	}
}

func TestAnnotations_RoundTrip(t *testing.T) {
	at := time.Date(2026, 9, 24, 3, 4, 5, 600_000_000, time.UTC)
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		AnnotationRobotID:           FormatRobotID(42),
		AnnotationRotationNotBefore: FormatRotationNotBefore(at),
	}}}
	if id, ok := RobotID(s); !ok || id != 42 {
		t.Errorf("RobotID = %d %v", id, ok)
	}
	got, ok := RotationNotBefore(s)
	if !ok {
		t.Fatal("RotationNotBefore unparseable")
	}
	if got.Before(at) || got.Sub(at) >= time.Second {
		t.Errorf("RotationNotBefore = %s, want %s rounded up to the second", got, at)
	}
	if _, ok := RobotID(&corev1.Secret{}); ok {
		t.Error("missing annotation reported as present")
	}
	bad := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		AnnotationRobotID: "x", AnnotationRotationNotBefore: "tomorrow",
	}}}
	if _, ok := RobotID(bad); ok {
		t.Error("garbage robot ID accepted")
	}
	if _, ok := RotationNotBefore(bad); ok {
		t.Error("garbage timestamp accepted")
	}
}

func TestCredentials(t *testing.T) {
	s := &corev1.Secret{Data: map[string][]byte{KeyUsername: []byte("u"), KeyPassword: []byte("p")}}
	if u, p, err := Credentials(s); err != nil || u != "u" || p != "p" {
		t.Errorf("Credentials = %q %q %v", u, p, err)
	}
	if _, _, err := Credentials(&corev1.Secret{Data: map[string][]byte{KeyUsername: []byte("u")}}); err == nil {
		t.Error("missing password accepted")
	}
}
