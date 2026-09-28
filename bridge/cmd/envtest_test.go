// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus/nexustest"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/dataplane"
)

// startEnvtest starts a kube-apiserver and etcd with the project's CRDs.
// It skips when KUBEBUILDER_ASSETS is unset; `make envtest` sets it.
func startEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via `make envtest` to install kube-apiserver+etcd binaries")
	}
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

// writeServingCert writes a self-signed serving certificate for 127.0.0.1
// and returns the paths and a pool that trusts it.
func writeServingCert(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "bridge-envtest"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeFile(t, certFile, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, keyFile, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// emptyHarbor is a Harbor without robots: the Harbor side of the bridge
// runs, and finds nothing to do.
type emptyHarbor struct{}

func (emptyHarbor) Create(context.Context, string, string, []harbor.ProjectPermission) (*harbor.Robot, error) {
	return nil, apierrors.NewServiceUnavailable("no Harbor in this test")
}
func (emptyHarbor) Delete(context.Context, int64) error                      { return nil }
func (emptyHarbor) List(context.Context) ([]harbor.Robot, error)             { return nil, nil }
func (emptyHarbor) GetByName(context.Context, string) (*harbor.Robot, error) { return nil, nil }
func (emptyHarbor) RefreshSecret(context.Context, int64) (string, error)     { return "", nil }
func (emptyHarbor) Update(context.Context, *harbor.Robot, string, []harbor.ProjectPermission) error {
	return nil
}

// fixedValidator accepts every token as the given claims.
type fixedValidator struct{ claims dataplane.Claims }

func (v fixedValidator) Validate(context.Context, string) (*dataplane.Claims, error) {
	c := v.claims
	return &c, nil
}

type auditLines struct {
	mu    sync.Mutex
	lines []string
}

func (a *auditLines) logger() logr.Logger {
	return funcr.New(func(_, args string) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.lines = append(a.lines, args)
	}, funcr.Options{})
}

func (a *auditLines) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.lines) == 0 {
		return ""
	}
	return a.lines[len(a.lines)-1]
}

