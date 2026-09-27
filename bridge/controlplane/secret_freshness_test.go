// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/robotsecret"
)

// A password rotation in Harbor cannot be undone, so the pass that decides
// to rotate must see the robot Secret as it is. These tests pin that the
// reconciler reads it uncached and never drops a password Harbor already
// switched to.

var testSecretKey = types.NamespacedName{Namespace: testNS, Name: robotsecret.Name(testHANamespace, testHAName)}

// staleSecretClient serves reads of the Secrets in snapshot from that
// frozen state, the way an informer cache does before it has seen the
// latest write; a nil entry means "not in the cache yet". Everything else
// goes to the wrapped client.
type staleSecretClient struct {
	client.Client
	snapshot map[types.NamespacedName]*corev1.Secret
}

func (c *staleSecretClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if s, ok := obj.(*corev1.Secret); ok {
		if snap, frozen := c.snapshot[key]; frozen {
			if snap == nil {
				return apierrors.NewNotFound(corev1.Resource("secrets"), key.Name)
			}
			snap.DeepCopyInto(s)
			return nil
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// harborPassword returns the password Harbor currently accepts for the
// robot with the given ID.
func (m *mockHarbor) harborPassword(id int64) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.robots[id]; ok {
		return r.Secret
	}
	return ""
}

func getTestSecret(t *testing.T, c client.Reader) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(context.Background(), testSecretKey, s); err != nil {
		t.Fatalf("get robot Secret: %v", err)
	}
	return s
}

// The pass right after the one that created the robot and its Secret can
// run before the Secret has reached the cache (the finalizer patch queues
// it). It must not take the Secret for missing and rotate.
func TestReconcile_StaleCacheAfterCreate_DoesNotRotate(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	r := newReconciler(t, mh, fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}, ha)
	live := r.Client
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	id := mh.createCalls0ID()

	r.Client = &staleSecretClient{Client: live, snapshot: map[types.NamespacedName]*corev1.Secret{testSecretKey: nil}}
	r.APIReader = live
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatalf("pass on a lagging cache: %v", err)
	}
	r.Client = live
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}

	if len(mh.refreshCalls) != 0 {
		t.Errorf("rotated on a lagging cache: refresh calls %v", mh.refreshCalls)
	}
	if got, want := string(getTestSecret(t, live).Data["password"]), mh.harborPassword(id); got != want {
		t.Errorf("stored password %q, Harbor accepts %q", got, want)
	}
}

// The pass right after a scheduled rotation can see the Secret as it was
// before the rotation, with a rotation-not-before already in the past. It
// must not rotate a second time.
func TestReconcile_StaleCacheAfterRotation_DoesNotRotateAgain(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	t0 := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	r := newReconciler(t, mh, fixedClock{t0}, ha)
	live := r.Client
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	id := mh.createCalls0ID()
	beforeRotation := getTestSecret(t, live)

	r.Clock = fixedClock{t0.Add(PasswordRotationInterval + RotationSafetyMargin)}
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	if len(mh.refreshCalls) != 1 {
		t.Fatalf("setup: scheduled rotation did not happen: %v", mh.refreshCalls)
	}

	r.Client = &staleSecretClient{Client: live, snapshot: map[types.NamespacedName]*corev1.Secret{testSecretKey: beforeRotation}}
	r.APIReader = live
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatalf("pass on a lagging cache: %v", err)
	}
	r.Client = live
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}

	if len(mh.refreshCalls) != 1 {
		t.Errorf("rotated again on a lagging cache: refresh calls %v", mh.refreshCalls)
	}
	if got, want := string(getTestSecret(t, live).Data["password"]), mh.harborPassword(id); got != want {
		t.Errorf("stored password %q, Harbor accepts %q", got, want)
	}
}

// newRacingReconciler wires the reconciler to a fake client whose Secret
// writes go through funcs, to inject a concurrent writer.
func newRacingReconciler(t *testing.T, mh *mockHarbor, clock Clock, funcs interceptor.Funcs, objects ...client.Object) *Reconciler {
	t.Helper()
	r := newReconciler(t, mh, clock)
	r.Client = fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objects...).
		WithStatusSubresource(&harborv1alpha1.HarborAccess{}).
		WithInterceptorFuncs(funcs).
		Build()
	return r
}

