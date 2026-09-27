// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/yaml"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
)

const crdFile = "harbor.aetherize.io_harboraccesses.yaml"

// crdDir is config/crd/bases, the CRDs setupEnvtest installs.
func crdDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "config", "crd", "bases"))
}

// installCRD creates or updates the HarborAccess CRD from dir and waits
// until the apiserver validates with it: ready reports whether the new
// schema is in effect.
func installCRD(t *testing.T, cfg *rest.Config, dir string, ready func() bool) {
	t.Helper()
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{Paths: []string{dir}, ErrorIfPathMissing: true}); err != nil {
		t.Fatalf("install CRD from %s: %v", dir, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("CRD from %s never took effect", dir)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// legacyCRDDir writes the HarborAccess CRD as releases up to 0.10.0
// shipped it, as far as these tests care: tokenTTL with `format: duration`
// (the apiserver then accepts strfmt durations such as "1d"), no maxLength
// and the bounds rule without the unchanged-value exemption.
func legacyCRDDir(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(crdDir(), crdFile))
	if err != nil {
		t.Fatal(err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	versions, _ := crd["spec"].(map[string]any)["versions"].([]any)
	root := versions[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	spec := root["properties"].(map[string]any)["spec"].(map[string]any)
	ttl := spec["properties"].(map[string]any)["tokenTTL"].(map[string]any)
	ttl["format"] = "duration"
	delete(ttl, "maxLength")
	ttl["x-kubernetes-validations"] = []any{map[string]any{
		"message": "tokenTTL must be between 5m and 24h",
		"rule":    "duration(self) >= duration('5m') && duration(self) <= duration('24h')",
	}}
	out, err := yaml.Marshal(crd)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, crdFile), out, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// rawHarborAccess is newHarborAccess as an unstructured object whose
// tokenTTL is raw, so the apiserver sees exactly that string (a typed
// client would send a valid duration in canonical form).
func rawHarborAccess(t *testing.T, namespace, name, tokenTTL string) *unstructured.Unstructured {
	t.Helper()
	ha := newHarborAccess()
	ha.Namespace, ha.Name = namespace, name
	obj, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(ha)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{Object: obj}
	u.SetGroupVersionKind(harborv1alpha1.GroupVersion.WithKind("HarborAccess"))
	if err := unstructured.SetNestedField(u.Object, tokenTTL, "spec", "tokenTTL"); err != nil {
		t.Fatal(err)
	}
	return u
}

// setTokenTTL merge-patches the stored object's tokenTTL to raw.
func setTokenTTL(ctx context.Context, k8s client.Client, key client.ObjectKey, raw string) error {
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"tokenTTL": raw}})
	if err != nil {
		return err
	}
	ha := &harborv1alpha1.HarborAccess{}
	ha.Namespace, ha.Name = key.Namespace, key.Name
	return k8s.Patch(ctx, ha, client.RawPatch(types.MergePatchType, patch))
}

func ensureNamespaces(t *testing.T, k8s client.Client, names ...string) {
	t.Helper()
	for _, ns := range names {
		if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
}

// The apiserver must admit exactly the tokenTTL values the bridge can
// decode (time.ParseDuration), within 5m..24h. Under `format: duration` it
// also admitted strfmt forms such as "1d", and one of those failed every
// HarborAccess list the bridge made.
func TestEnvtest_CRDTokenTTLAdmitsOnlyGoDurations(t *testing.T) {
	cfg := setupEnvtest(t)
	ctx := context.Background()
	k8s, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatal(err)
	}
	ensureNamespaces(t, k8s, testNS)

	for i, raw := range []string{
		"1d", "3 hours", "1 day", "PT1H", "1x5m", "1h ", "",
		"18446744374s", // overflows time.Duration; strfmt wrapped it into range
		"4m", "25h", "0",
		strings.Repeat("0", 63) + "1h", // a valid 1h, but longer than maxLength
	} {
		if err := k8s.Create(ctx, rawHarborAccess(t, testNS, fmt.Sprintf("rejected-%d", i), raw)); !apierrors.IsInvalid(err) {
			t.Errorf("tokenTTL %q: err = %v, want an Invalid admission error", raw, err)
		}
	}

	accepted := map[string]time.Duration{
		"5m": 5 * time.Minute, "90m": 90 * time.Minute, "1h0m0s": time.Hour, "24h": 24 * time.Hour,
		"1.5h": 90 * time.Minute, "300000ms": 5 * time.Minute, "1h30m": 90 * time.Minute,
	}
	names := map[string]time.Duration{}
	for raw, want := range accepted {
		name := fmt.Sprintf("accepted-%d", len(names))
		names[name] = want
		if err := k8s.Create(ctx, rawHarborAccess(t, testNS, name, raw)); err != nil {
			t.Errorf("tokenTTL %q refused: %v", raw, err)
		}
	}

	// Everything admitted decodes: a typed LIST (what the informer does)
	// succeeds and yields the written durations.
	var list harborv1alpha1.HarborAccessList
	if err := k8s.List(ctx, &list, client.InNamespace(testNS)); err != nil {
		t.Fatalf("typed List after the admissible edge cases: %v", err)
	}
	if len(list.Items) != len(accepted) {
		t.Errorf("listed %d objects, want %d", len(list.Items), len(accepted))
	}
	for _, ha := range list.Items {
		if ttl := ha.Spec.TokenTTL; ttl.Err() != nil || ttl.Duration != names[ha.Name] {
			t.Errorf("%s: tokenTTL decoded to %s (Err %v), want %s", ha.Name, ttl.Duration, ttl.Err(), names[ha.Name])
		}
	}

	// Changing a stored value is held to the same rule.
	if err := setTokenTTL(ctx, k8s, client.ObjectKey{Namespace: testNS, Name: "accepted-0"}, "1d"); !apierrors.IsInvalid(err) {
		t.Errorf(`update to "1d": err = %v, want an Invalid admission error`, err)
	}
}

// A HarborAccess stored under the old CRD with tokenTTL "1d" stays in etcd
// after the upgrade. It must not take the bridge down: the controller's
// cache syncs, other HarborAccess objects reconcile, the cached LIST the
// data plane uses succeeds, and the bad object is reported as InvalidSpec.
// It stays writable (status, finalizer, a fix of tokenTTL), but cannot be
// changed to another invalid value.
func TestEnvtest_LegacyTokenTTLDoesNotStallTheBridge(t *testing.T) {
	cfg := setupEnvtest(t)
	ctx := context.Background()
	k8s, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatal(err)
	}
	ensureNamespaces(t, k8s, testNS, "tenant-b")

	probe := func() *unstructured.Unstructured { return rawHarborAccess(t, testNS, "probe", "1d") }
	installCRD(t, cfg, legacyCRDDir(t), func() bool {
		p := probe()
		p.SetFinalizers(nil)
		if k8s.Create(ctx, p) != nil {
			return false
		}
		_ = k8s.Delete(ctx, p)
		return true
	})
	typo := client.ObjectKey{Namespace: "tenant-b", Name: "typo"}
	fixme := client.ObjectKey{Namespace: "tenant-b", Name: "fixme"}
	for key, raw := range map[client.ObjectKey]string{typo: "1d", fixme: "3 hours"} {
		u := rawHarborAccess(t, key.Namespace, key.Name, raw)
		// A ServiceAccount of its own, so its robot does not collide with valid's.
		if err := unstructured.SetNestedField(u.Object, key.Name, "spec", "serviceAccountRef", "name"); err != nil {
			t.Fatal(err)
		}
		if err := k8s.Create(ctx, u); err != nil {
			t.Fatalf("create legacy object %s: %v", key, err)
		}
	}
	valid := newHarborAccess()
	valid.Finalizers = nil
	if err := k8s.Create(ctx, valid); err != nil {
		t.Fatal(err)
	}

	installCRD(t, cfg, crdDir(), func() bool {
		return apierrors.IsInvalid(k8s.Create(ctx, probe()))
	})

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  testScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Several envtest managers share this process; the controller
		// name is registered globally for metrics. A short sync timeout
		// makes a regression fail fast instead of after the 2m default.
		Controller: config.Controller{SkipNameValidation: ptr.To(true), CacheSyncTimeout: 20 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &Reconciler{Client: mgr.GetClient(), Scheme: testScheme, Harbor: newMockHarbor(), Config: testReconcilerConfig(), Clock: RealClock{}}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	mgrCtx, cancel := context.WithCancel(ctx)
	// done is closed, not sent on, so that both waitReady and the cleanup
	// can wait for the manager: a single value read by waitReady would
	// leave the cleanup blocked forever and hang the test binary.
	done := make(chan struct{})
	var mgrErr error
	go func() { mgrErr = mgr.Start(mgrCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// waitReady waits until every key's Ready condition has the wanted
	// reason, failing early if the manager exits.
	waitReady := func(want map[client.ObjectKey]string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			select {
			case <-done:
				t.Fatalf("manager exited: %v", mgrErr)
			default:
			}
			var pending []string
			for key, reason := range want {
				var ha harborv1alpha1.HarborAccess
				if err := k8s.Get(ctx, key, &ha); err != nil {
					t.Fatalf("get %s: %v", key, err)
				}
				if c := meta.FindStatusCondition(ha.Status.Conditions, harborv1alpha1.ConditionReady); c == nil || c.Reason != reason {
					pending = append(pending, fmt.Sprintf("%s: %+v, want %s", key, c, reason))
				}
			}
			if len(pending) == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("Ready conditions not reached: %s", strings.Join(pending, "; "))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	waitReady(map[client.ObjectKey]string{
		client.ObjectKeyFromObject(valid): ReasonReconcileSucceeded,
		typo:                              ReasonInvalidSpec,
		fixme:                             ReasonInvalidSpec,
	})

	// The data plane lists through the same cache on every request.
	var list harborv1alpha1.HarborAccessList
	if err := mgr.GetClient().List(ctx, &list); err != nil {
		t.Fatalf("cached List: %v", err)
	}

	// The legacy value may be fixed, but not replaced by another invalid one.
	if err := setTokenTTL(ctx, k8s, typo, "2d"); !apierrors.IsInvalid(err) {
		t.Errorf(`update legacy "1d" to "2d": err = %v, want an Invalid admission error`, err)
	}
	if err := setTokenTTL(ctx, k8s, fixme, "3h"); err != nil {
		t.Fatalf(`fix legacy "3 hours" to "3h": %v`, err)
	}
	waitReady(map[client.ObjectKey]string{fixme: ReasonReconcileSucceeded})

	// Deleting the legacy object releases its finalizer: the finalizer
	// patch must pass validation although tokenTTL stays "1d".
	if err := k8s.Delete(ctx, &harborv1alpha1.HarborAccess{ObjectMeta: metav1.ObjectMeta{Namespace: typo.Namespace, Name: typo.Name}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := k8s.Get(ctx, typo, &harborv1alpha1.HarborAccess{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("legacy object not deleted: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