// TestEnvtest_NexusBackendWiring runs the bridge as run() wires it, with
// the Nexus backend, against an apiserver and a fake Nexus: the scheme and
// the cache options, the control plane (SetupNexus), and the data plane
// (the NexusAccess index, readiness after the NexusAccess cache synced,
// routing by registry host). The credentials the data plane serves over
// TLS must be the ones the real reconciler created in Nexus, and every
// refusal the Secret contract promises must reach the kubelet's plugin.
func TestEnvtest_NexusBackendWiring(t *testing.T) {
	restCfg := startEnvtest(t)
	ctx := context.Background()

	const (
		bridgeNS = "bridge-system"
		tenantNS = "team-a"
		audience = "bridge-envtest"
		issuer   = "https://kubernetes.default.svc"
		image    = "nexus.example.com:8443/docker-hosted/app:v1"
	)
	nx := nexustest.New(t)
	nx.AddRepository("docker", "docker-hosted")

	harborAdmin, nexusAdmin := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(harborAdmin, "username"), "admin")
	writeFile(t, filepath.Join(harborAdmin, "password"), "harbor-password")
	writeFile(t, filepath.Join(nexusAdmin, "username"), nexustest.AdminUsername)
	writeFile(t, filepath.Join(nexusAdmin, "password"), nexustest.AdminPassword)
	certFile, keyFile, pool := writeServingCert(t)

	env := nexusEnv()
	env[controlplane.EnvNamespace] = bridgeNS
	env[controlplane.EnvAudience] = audience
	env[controlplane.EnvHarborAdminDir] = harborAdmin
	env[controlplane.EnvNexusURL] = nx.URL.String()
	env[controlplane.EnvNexusAllowHTTP] = "true"
	env[controlplane.EnvNexusAdminDir] = nexusAdmin
	env[controlplane.EnvNexusRegistryHosts] = "nexus.example.com:8443"
	cfg := loadConfig(t, env)
	for k, v := range map[string]string{
		envMetricsAddr: "0", envHealthAddr: "0", envListenAddr: "127.0.0.1:0",
		envTLSCertFile: certFile, envTLSKeyFile: keyFile, envTLSClientCAFile: "",
	} {
		t.Setenv(k, v)
	}

	k8s, err := client.New(restCfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := addToScheme(clientgoscheme.Scheme, cfg); err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{bridgeNS, tenantNS} {
		if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}

	opts := managerOptions(cfg, false)
	// The controller names are registered process-wide.
	opts.Controller.SkipNameValidation = ptr.To(true)
	mgr, err := ctrl.NewManager(restCfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := setupControlPlane(mgr, cfg, emptyHarbor{}, logr.Discard(), prometheus.NewRegistry()); err != nil {
		t.Fatal(err)
	}
	audit := &auditLines{}
	handler := &dataplane.Handler{
		K8sClient: mgr.GetClient(),
		Validator: fixedValidator{claims: dataplane.Claims{
			Subject: "system:serviceaccount:team-a:web", Audience: []string{audience}, Issuer: issuer,
			Expiry: time.Now().Add(time.Hour),
		}},
		Config:  handlerConfig(cfg),
		Metrics: newMetrics(cfg, prometheus.NewRegistry()),
		Audit:   audit.logger(),
	}
	mgrCtx, cancel := context.WithCancel(ctx)
	server, err := addDataPlane(mgrCtx, mgr, cfg, handler, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() { cancel(); <-done })

	eventually(t, "the data plane is ready", func() bool { return server.ReadyCheck(nil) == nil })

	httpClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
	pull := func(image string) (int, dataplane.Response) {
		body, err := json.Marshal(dataplane.Request{Image: image})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPost, "https://"+server.Addr()+dataplane.CredentialsPath, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer token")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		var out dataplane.Response
		if resp.StatusCode == http.StatusOK {
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("decode %s: %v", raw, err)
			}
		}
		return resp.StatusCode, out
	}

	// Nothing to serve yet.
	if code, _ := pull(image); code != http.StatusForbidden {
		t.Fatalf("status before any NexusAccess = %d, want 403", code)
	}

	nxa := &nexusv1alpha1.NexusAccess{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: tenantNS},
		Spec: nexusv1alpha1.NexusAccessSpec{
			ServiceAccountRef: nexusv1alpha1.ServiceAccountRef{Namespace: tenantNS, Name: "web"},
			TrustPolicy:       nexusv1alpha1.TrustPolicy{Issuer: issuer, Audience: audience},
			Repositories: []nexusv1alpha1.RepositoryGrant{
				{Name: "docker-hosted", Access: nexusv1alpha1.AccessPull},
			},
			TokenTTL: harborv1alpha1.Duration{Duration: time.Hour},
		},
	}
	if err := k8s.Create(ctx, nxa); err != nil {
		t.Fatal(err)
	}

	// The reconciler creates the role and the user and writes the
	// Secret; the data plane then serves exactly that user's password.
	var served dataplane.Response
	eventually(t, "the data plane serves the NexusAccess's user", func() bool {
		code, resp := pull(image)
		served = resp
		return code == http.StatusOK
	})
	if !nx.Authenticate(served.Username, served.Password) {
		t.Fatalf("the data plane served %q with a password Nexus does not accept", served.Username)
	}
	if served.ExpiresInSecs != 3600 || served.CacheKeyType != "Registry" {
		t.Errorf("response = expires_in %d, cache_key_type %q; want 3600, Registry", served.ExpiresInSecs, served.CacheKeyType)
	}
	if line := audit.last(); !strings.Contains(line, `"access_kind"="nexus"`) || !strings.Contains(line, `"nexus_user"="`+served.Username+`"`) {
		t.Errorf("issuance audit line = %s", line)
	}

	// Routing: the same ServiceAccount has no HarborAccess, and an image
	// of neither backend gets nothing.
	for img, reason := range map[string]string{
		"harbor.example.com/production/app:v1": "no_matching_harboraccess",
		"docker.io/library/nginx:1.27":         "no_backend",
	} {
		if code, _ := pull(img); code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", img, code)
		}
		if line := audit.last(); !strings.Contains(line, `"reason"="`+reason+`"`) {
			t.Errorf("%s: audit line %s, want reason %s", img, line, reason)
		}
	}

	// A repository that does not exist: the reconciler marks the Secret,
	// and the data plane refuses (ADR-0036 decision d).
	editNXA := func(edit func(*nexusv1alpha1.NexusAccess)) {
		t.Helper()
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			cur := &nexusv1alpha1.NexusAccess{}
			if err := k8s.Get(ctx, client.ObjectKeyFromObject(nxa), cur); err != nil {
				return err
			}
			edit(cur)
			return k8s.Update(ctx, cur)
		}); err != nil {
			t.Fatal(err)
		}
	}
	editNXA(func(n *nexusv1alpha1.NexusAccess) {
		n.Spec.Repositories = append(n.Spec.Repositories, nexusv1alpha1.RepositoryGrant{Name: "absent", Access: nexusv1alpha1.AccessPull})
	})
	eventually(t, "the data plane refuses while a repository is missing", func() bool {
		code, _ := pull(image)
		return code == http.StatusForbidden && strings.Contains(audit.last(), `"reason"="grants_incomplete"`)
	})
	if line := audit.last(); !strings.Contains(line, `"missing_repositories"="absent"`) {
		t.Errorf("audit line %s does not name the missing repository", line)
	}

	// Once the spec no longer names it, the same user is served again.
	editNXA(func(n *nexusv1alpha1.NexusAccess) { n.Spec.Repositories = n.Spec.Repositories[:1] })
	eventually(t, "the data plane serves again", func() bool {
		code, resp := pull(image)
		return code == http.StatusOK && resp.Username == served.Username
	})

	// Deleting the NexusAccess is the revocation.
	if err := k8s.Delete(ctx, nxa); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the data plane refuses the deleted NexusAccess", func() bool {
		code, _ := pull(image)
		return code == http.StatusForbidden
	})
	eventually(t, "the reconciler deletes the user", func() bool {
		_, exists := nx.User(served.Username)
		return !exists
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting until %s", what)
}
