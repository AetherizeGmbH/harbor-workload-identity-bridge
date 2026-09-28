// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus/nexustest"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/nexussecret"
)

// nexusEnvtest is an envtest apiserver with the tenant and bridge
// namespaces.
func nexusEnvtest(t *testing.T) (*rest.Config, client.Client) {
	t.Helper()
	cfg := setupEnvtest(t)
	k8s, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{testNS, testNXANamespace} {
		if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	return cfg, k8s
}

func newEnvtestManager(t *testing.T, cfg *rest.Config, bridge *Config) ctrl.Manager {
	t.Helper()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  testScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Cache:   bridge.CacheOptions(),
		// Several envtest managers share this process; the controller
		// name is registered globally for metrics.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

func runManager(t *testing.T, mgr ctrl.Manager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	// Wait for the manager to stop, so no reconcile outlives the test.
	t.Cleanup(func() { cancel(); <-done })
}

func eventually(t *testing.T, what string, cond func() bool) {
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

// TestEnvtest_NexusCRD proves the CRD rejects what the reconciler could
// never serve, and defaults the format.
func TestEnvtest_NexusCRD(t *testing.T) {
	_, k8s := nexusEnvtest(t)
	ctx := context.Background()
	for name, edit := range map[string]func(*nexusv1alpha1.NexusAccess){
		"64-character name":      func(n *nexusv1alpha1.NexusAccess) { n.Name = strings.Repeat("n", 64) },
		"64-character namespace": func(n *nexusv1alpha1.NexusAccess) { n.Spec.ServiceAccountRef.Namespace = strings.Repeat("a", 64) },
		"wildcard repository":    func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories[0].Name = "*" },
		"repository too long":    func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories[0].Name = strings.Repeat("r", 168) },
		"unknown access":         func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories[0].Access = "delete" },
		"oci format":             func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories[0].Format = "oci" },
		"no repositories":        func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories = nil },
		"duplicate repository": func(n *nexusv1alpha1.NexusAccess) {
			n.Spec.Repositories = append(n.Spec.Repositories, n.Spec.Repositories[0])
		},
		"empty audience": func(n *nexusv1alpha1.NexusAccess) { n.Spec.TrustPolicy.Audience = "" },
		"issuer without scheme": func(n *nexusv1alpha1.NexusAccess) {
			n.Spec.TrustPolicy.Issuer = "kubernetes.default.svc"
		},
	} {
		nxa := newNexusAccess()
		nxa.Finalizers = nil
		nxa.Name = "invalid-" + strings.ReplaceAll(name, " ", "-")
		edit(nxa)
		if err := k8s.Create(ctx, nxa); !apierrors.IsInvalid(err) {
			t.Errorf("%s: err = %v, want an Invalid admission error", name, err)
		}
	}

	// tokenTTL and spec as raw JSON: a typed client would send a valid
	// duration and an empty spec object.
	for name, patch := range map[string]func(u *unstructured.Unstructured){
		"tokenTTL in days": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "1d", "spec", "tokenTTL")
		},
		"tokenTTL below 5m": func(u *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(u.Object, "1m", "spec", "tokenTTL")
		},
		"no spec": func(u *unstructured.Unstructured) { unstructured.RemoveNestedField(u.Object, "spec") },
	} {
		nxa := newNexusAccess()
		nxa.Finalizers = nil
		nxa.Name = "raw-" + strings.ReplaceAll(name, " ", "-")
		obj, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(nxa)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{Object: obj}
		u.SetGroupVersionKind(nexusv1alpha1.GroupVersion.WithKind("NexusAccess"))
		patch(u)
		if err := k8s.Create(ctx, u); !apierrors.IsInvalid(err) {
			t.Errorf("%s: err = %v, want an Invalid admission error", name, err)
		}
	}

	ok := newNexusAccess()
	ok.Finalizers = nil
	ok.Name = "valid"
	ok.Spec.Repositories = []nexusv1alpha1.RepositoryGrant{
		{Name: "Docker-Hosted_1.x", Access: nexusv1alpha1.AccessPull},
		{Name: "ci", Access: nexusv1alpha1.AccessPullPush, Format: nexusv1alpha1.FormatDocker},
	}
	obj, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(ok)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(nexusv1alpha1.GroupVersion.WithKind("NexusAccess"))
	unstructured.RemoveNestedField(u.Object, "spec", "tokenTTL")
	if err := k8s.Create(ctx, u); err != nil {
		t.Fatalf("valid NexusAccess refused: %v", err)
	}
	got := &nexusv1alpha1.NexusAccess{}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(ok), got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Repositories[0].Format != nexusv1alpha1.FormatDocker || got.Spec.TokenTTL.Duration != time.Hour {
		t.Errorf("defaults: format %q, tokenTTL %s", got.Spec.Repositories[0].Format, got.Spec.TokenTTL.Duration)
	}
}

