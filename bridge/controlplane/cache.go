// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
)

// CacheSyncTimeout bounds how long a replica waits at startup for the
// HarborAccess and Secret caches before it exits with an error. It is
// controller-runtime's default for a controller's watches, so the leader's
// reconciler and every replica's data plane give up after the same time.
const CacheSyncTimeout = 2 * time.Minute

// CacheOptions scopes the manager's cache to what the bridge reads, which
// keeps its RBAC minimal. HarborAccess is namespaced, but its objects may
// live in any namespace, so the cache watches it cluster-wide, limited by
// the selector when one is set (ADR-0026). Secrets are read from the bridge
// namespace only (ADR-0011); without the override the cache would watch
// them cluster-wide and need cluster-scoped Secret RBAC.
//
// With the Nexus backend (ADR-0036) NexusAccess is cached the same way as
// HarborAccess, limited by the same selector.
func (c *Config) CacheOptions() cache.Options {
	opts := cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Namespaces: map[string]cache.Config{c.Namespace: {}},
			},
			// nil keeps every object.
			&harborv1alpha1.HarborAccess{}: {Label: c.HarborAccessSelector},
		},
	}
	if c.Nexus != nil {
		opts.ByObject[&nexusv1alpha1.NexusAccess{}] = cache.ByObject{Label: c.HarborAccessSelector}
	}
	return opts
}

// cachedObjects are the types the bridge reads through the manager's
// cache: the reconciler watches both, and the data-plane handler lists
// HarborAccess objects and gets robot Secrets. A type either of them starts
// reading through the cache belongs here.
func cachedObjects() []client.Object {
	return []client.Object{&harborv1alpha1.HarborAccess{}, &corev1.Secret{}}
}

// CachedObjects returns the types this configuration reads through the
// manager's cache: cachedObjects, plus NexusAccess with the Nexus backend.
// Pass them to AddAfterCacheSync so that readiness waits for all of them.
func (c *Config) CachedObjects() []client.Object {
	objs := cachedObjects()
	if c.Nexus != nil {
		objs = append(objs, &nexusv1alpha1.NexusAccess{})
	}
	return objs
}

// ReadyRunnable is a runnable that reports when it can serve, such as the
// data-plane server once its listener is bound.
type ReadyRunnable interface {
	manager.Runnable
	ReadyCheck(*http.Request) error
}

// AddAfterCacheSync adds r to mgr so that it runs on every replica, not
// only the leader, and starts only once the caches of cachedObjects have
// synced; r's ReadyCheck becomes the manager's readiness check name. A
// replica therefore turns ready only when it can answer from a synced cache
// (ADR-0025).
//
// controller-runtime creates informers lazily and, before it starts
// non-leader runnables, waits only for the informers that exist when the
// manager starts; the reconciler's watches start later, on the leader only.
// Creating the informers before the manager starts would make it wait, but
// that wait ignores SIGTERM: informers that cannot sync (RBAC or CRD
// broken, apiserver unreachable) would keep the process spinning until it
// is killed. So the runnable waits itself. SIGTERM ends the wait, and a
// replica whose caches have not synced within timeout exits with an error.
//
// objects are the types whose caches must sync (Config.CachedObjects);
// none means cachedObjects.
func AddAfterCacheSync(mgr manager.Manager, name string, r ReadyRunnable, timeout time.Duration, objects ...client.Object) error {
	if len(objects) == 0 {
		objects = cachedObjects()
	}
	if err := mgr.Add(&afterCacheSync{informers: mgr.GetCache(), runnable: r, timeout: timeout, objects: objects}); err != nil {
		return fmt.Errorf("add %s: %w", name, err)
	}
	if err := mgr.AddReadyzCheck(name, r.ReadyCheck); err != nil {
		return fmt.Errorf("add %s readiness check: %w", name, err)
	}
	return nil
}

// afterCacheSync starts runnable once the caches of objects have synced.
type afterCacheSync struct {
	informers cache.Informers
	runnable  manager.Runnable
	timeout   time.Duration
	objects   []client.Object
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: the data
// plane serves on every replica.
func (a *afterCacheSync) NeedLeaderElection() bool { return false }

func (a *afterCacheSync) Start(ctx context.Context) error {
	if err := waitForCacheSync(ctx, a.informers, a.timeout, a.objects); err != nil {
		if stopped := ctx.Err(); stopped != nil {
			// Stopped before the caches synced; nothing was started. The
			// manager expects context.Canceled on the way out.
			return stopped
		}
		return err
	}
	return a.runnable.Start(ctx)
}

// waitForCacheSync creates the informers of objects and waits until they
// have synced, ctx is done, or timeout has passed.
func waitForCacheSync(ctx context.Context, informers cache.Informers, timeout time.Duration, objects []client.Object) error {
	what := "the HarborAccess and Secret caches"
	if len(objects) != len(cachedObjects()) {
		what = fmt.Sprintf("the caches of %d types", len(objects))
	}
	log.FromContext(ctx).Info("waiting for "+what+" to sync", "timeout", timeout.String())
	syncCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, obj := range objects {
		// GetInformer waits for the sync only once the cache has started;
		// waiting here as well makes the order irrelevant.
		inf, err := informers.GetInformer(syncCtx, obj, cache.BlockUntilSynced(false))
		if err != nil {
			return fmt.Errorf("start the %T cache: %w", obj, err)
		}
		if !toolscache.WaitForCacheSync(syncCtx.Done(), inf.HasSynced) {
			crd := "HarborAccess"
			if _, ok := obj.(*nexusv1alpha1.NexusAccess); ok {
				crd = "NexusAccess"
			}
			return fmt.Errorf("the %T cache did not sync within %s; check the bridge's RBAC and the %s CRD", obj, timeout, crd)
		}
	}
	return nil
}
