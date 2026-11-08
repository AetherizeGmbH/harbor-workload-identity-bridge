// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"

	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/dataplane"
)

// TestValidatorConfig_TokenPolicy follows the token policy from the env
// vars the chart renders to the validator's config (ADR-0028). The two
// pod-binding settings mean opposite things; an inverted mapping would
// accept tokens bound to no pod by default, and nothing else would notice:
// kubelet's tokens are pod-bound either way.
func TestValidatorConfig_TokenPolicy(t *testing.T) {
	const issuer = "https://kubernetes.default.svc"
	tests := []struct {
		name        string
		maxLifetime string // "" = unset
		requirePod  string // "" = unset
		jwksURL     string // "" = unset
		want        dataplane.Config
	}{
		{
			name: "defaults require pod binding and cap at 1h",
			want: dataplane.Config{Issuer: issuer, MaxTokenLifetime: time.Hour},
		},
		{
			name:       "pod binding required explicitly",
			requirePod: "true",
			want:       dataplane.Config{Issuer: issuer, MaxTokenLifetime: time.Hour},
		},
		{
			name:       "pod binding turned off",
			requirePod: "false",
			want:       dataplane.Config{Issuer: issuer, MaxTokenLifetime: time.Hour, AllowNonPodBoundTokens: true},
		},
		{
			name:        "custom maximum lifetime",
			maxLifetime: "90m",
			want:        dataplane.Config{Issuer: issuer, MaxTokenLifetime: 90 * time.Minute},
		},
		{
			name:    "JWKS URL override",
			jwksURL: "https://127.0.0.1:8001/openid/v1/jwks",
			want: dataplane.Config{
				Issuer:           issuer,
				JWKSURL:          "https://127.0.0.1:8001/openid/v1/jwks",
				MaxTokenLifetime: time.Hour,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// An empty value counts as unset for every variable below.
			for k, v := range map[string]string{
				controlplane.EnvClusterName:          "prod",
				controlplane.EnvNamespace:            "harbor-bridge-system",
				controlplane.EnvOIDCIssuer:           issuer,
				controlplane.EnvHarborURL:            "https://harbor.example.com",
				controlplane.EnvHarborAdminDir:       "/var/run/secrets/harbor-admin",
				controlplane.EnvAudience:             "harbor-bridge-prod",
				controlplane.EnvOIDCJWKSURL:          tt.jwksURL,
				controlplane.EnvTokenMaxLifetime:     tt.maxLifetime,
				controlplane.EnvRequirePodBoundToken: tt.requirePod,
			} {
				t.Setenv(k, v)
			}
			cfg, err := controlplane.LoadFromEnv()
			if err != nil {
				t.Fatalf("LoadFromEnv: %v", err)
			}
			if got := validatorConfig(cfg, nil); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("validatorConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// setBridgeEnv sets the variables LoadFromEnv requires, except the
// issuer, plus extra; an empty value counts as unset.
func setBridgeEnv(t *testing.T, extra map[string]string) {
	t.Helper()
	for k, v := range map[string]string{
		controlplane.EnvClusterName:    "prod",
		controlplane.EnvNamespace:      "harbor-bridge-system",
		controlplane.EnvOIDCIssuer:     "",
		controlplane.EnvOIDCJWKSURL:    "",
		controlplane.EnvHarborURL:      "https://harbor.example.com",
		controlplane.EnvHarborAdminDir: "/var/run/secrets/harbor-admin",
		controlplane.EnvAudience:       "harbor-bridge-prod",
	} {
		t.Setenv(k, v)
	}
	for k, v := range extra {
		t.Setenv(k, v)
	}
}

// fakeAPIServer serves a discovery document on an httptest TLS server and
// counts the requests for it. It returns the server and a bridge OIDC
// client that trusts it.
func fakeAPIServer(t *testing.T, status int, doc map[string]any) (*httptest.Server, *http.Client, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := dataplane.NewOIDCHTTPClient(caFile, "")
	if err != nil {
		t.Fatal(err)
	}
	return srv, client, &requests
}

// ADR-0044: without BRIDGE_OIDC_ISSUER the bridge reads the issuer from
// the apiserver and makes it the issuer of both planes (cfg.OIDCIssuer,
// which the reconciler's IssuerMismatch check and the startup log read)
// and of the validator, whose keys then come from the apiserver's own key
// endpoint unless BRIDGE_OIDC_JWKS_URL names another.
func TestDiscoverIssuer_EmptyIssuerReadsTheAPIServer(t *testing.T) {
	const issuer = "https://container.googleapis.com/v1/projects/p/locations/europe-west3/clusters/c"
	for _, tc := range []struct {
		name, jwksURL, wantJWKS string
	}{
		{name: "keys from the apiserver"},
		{name: "JWKS URL override", jwksURL: "https://127.0.0.1:8001/openid/v1/jwks", wantJWKS: "https://127.0.0.1:8001/openid/v1/jwks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, client, requests := fakeAPIServer(t, http.StatusOK, map[string]any{
				"issuer":                                issuer,
				"jwks_uri":                              issuer + "/jwks",
				"id_token_signing_alg_values_supported": []string{"ES256"},
			})
			setBridgeEnv(t, map[string]string{controlplane.EnvOIDCJWKSURL: tc.jwksURL})
			cfg, err := controlplane.LoadFromEnv()
			if err != nil {
				t.Fatalf("LoadFromEnv without an issuer: %v", err)
			}
			if cfg.OIDCIssuer != nil {
				t.Fatalf("OIDCIssuer = %s before discovery, want nil", cfg.OIDCIssuer)
			}

			d, err := discoverIssuer(context.Background(), cfg, client, srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if d == nil || requests.Load() != 1 {
				t.Fatalf("discovery = %+v after %d requests, want one read of the document", d, requests.Load())
			}
			if cfg.OIDCIssuer == nil || cfg.OIDCIssuer.String() != issuer {
				t.Fatalf("cfg.OIDCIssuer = %v, want %s", cfg.OIDCIssuer, issuer)
			}
			if got := cfg.Sanitized()[controlplane.EnvOIDCIssuer]; got != issuer {
				t.Errorf("startup log shows issuer %q, want %q", got, issuer)
			}
			wantJWKS := tc.wantJWKS
			if wantJWKS == "" {
				wantJWKS = srv.URL + "/openid/v1/jwks"
			}
			want := dataplane.Config{
				Issuer:           issuer,
				JWKSURL:          wantJWKS,
				SigningAlgs:      []string{"ES256"},
				MaxTokenLifetime: time.Hour,
			}
			if got := validatorConfig(cfg, d); !reflect.DeepEqual(got, want) {
				t.Errorf("validatorConfig() = %+v, want %+v", got, want)
			}
		})
	}
}

// A set issuer works as before: nothing is read from the apiserver.
func TestDiscoverIssuer_SetIssuerIsKept(t *testing.T) {
	const issuer = "https://kubernetes.default.svc.cluster.local"
	srv, client, requests := fakeAPIServer(t, http.StatusOK, map[string]any{"issuer": "https://other.example"})
	setBridgeEnv(t, map[string]string{controlplane.EnvOIDCIssuer: issuer})
	cfg, err := controlplane.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	d, err := discoverIssuer(context.Background(), cfg, client, srv.URL)
	if err != nil || d != nil {
		t.Fatalf("discoverIssuer = %+v, %v; want nothing", d, err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("%d requests to the apiserver, want none", n)
	}
	if cfg.OIDCIssuer.String() != issuer {
		t.Fatalf("cfg.OIDCIssuer = %s, want %s", cfg.OIDCIssuer, issuer)
	}
	want := dataplane.Config{Issuer: issuer, MaxTokenLifetime: time.Hour}
	if got := validatorConfig(cfg, d); !reflect.DeepEqual(got, want) {
		t.Errorf("validatorConfig() = %+v, want %+v", got, want)
	}
}

// A failed read stops the bridge with an error that says what to do, and
// leaves no issuer behind.
func TestDiscoverIssuer_FailureNamesTheFix(t *testing.T) {
	srv, client, _ := fakeAPIServer(t, http.StatusForbidden, map[string]any{"kind": "Status", "reason": "Forbidden"})
	setBridgeEnv(t, nil)
	cfg, err := controlplane.LoadFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	_, err = discoverIssuer(context.Background(), cfg, client, srv.URL)
	if err == nil {
		t.Fatal("discoverIssuer succeeded on a 403")
	}
	for _, want := range []string{
		"BRIDGE_OIDC_ISSUER is empty", "403 Forbidden", "/.well-known/openid-configuration",
		"ClusterRole", "bridge.oidcIssuer", "kubectl get --raw /.well-known/openid-configuration",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not contain %q: %v", want, err)
		}
	}
	if cfg.OIDCIssuer != nil {
		t.Errorf("cfg.OIDCIssuer = %s after a failed read, want nil", cfg.OIDCIssuer)
	}
}

// The data plane refuses a robot Secret whose username is not this one,
// so it must be exactly what Harbor reports for the robot the reconciler
// creates: the configured prefix plus harbor.RobotName.
func TestRobotUsername(t *testing.T) {
	for _, tc := range []struct {
		prefix, want string
	}{
		{"robot$", "robot$bridge-prod.flux-system.source-controller"},
		{"svc+", "svc+bridge-prod.flux-system.source-controller"},
	} {
		got, err := robotUsername(&controlplane.Config{ClusterName: "prod", HarborRobotPrefix: tc.prefix})("flux-system", "source-controller")
		if err != nil || got != tc.want {
			t.Errorf("prefix %q: robotUsername = %q, %v; want %q", tc.prefix, got, err, tc.want)
		}
	}
}

func TestLogWeakTokenValidation(t *testing.T) {
	const (
		unbound = "tokens not bound to a pod are accepted"
		tooLow  = "below the lifetime of kubelet's tokens"
	)
	tests := []struct {
		name string
		vc   dataplane.Config
		want []string // one fragment per expected line, in order
	}{
		{name: "defaults", vc: dataplane.Config{MaxTokenLifetime: time.Hour}},
		{name: "longer cap", vc: dataplane.Config{MaxTokenLifetime: 2 * time.Hour}},
		{
			name: "unbound tokens accepted",
			vc:   dataplane.Config{MaxTokenLifetime: time.Hour, AllowNonPodBoundTokens: true},
			want: []string{unbound},
		},
		{
			name: "cap below kubelet's tokens",
			vc:   dataplane.Config{MaxTokenLifetime: time.Hour - time.Second},
			want: []string{tooLow},
		},
		{
			name: "both",
			vc:   dataplane.Config{MaxTokenLifetime: 30 * time.Minute, AllowNonPodBoundTokens: true},
			want: []string{unbound, tooLow},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lines []string
			log := funcr.New(func(_, args string) { lines = append(lines, args) }, funcr.Options{})
			logWeakTokenValidation(log, tt.vc)
			if len(lines) != len(tt.want) {
				t.Fatalf("logged %d lines, want %d: %q", len(lines), len(tt.want), lines)
			}
			for i, fragment := range tt.want {
				if !strings.Contains(lines[i], fragment) {
					t.Errorf("line %d = %q, want it to contain %q", i, lines[i], fragment)
				}
			}
		})
	}
}

// TestCredentialMux_ServesOnlyTheCredentialEndpoint pins what the NodePort
// exposes on every node: the credential endpoint and nothing else
// (ADR-0025). An unauthenticated /healthz used to sit next to it, outside
// the per-source rate limit, unused by any probe.
func TestCredentialMux_ServesOnlyTheCredentialEndpoint(t *testing.T) {
	credentials := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	mux := credentialMux(credentials)
	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, dataplane.CredentialsPath, http.StatusTeapot},
		{http.MethodGet, "/healthz", http.StatusNotFound},
		{http.MethodGet, "/readyz", http.StatusNotFound},
		{http.MethodGet, "/metrics", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
		if rec.Code != tt.want {
			t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.want)
		}
	}
}

