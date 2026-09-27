// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
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
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
)

// cacheProbe stands in for the data-plane server: it reads through the
// manager's cache the moment it starts, and it is ready once started.
type cacheProbe struct {
	reader  client.Reader
	started atomic.Bool
	result  chan cacheProbeResult
}

type cacheProbeResult struct {
	harborAccesses  []harborv1alpha1.HarborAccess
	listErr         error
	bridgeSecretErr error
	otherSecretErr  error
}

func (p *cacheProbe) Start(ctx context.Context) error {
	p.started.Store(true)
	var r cacheProbeResult
	var list harborv1alpha1.HarborAccessList
	r.listErr = p.reader.List(ctx, &list)
	r.harborAccesses = list.Items
	r.bridgeSecretErr = p.reader.Get(ctx, types.NamespacedName{Namespace: testNS, Name: "robot-probe"}, &corev1.Secret{})
	r.otherSecretErr = p.reader.Get(ctx, types.NamespacedName{Namespace: "tenant-a", Name: "tenant-secret"}, &corev1.Secret{})
	p.result <- r
	<-ctx.Done()
	return nil
}

func (p *cacheProbe) ReadyCheck(*http.Request) error {
	if !p.started.Load() {
		return errors.New("not started")
	}
	return nil
}

// startManager starts mgr and returns a channel with Start's result. The
// cleanup stops it.
func startManager(t *testing.T, mgr manager.Manager) (cancel context.CancelFunc, stopped <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

// freeAddr returns a loopback address with a port that was free a moment
// ago, for the manager's health probe server.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// probeReadiness returns the status of the manager's readiness check
// "probe": 200 ready, 500 not ready, 404 not registered, 0 no answer.
func probeReadiness(addr string) int {
	resp, err := http.Get("http://" + addr + "/readyz/probe")
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestEnvtest_InformersSyncBeforeNonLeaderRunnables pins ADR-0025 §2: the
// data-plane server, which runs on every replica, starts only after the
// HarborAccess and Secret caches have synced, and its ReadyCheck is the
// manager's readiness. controller-runtime creates informers lazily and
// waits only for those that exist when the manager starts; the
// reconciler's watches start later, on the leader. So without
// AddAfterCacheSync every replica turned ready with no informer at all.
// ReaderFailOnMissingInformer turns such a read into an error instead of a
// lazily started informer.
func TestEnvtest_InformersSyncBeforeNonLeaderRunnables(t *testing.T) {
	restCfg := setupEnvtest(t)
	ctx := context.Background()

	k8s, err := client.New(restCfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	for _, ns := range []string{testNS, "tenant-a"} {
		if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create namespace %s: %v", ns, err)
		}
	}
	ha := newHarborAccess()
	ha.Namespace = "tenant-a"
	ha.Finalizers = nil
	if err := k8s.Create(ctx, ha); err != nil {
		t.Fatalf("create HarborAccess: %v", err)
	}
	for _, s := range []*corev1.Secret{
		{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: "robot-probe"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "tenant-secret"}},
	} {
		if err := k8s.Create(ctx, s); err != nil {
			t.Fatalf("create Secret %s/%s: %v", s.Namespace, s.Name, err)
		}
	}

	cfg := testReconcilerConfig()
	cacheOpts := cfg.CacheOptions()
	cacheOpts.ReaderFailOnMissingInformer = true
	healthAddr := freeAddr(t)
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: healthAddr,
		Cache:                  cacheOpts,
		Controller:             config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	probe := &cacheProbe{reader: mgr.GetClient(), result: make(chan cacheProbeResult, 1)}
	if err := AddAfterCacheSync(mgr, "probe", probe, time.Minute); err != nil {
		t.Fatalf("AddAfterCacheSync: %v", err)
	}

	cancel, stopped := startManager(t, mgr)
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})

	var r cacheProbeResult
	select {
	case r = <-probe.result:
	case err := <-stopped:
		t.Fatalf("manager exited before the probe ran: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the probe never started")
	}

	if r.listErr != nil {
		t.Errorf("HarborAccess list when non-leader runnables start: %v", r.listErr)
	} else if len(r.harborAccesses) != 1 || r.harborAccesses[0].Name != ha.Name {
		t.Errorf("HarborAccess list when non-leader runnables start = %d objects, want the one created before the start", len(r.harborAccesses))
	}
	if r.bridgeSecretErr != nil {
		t.Errorf("Secret in the bridge namespace when non-leader runnables start: %v", r.bridgeSecretErr)
	}
	// ADR-0011: the Secret cache stays scoped to the bridge namespace.
	if r.otherSecretErr == nil {
		t.Error("a Secret outside the bridge namespace was readable through the cache; the cache must watch Secrets in the bridge namespace only")
	}
	if got := probeReadiness(healthAddr); got != http.StatusOK {
		t.Errorf("/readyz/probe after the probe started = %d, want %d", got, http.StatusOK)
	}
}