// The CRD's repository-name limit is the one the Nexus naming derives from
// Nexus's 200-character privilege names.
func TestNexusCRD_RepositoryNameLimit(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(crdDir(), "nexus.aetherize.io_nexusaccesses.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema struct {
						Properties struct {
							Spec struct {
								Properties struct {
									Repositories struct {
										Items struct {
											Properties struct {
												Name struct {
													MaxLength int    `json:"maxLength"`
													Pattern   string `json:"pattern"`
												} `json:"name"`
											} `json:"properties"`
										} `json:"items"`
									} `json:"repositories"`
								} `json:"properties"`
							} `json:"spec"`
						} `json:"properties"`
					} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	name := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Repositories.Items.Properties.Name
	if name.MaxLength != nexus.RepositoryNameMaxLen {
		t.Errorf("CRD maxLength %d, nexus.RepositoryNameMaxLen %d", name.MaxLength, nexus.RepositoryNameMaxLen)
	}
	if name.Pattern != `^[a-zA-Z0-9-][a-zA-Z0-9_.-]*$` {
		t.Errorf("CRD pattern %q is not Nexus's repository rule", name.Pattern)
	}
}

// TestEnvtest_SetupNexus runs the backend as main wires it: SetupNexus
// with the admin credentials in a directory, the cache options of a Nexus
// configuration, a NexusAccess taken to Ready.
func TestEnvtest_SetupNexus(t *testing.T) {
	cfg, k8s := nexusEnvtest(t)
	ctx := context.Background()
	nx := nexustest.New(t)
	nx.AddRepository("docker", testRepo)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "username"), nexustest.AdminUsername)
	mustWrite(t, filepath.Join(dir, "password"), nexustest.AdminPassword)
	bridge := testReconcilerConfig()
	bridge.Nexus = &NexusConfig{URL: nx.URL, AdminDir: dir, AllowHTTP: true, RateLimitBackoff: time.Minute}
	mgr := newEnvtestManager(t, cfg, bridge)
	reg := prometheus.NewRegistry()
	if err := SetupNexus(mgr, bridge, logr.Discard(), reg); err != nil {
		t.Fatal(err)
	}
	runManager(t, mgr)

	nxa := newNexusAccess()
	nxa.Name, nxa.Finalizers = "setup", nil
	if err := k8s.Create(ctx, nxa); err != nil {
		t.Fatal(err)
	}
	secretKey := types.NamespacedName{Namespace: testNS, Name: nexussecret.Name(testNXANamespace, "setup")}
	eventually(t, "NexusAccess Ready with a Secret Nexus accepts", func() bool {
		got := &nexusv1alpha1.NexusAccess{}
		s := &corev1.Secret{}
		return k8s.Get(ctx, client.ObjectKeyFromObject(nxa), got) == nil && nexusReady(got) &&
			k8s.Get(ctx, secretKey, s) == nil && nx.Authenticate(string(s.Data["username"]), string(s.Data["password"]))
	})
	mfs, err := reg.Gather()
	if err != nil || len(mfs) != 1 || mfs[0].GetName() != "bridge_nexus_rate_limited" {
		t.Errorf("registered metrics = %v, %v", mfs, err)
	}
}

func nexusReady(nxa *nexusv1alpha1.NexusAccess) bool {
	return meta.IsStatusConditionTrue(nxa.Status.Conditions, nexusv1alpha1.ConditionReady)
}