// TestLeaderElectionFromEnv: a value that is not a boolean must fail
// startup. It used to mean "off", so `--set bridge.leaderElection=on` or a
// quoted "True" ran every replica as a leader: two reconcilers and two
// janitors (ADR-0025 keeps both leader-only).
func TestLeaderElectionFromEnv(t *testing.T) {
	tests := []struct {
		raw     string
		want    bool
		wantErr bool
	}{
		{raw: "", want: false},
		{raw: "true", want: true},
		{raw: "false", want: false},
		{raw: "True", want: true},
		{raw: "TRUE", want: true},
		{raw: "1", want: true},
		{raw: "0", want: false},
		{raw: " true\n", want: true},
		{raw: "yes", want: true},
		{raw: "no", want: false},
		{raw: "on", wantErr: true},
		{raw: "off", wantErr: true},
		{raw: "enabled", wantErr: true},
		{raw: "Yes", wantErr: true},
		{raw: "tru", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Setenv(envEnableLeaderElec, tt.raw)
			got, err := leaderElectionFromEnv()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("leaderElectionFromEnv() = %v, want an error", got)
				}
				if !strings.Contains(err.Error(), envEnableLeaderElec) {
					t.Errorf("error %q does not name %s", err, envEnableLeaderElec)
				}
				return
			}
			if err != nil {
				t.Fatalf("leaderElectionFromEnv(): %v", err)
			}
			if got != tt.want {
				t.Errorf("leaderElectionFromEnv() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShutdownDelayFromEnv(t *testing.T) {
	tests := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{raw: "", want: 5 * time.Second},
		{raw: "0", want: 0},
		{raw: "10s", want: 10 * time.Second},
		{raw: " 1500ms ", want: 1500 * time.Millisecond},
		{raw: "15s", want: 15 * time.Second},
		{raw: "15.001s", wantErr: true},
		{raw: "30s", wantErr: true},
		{raw: "-1s", wantErr: true},
		{raw: "5", wantErr: true},
		{raw: "soon", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Setenv(envShutdownDelay, tt.raw)
			got, err := shutdownDelayFromEnv()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("shutdownDelayFromEnv() = %s, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("shutdownDelayFromEnv(): %v", err)
			}
			if got != tt.want {
				t.Errorf("shutdownDelayFromEnv() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestManagerOptions_LeaderElection: a stopping leader must hand the Lease
// back, or reconciles and janitor sweeps pause for the lease duration on
// every rollout of the leader.
func TestManagerOptions_LeaderElection(t *testing.T) {
	cfg := &controlplane.Config{Namespace: "harbor-bridge-system"}
	for _, enabled := range []bool{true, false} {
		opts := managerOptions(cfg, enabled)
		if opts.LeaderElection != enabled {
			t.Errorf("LeaderElection = %v, want %v", opts.LeaderElection, enabled)
		}
		if !opts.LeaderElectionReleaseOnCancel {
			t.Error("LeaderElectionReleaseOnCancel = false, want true")
		}
		if opts.LeaderElectionID != leaderElectionID || opts.LeaderElectionNamespace != cfg.Namespace {
			t.Errorf("Lease = %s/%s, want %s/%s", opts.LeaderElectionNamespace, opts.LeaderElectionID, cfg.Namespace, leaderElectionID)
		}
	}
}

// TestShutdownBudget: after SIGTERM the manager stops the credential
// listener first (shutdown delay, then its graceful shutdown) and the
// reconciler and the janitor after it, all within its graceful shutdown
// timeout, which is also when kubelet kills the pod. The longest accepted
// delay must still leave the leader's runnables their budget; otherwise
// they are cut off mid-reconcile and the Lease is released while they run.
func TestShutdownBudget(t *testing.T) {
	opts := managerOptions(&controlplane.Config{Namespace: "harbor-bridge-system"}, true)
	if opts.GracefulShutdownTimeout == nil || *opts.GracefulShutdownTimeout != 30*time.Second {
		t.Fatalf("GracefulShutdownTimeout = %v, want 30s, the pod's default termination grace period", opts.GracefulShutdownTimeout)
	}
	t.Setenv(envShutdownDelay, maxShutdownDelay.String())
	delay, err := shutdownDelayFromEnv()
	if err != nil {
		t.Fatalf("the longest shutdown delay is refused: %v", err)
	}
	sc := serverConfig(http.NotFoundHandler(), delay)
	if sc.ShutdownDelay != delay {
		t.Errorf("ShutdownDelay = %s, want %s", sc.ShutdownDelay, delay)
	}
	if sc.ShutdownTimeout <= 0 {
		t.Fatalf("ShutdownTimeout = %s; the server's default is not part of the budget", sc.ShutdownTimeout)
	}
	if left := *opts.GracefulShutdownTimeout - sc.ShutdownDelay - sc.ShutdownTimeout; left < leaderStopBudget {
		t.Errorf("the longest shutdown delay (%s) and the listener's shutdown (%s) leave %s of %s for the reconciler and the janitor, want at least %s",
			sc.ShutdownDelay, sc.ShutdownTimeout, left, *opts.GracefulShutdownTimeout, leaderStopBudget)
	}
}

// TestNewHarborClient_ReadsRotatedAdminCredentials: the operator rotates
// the Harbor admin password (or the system robot's secret) and updates the
// mounted Secret; the next Harbor call must carry the new password without
// a restart. It used to be read once at startup, so every call failed with
// 401 and HarborAccess deletions hung in DeletionBlocked until the pods
// were restarted by hand.
func TestNewHarborClient_ReadsRotatedAdminCredentials(t *testing.T) {
	var mu sync.Mutex
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	harborURL, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(key, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, key), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("username", "admin")
	write("password", "before")

	c, err := newHarborClient(&controlplane.Config{HarborURL: harborURL, HarborAdminDir: dir}, logr.Discard())
	if err != nil {
		t.Fatalf("newHarborClient: %v", err)
	}
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	write("password", "after")
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("List after rotation: %v", err)
	}

	basic := func(p string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:"+p)) }
	mu.Lock()
	defer mu.Unlock()
	if len(auth) != 2 || auth[0] != basic("before") || auth[1] != basic("after") {
		t.Fatalf("Authorization headers = %q, want %q", auth, []string{basic("before"), basic("after")})
	}
}

func TestNewHarborClient_FailsWithoutAdminCredentials(t *testing.T) {
	harborURL, err := url.Parse("https://harbor.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newHarborClient(&controlplane.Config{HarborURL: harborURL, HarborAdminDir: t.TempDir()}, logr.Discard()); err == nil {
		t.Fatal("newHarborClient succeeded with an empty admin directory")
	}
}
