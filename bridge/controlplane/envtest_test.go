// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
)

// setupEnvtest starts an envtest control plane (kube-apiserver + etcd) and
// installs the project's CRDs. Skips the test when KUBEBUILDER_ASSETS is
// unset so `go test ./...` keeps working without the envtest binaries
// installed — `make envtest` is the entry point that fetches them.
func setupEnvtest(t *testing.T) *rest.Config {
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via `make envtest` to install kube-apiserver+etcd binaries")
	}

	// Resolve the repo root from this file's location so the test does
	// not depend on the working directory.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest stop: %v", err)
		}
	})
	return cfg
}

// flakyHarbor wraps the in-memory mockHarbor and fails the first
// failuresBefore Create/GetByName calls with a transient error. Used to
// verify controller-runtime's retry semantics: markTransientError must
// return the error from Reconcile so the manager schedules a requeue, not
// just log and move on.
type flakyHarbor struct {
	*mockHarbor
	failuresLeft  int32 // atomic
	totalAttempts int32 // atomic
}

func newFlakyHarbor(failures int) *flakyHarbor {
	return &flakyHarbor{mockHarbor: newMockHarbor(), failuresLeft: int32(failures)}
}

// Attempts returns the total number of GetByName + Create attempts so
// far, including the failed ones. Test asserts this is > failures so it
// can prove controller-runtime actually retried.
func (f *flakyHarbor) Attempts() int { return int(atomic.LoadInt32(&f.totalAttempts)) }

func (f *flakyHarbor) maybeFail() error {
	atomic.AddInt32(&f.totalAttempts, 1)
	if atomic.AddInt32(&f.failuresLeft, -1) >= 0 {
		return errors.New("simulated transient Harbor 502 — backend unavailable")
	}
	return nil
}

func (f *flakyHarbor) Create(ctx context.Context, name, description string, perms []harbor.ProjectPermission) (*harbor.Robot, error) {
	if err := f.maybeFail(); err != nil {
		return nil, err
	}
	return f.mockHarbor.Create(ctx, name, description, perms)
}

func (f *flakyHarbor) GetByName(ctx context.Context, name string) (*harbor.Robot, error) {
	if err := f.maybeFail(); err != nil {
		return nil, err
	}
	return f.mockHarbor.GetByName(ctx, name)
}

// TestEnvtest_TransientHarborError_RetriesUntilReady proves the
// controller-runtime retry contract holds end-to-end: when
// markTransientError returns the cause from Reconcile, the manager
// schedules a requeue with backoff and the reconcile eventually
// succeeds. A unit test against Reconcile() alone cannot verify this
// because the retry behaviour lives in controller-runtime's controller
// loop, not in our function.
func TestEnvtest_TransientHarborError_RetriesUntilReady(t *testing.T) {
	cfg := setupEnvtest(t)

	// Need a namespace to put the HarborAccess + the Secret into.
	k8s, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := k8s.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNS},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  testScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// Several envtest managers share this process; the controller
		// name is registered globally for metrics.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}

	const failures = 3 // Harbor will reject GetByName/Create this many times.
	flaky := newFlakyHarbor(failures)
	clock := fixedClock{time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)}

	rec := &Reconciler{
		Client: mgr.GetClient(),
		Scheme: testScheme,
		Harbor: flaky,
		Config: testReconcilerConfig(),
		Clock:  clock,
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup: %v", err)
	}

	mgrCtx, cancelMgr := context.WithCancel(context.Background())
	t.Cleanup(cancelMgr)
	managerStopped := make(chan error, 1)
	go func() { managerStopped <- mgr.Start(mgrCtx) }()

	ha := newHarborAccess()
	ha.Finalizers = nil // let the controller add it
	if err := k8s.Create(context.Background(), ha); err != nil {
		t.Fatalf("create HarborAccess: %v", err)
	}

	// Poll status. Default workqueue exponential backoff starts at 5ms
	// and doubles, capped at 1000s. Three failures should land Ready=True
	// in a few hundred ms, but give it 15s headroom for slow CI.
	deadline := time.Now().Add(15 * time.Second)
	var final harborv1alpha1.HarborAccess
	for time.Now().Before(deadline) {
		if err := k8s.Get(context.Background(),
			types.NamespacedName{Namespace: ha.Namespace, Name: ha.Name},
			&final); err == nil && readyTrue(&final) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !readyTrue(&final) {
		t.Fatalf("HarborAccess never reached Ready=True. Conditions: %#v", final.Status.Conditions)
	}
	if got := flaky.Attempts(); got <= failures {
		t.Fatalf("flakyHarbor.Attempts = %d; expected > %d (retry did not happen)", got, failures)
	}

	cancelMgr()
	select {
	case err := <-managerStopped:
		if err != nil {
			t.Logf("manager.Start returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop after cancel")
	}
}

func readyTrue(ha *harborv1alpha1.HarborAccess) bool {
	for _, c := range ha.Status.Conditions {
		if c.Type == harborv1alpha1.ConditionReady && c.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func testReconcilerConfig() *Config {
	issuer, _ := url.Parse(testIssuer)
	harborURL, _ := url.Parse(testHarbor)
	return &Config{
		ClusterName:    testCluster,
		Namespace:      testNS,
		OIDCIssuer:     issuer,
		HarborURL:      harborURL,
		HarborAdminDir: "/dev/null",
		LogLevel:       "debug",
		Audience:       "harbor.example.com",
	}
}

// The CRD enforces Harbor's project-name rule at admission; "*" (every
// project in Harbor) must never be stored.
func TestEnvtest_CRDRejectsInvalidProjectNames(t *testing.T) {
	cfg := setupEnvtest(t)
	k8s, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := k8s.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: testNS},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
	for i, project := range []string{"*", "Prod", "a..b", "-x", "team/app"} {
		ha := newHarborAccess()
		ha.Name = fmt.Sprintf("invalid-project-%d", i)
		ha.Spec.Permissions[0].Project = project
		err := k8s.Create(context.Background(), ha)
		if !apierrors.IsInvalid(err) {
			t.Errorf("project %q: err = %v, want an Invalid admission error", project, err)
		}
	}
	ok := newHarborAccess()
	ok.Name = "valid-project"
	ok.Spec.Permissions[0].Project = "team-a.app_1"
	if err := k8s.Create(context.Background(), ok); err != nil {
		t.Errorf("valid project name refused: %v", err)
	}
}