func nexusReason(nxa *nexusv1alpha1.NexusAccess) string {
	if c := meta.FindStatusCondition(nxa.Status.Conditions, nexusv1alpha1.ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

// TestEnvtest_NexusLifecycle runs the reconciler against a real apiserver
// and the fake Nexus through a NexusAccess's life (ADR-0036): create, a
// grant change, a missing repository, a scheduled rotation and the
// retirement of the previous user, a deleted Secret rebuilt through the
// Secret watch, a serviceAccountRef change, a refusal and its end, a
// second NexusAccess for the same ServiceAccount taking over once the
// first is deleted, deletion blocked while Nexus is down, and deletion
// revoking every user and role.
func TestEnvtest_NexusLifecycle(t *testing.T) {
	cfg, k8s := nexusEnvtest(t)
	ctx := context.Background()
	nx := nexustest.New(t)
	nx.AddRepository("docker", testRepo)
	clock := newStepClock()
	bridge := testReconcilerConfig()
	bridge.Nexus = &NexusConfig{}
	mgr := newEnvtestManager(t, cfg, bridge)
	backoff := &NexusBackoff{Window: time.Minute, Clock: clock}
	rec := &NexusReconciler{Client: mgr.GetClient(), Scheme: testScheme, Nexus: backoff.Wrap(nx.Client(t)), Backoff: backoff, Config: bridge, Clock: clock}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if rec.APIReader != mgr.GetAPIReader() {
		t.Fatal("SetupWithManager did not wire the uncached API reader")
	}
	runManager(t, mgr)

	key := types.NamespacedName{Namespace: testNXANamespace, Name: testNXAName}
	secretKey := types.NamespacedName{Namespace: testNS, Name: nexussecret.Name(testNXANamespace, testNXAName)}
	get := func(k types.NamespacedName) *nexusv1alpha1.NexusAccess {
		got := &nexusv1alpha1.NexusAccess{}
		if err := k8s.Get(ctx, k, got); err != nil {
			return nil
		}
		return got
	}
	secret := func() *corev1.Secret {
		s := &corev1.Secret{}
		if k8s.Get(ctx, secretKey, s) != nil || s.DeletionTimestamp != nil {
			return nil
		}
		return s
	}
	// stored returns the user the Secret holds when Nexus accepts its
	// password.
	stored := func() string {
		s := secret()
		if s == nil || !nx.Authenticate(string(s.Data["username"]), string(s.Data["password"])) {
			return ""
		}
		return string(s.Data["username"])
	}
	edit := func(k types.NamespacedName, fn func(*nexusv1alpha1.NexusAccess)) {
		t.Helper()
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			got := &nexusv1alpha1.NexusAccess{}
			if err := k8s.Get(ctx, k, got); err != nil {
				return err
			}
			fn(got)
			return k8s.Update(ctx, got)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// touch triggers a pass without a spec change, after the clock moved.
	touch := func(k types.NamespacedName) {
		t.Helper()
		edit(k, func(n *nexusv1alpha1.NexusAccess) {
			if n.Annotations == nil {
				n.Annotations = map[string]string{}
			}
			n.Annotations["test/touch"] = clock.Now().Format(time.RFC3339Nano)
		})
	}
	roleIDs := func() []string {
		var ids []string
		for _, r := range nx.Roles() {
			ids = append(ids, r.ID)
		}
		return ids
	}

	// Create.
	nxa := newNexusAccess()
	nxa.Finalizers = nil
	if err := k8s.Create(ctx, nxa); err != nil {
		t.Fatal(err)
	}
	var first string
	eventually(t, "role, user and Secret created, Ready", func() bool {
		first = stored()
		got := get(key)
		return first != "" && got != nil && nexusReady(got) && got.Status.ObservedGeneration == got.Generation
	})

	// A grant change updates the role, not the user.
	nx.AddRepository("docker", "ci")
	edit(key, func(n *nexusv1alpha1.NexusAccess) {
		n.Spec.Repositories = append(n.Spec.Repositories, nexusv1alpha1.RepositoryGrant{Name: "ci", Access: nexusv1alpha1.AccessPullPush})
	})
	eventually(t, "role grants ci as well, same user", func() bool {
		role, _ := nx.Role(testIdentity)
		got := get(key)
		return len(role.Privileges) == 4 && stored() == first && got != nil && nexusReady(got) && got.Status.ObservedGeneration == got.Generation
	})

	// A missing repository: RepositoryNotFound, grants-incomplete, the
	// generation not observed; granted once it exists.
	edit(key, func(n *nexusv1alpha1.NexusAccess) {
		n.Spec.Repositories = append(n.Spec.Repositories, nexusv1alpha1.RepositoryGrant{Name: "later", Access: nexusv1alpha1.AccessPull})
	})
	eventually(t, "RepositoryNotFound with the Secret marked", func() bool {
		got, s := get(key), secret()
		if got == nil || s == nil {
			return false
		}
		missing, incomplete := nexussecret.GrantsIncomplete(s)
		return nexusReason(got) == ReasonRepositoryNotFound && incomplete && slices.Equal(missing, []string{"later"}) &&
			got.Status.ObservedGeneration != got.Generation
	})
	nx.AddRepository("docker", "later")
	touch(key)
	eventually(t, "Ready once the repository exists, mark removed", func() bool {
		got, s := get(key), secret()
		if got == nil || s == nil {
			return false
		}
		_, incomplete := nexussecret.GrantsIncomplete(s)
		return nexusReady(got) && !incomplete
	})

	// Scheduled rotation: a new generation, the previous user retiring,
	// then deleted after the grace.
	clock.Advance(PasswordRotationInterval + RotationSafetyMargin)
	touch(key)
	var second string
	eventually(t, "rotated to a new generation, the previous user retiring", func() bool {
		second = stored()
		s := secret()
		if second == "" || second == first || s == nil {
			return false
		}
		id, _, ok := nexussecret.Retiring(s)
		_, firstExists := nx.User(first)
		return ok && id == first && firstExists
	})
	clock.Advance(NexusUserRetireGrace)
	touch(key)
	eventually(t, "the previous user deleted after its grace", func() bool {
		_, firstExists := nx.User(first)
		s := secret()
		if s == nil {
			return false
		}
		_, _, retiring := nexussecret.Retiring(s)
		return !firstExists && !retiring && stored() == second
	})

	// A deleted Secret is rebuilt through the Secret watch, with a new
	// user; the previous one goes at once.
	if err := k8s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: secretKey.Name}}); err != nil {
		t.Fatal(err)
	}
	var third string
	eventually(t, "Secret rebuilt with a new user, the previous one deleted", func() bool {
		third = stored()
		_, secondExists := nx.User(second)
		return third != "" && third != second && !secondExists
	})

	// serviceAccountRef change: new identity, the old one's user and role
	// revoked.
	edit(key, func(n *nexusv1alpha1.NexusAccess) { n.Spec.ServiceAccountRef.Name = "new-sa" })
	newIdentity := "bridge-" + testCluster + "." + testSANamespace + ".new-sa"
	var fourth string
	eventually(t, "new identity provisioned, old identity revoked", func() bool {
		fourth = stored()
		id, _, _ := nexus.ParseUserID(fourth)
		_, thirdExists := nx.User(third)
		return id == newIdentity && !thirdExists && slices.Equal(roleIDs(), []string{newIdentity})
	})

	// Refusal: the users and the Secret go, the role stays; accepting the
	// object again creates a new user.
	edit(key, func(n *nexusv1alpha1.NexusAccess) { n.Spec.TrustPolicy.Audience = "another-bridge" })
	eventually(t, "refused: user and Secret revoked", func() bool {
		got := get(key)
		_, exists := nx.User(fourth)
		return got != nil && nexusReason(got) == ReasonAudienceMismatch && !exists && secret() == nil
	})
	edit(key, func(n *nexusv1alpha1.NexusAccess) { n.Spec.TrustPolicy.Audience = testReconcilerConfig().Audience })
	var fifth string
	eventually(t, "accepted again with a new user", func() bool {
		fifth = stored()
		got := get(key)
		return fifth != "" && fifth != fourth && got != nil && nexusReady(got)
	})

	// A second NexusAccess for the same ServiceAccount conflicts, and
	// takes over once the first is deleted.
	v2 := newNexusAccess()
	v2.Name, v2.Finalizers = "web-v2", nil
	v2.Spec.ServiceAccountRef.Name = "new-sa"
	if err := k8s.Create(ctx, v2); err != nil {
		t.Fatal(err)
	}
	v2Key := client.ObjectKeyFromObject(v2)
	eventually(t, "second NexusAccess reports RoleConflict", func() bool {
		got := get(v2Key)
		return got != nil && nexusReason(got) == ReasonRoleConflict
	})

	// Deletion waits while Nexus is down.
	nx.SetUnavailable(true)
	if err := k8s.Delete(ctx, get(key)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "DeletionBlocked while Nexus is down", func() bool {
		got := get(key)
		return got != nil && nexusReason(got) == ReasonDeletionBlocked && slices.Contains(got.Finalizers, NexusFinalizerName)
	})
	nx.SetUnavailable(false)
	eventually(t, "first NexusAccess gone with its user and Secret", func() bool {
		_, exists := nx.User(fifth)
		return get(key) == nil && !exists && secret() == nil
	})
	v2SecretKey := types.NamespacedName{Namespace: testNS, Name: nexussecret.Name(testNXANamespace, "web-v2")}
	eventually(t, "the second NexusAccess took the role over", func() bool {
		got := get(v2Key)
		s := &corev1.Secret{}
		if got == nil || !nexusReady(got) || k8s.Get(ctx, v2SecretKey, s) != nil {
			return false
		}
		role, ok := nx.Role(newIdentity)
		return ok && role.Description == NexusRoleDescription(testCluster, testNXANamespace, "web-v2") &&
			nx.Authenticate(string(s.Data["username"]), string(s.Data["password"]))
	})

	if err := k8s.Delete(ctx, get(v2Key)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "no NexusAccess, user, role or Secret left", func() bool {
		s := &corev1.Secret{}
		return get(v2Key) == nil && len(nx.Users()) == 0 && len(nx.Roles()) == 0 &&
			apierrors.IsNotFound(k8s.Get(ctx, v2SecretKey, s))
	})
}