// Someone else modifies the Secret between the pass's read and its write
// of a freshly rotated password. The password is the only one Harbor
// accepts: it must be stored on top of the other writer's version.
func TestReconcile_KeepsRotatedPasswordWhenSecretChangesConcurrently(t *testing.T) {
	ha := newHarborAccess()
	mh := newMockHarbor()
	t0 := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	raced := false
	r := newRacingReconciler(t, mh, fixedClock{t0}, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if s, ok := obj.(*corev1.Secret); ok && !raced && len(mh.refreshCalls) > 0 {
				raced = true
				other := &corev1.Secret{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(s), other); err != nil {
					return err
				}
				other.Annotations["example.com/touched-by"] = "someone-else"
				if err := c.Update(ctx, other); err != nil {
					return err
				}
			}
			return c.Update(ctx, obj, opts...)
		},
	}, ha)
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatal(err)
	}
	id := mh.createCalls0ID()

	r.Clock = fixedClock{t0.Add(PasswordRotationInterval + RotationSafetyMargin)}
	if _, err := r.Reconcile(context.Background(), reqFor(ha)); err != nil {
		t.Fatalf("rotation pass: %v", err)
	}
	if !raced {
		t.Fatal("setup: the concurrent write was not injected")
	}
	s := getTestSecret(t, r.Client)
	if got, want := string(s.Data["password"]), mh.harborPassword(id); got != want {
		t.Errorf("stored password %q, Harbor accepts %q", got, want)
	}
	if s.Annotations["example.com/touched-by"] != "someone-else" {
		t.Error("the other writer's change was overwritten instead of built upon")
	}
	if nb, _ := robotsecret.RotationNotBefore(s); !nb.After(t0.Add(PasswordRotationInterval)) {
		t.Errorf("rotation-not-before %s not advanced", nb)
	}
}

// The Secret is missing, the pass rebuilds it with a new password, and a
// Secret appears at that name before the Create lands. If it belongs to
// this HarborAccess the password goes on top of it; if it belongs to
// someone else it is left alone.
func TestReconcile_SecretCreatedConcurrently(t *testing.T) {
	for _, tc := range []struct {
		name      string
		appeared  func() *corev1.Secret
		wantStore bool
	}{
		{
			name: "stamped for this HarborAccess",
			appeared: func() *corev1.Secret {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testSecretKey.Name,
					Labels: robotsecret.Labels(testCluster, testHANamespace, testHAName)}}
			},
			wantStore: true,
		},
		{
			name: "someone else's unmanaged Secret",
			appeared: func() *corev1.Secret {
				return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNS, Name: testSecretKey.Name},
					Data: map[string][]byte{"username": []byte("someone-else"), "password": []byte("theirs")}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ha := newHarborAccess()
			mh := newMockHarbor()
			id := mh.preexisting("bridge-prod-eu-west.flux-system.source-controller",
				RobotDescription(testCluster, testHANamespace, testHAName),
				harbor.ProjectPermission{Project: "production", Action: "pull"})
			raced := false
			r := newRacingReconciler(t, mh, fixedClock{time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)}, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*corev1.Secret); ok && !raced {
						raced = true
						if err := c.Create(ctx, tc.appeared()); err != nil {
							return err
						}
					}
					return c.Create(ctx, obj, opts...)
				},
			}, ha)

			_, err := r.Reconcile(context.Background(), reqFor(ha))
			if !raced {
				t.Fatal("setup: the concurrent create was not injected")
			}
			s := getTestSecret(t, r.Client)
			if tc.wantStore {
				if err != nil {
					t.Fatalf("reconcile: %v", err)
				}
				if got, want := string(s.Data["password"]), mh.harborPassword(id); got != want || want == "" {
					t.Errorf("stored password %q, Harbor accepts %q", got, want)
				}
				return
			}
			if err == nil {
				t.Error("stored the password into someone else's Secret without an error")
			}
			if string(s.Data["password"]) != "theirs" {
				t.Errorf("someone else's Secret overwritten: %q", s.Data["password"])
			}
		})
	}
}

// SetupWithManager wires the uncached reader, so production never decides
// a rotation from the cache.
func TestSetupWithManager_WiresUncachedSecretReader(t *testing.T) {
	mgr, err := ctrl.NewManager(&rest.Config{Host: "https://127.0.0.1:1"}, ctrl.Options{
		Scheme:  testScheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		// The controller name is registered process-wide; envtest tests
		// in this package register it too.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &Reconciler{Client: mgr.GetClient(), Scheme: testScheme, Harbor: newMockHarbor(), Config: testReconcilerConfig()}
	if err := rec.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	if rec.APIReader != mgr.GetAPIReader() {
		t.Errorf("APIReader = %T, want the manager's uncached API reader", rec.APIReader)
	}
}