// newUnsyncableManager builds a manager as a user without any RBAC, so
// every LIST is forbidden and no informer ever syncs: what a replica sees
// when its RBAC is broken.
func newUnsyncableManager(t *testing.T, timeout time.Duration) (manager.Manager, *cacheProbe, string) {
	t.Helper()
	restCfg := rest.CopyConfig(setupEnvtest(t))
	restCfg.Impersonate = rest.ImpersonationConfig{UserName: "no-rbac"}
	healthAddr := freeAddr(t)
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: healthAddr,
		Cache:                  testReconcilerConfig().CacheOptions(),
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	probe := &cacheProbe{reader: mgr.GetClient(), result: make(chan cacheProbeResult, 1)}
	if err := AddAfterCacheSync(mgr, "probe", probe, timeout); err != nil {
		t.Fatalf("AddAfterCacheSync: %v", err)
	}
	return mgr, probe, healthAddr
}

// TestEnvtest_StopsWhileCachesCannotSync: SIGTERM must stop a replica whose
// caches cannot sync, promptly, and it must stay not ready meanwhile.
// Informers created before the manager starts put the wait into the
// manager's cache group, which ignores the cancelled context: Start never
// returned, spinning a CPU core until the kubelet killed the pod.
func TestEnvtest_StopsWhileCachesCannotSync(t *testing.T) {
	mgr, probe, healthAddr := newUnsyncableManager(t, time.Minute)
	cancel, stopped := startManager(t, mgr)

	deadline := time.Now().Add(3 * time.Second)
	answered := false
	for time.Now().Before(deadline) {
		switch got := probeReadiness(healthAddr); got {
		case 0:
		case http.StatusInternalServerError:
			answered = true
		default:
			t.Fatalf("/readyz/probe = %d while the caches cannot sync, want %d", got, http.StatusInternalServerError)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !answered {
		t.Fatal("/readyz/probe never answered")
	}
	if probe.started.Load() {
		t.Fatal("the runnable started although the caches never synced")
	}

	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("manager stopped with %v, want no error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not stop within 5s of cancel while the caches could not sync")
	}
}

// TestEnvtest_ExitsWhenCachesDoNotSyncInTime: a replica whose caches do not
// sync within the timeout exits with an error that says why, instead of
// staying up not ready with nothing in its logs but retries.
func TestEnvtest_ExitsWhenCachesDoNotSyncInTime(t *testing.T) {
	mgr, probe, _ := newUnsyncableManager(t, 2*time.Second)
	_, stopped := startManager(t, mgr)

	select {
	case err := <-stopped:
		if err == nil || !strings.Contains(err.Error(), "did not sync within 2s") {
			t.Errorf("manager stopped with %v, want the cache sync timeout", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("manager still running 30s after a 2s cache sync timeout")
	}
	if probe.started.Load() {
		t.Error("the runnable started although the caches never synced")
	}
}
