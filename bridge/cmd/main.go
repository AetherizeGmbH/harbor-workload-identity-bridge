// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Command bridge is the entry point of the Harbor Workload Identity Bridge.
// It composes:
//
//   - the control-plane Reconciler (HarborAccess → persistent Harbor robot)
//   - the orphan-robot Janitor
//   - the data-plane OIDC Validator and HTTPS server
//
// into a single process driven by controller-runtime's Manager. See
// docs/adr/0002-bridge-control-plane-data-plane-split.md for the split
// rationale and docs/PHASES.md for the 9-step wiring sequence this file
// realises (Slice 3D).
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/dataplane"
)

// Environment variables read directly by main.go. controlplane.Config owns
// the BRIDGE_* set in config.go; these are integration-layer knobs that
// don't belong in either package.
const (
	envTLSCertFile      = "BRIDGE_TLS_CERT_FILE"
	envTLSKeyFile       = "BRIDGE_TLS_KEY_FILE"
	envTLSClientCAFile  = "BRIDGE_TLS_CLIENT_CA_FILE"
	envListenAddr       = "BRIDGE_LISTEN_ADDR"
	envHealthAddr       = "BRIDGE_HEALTH_ADDR"
	envMetricsAddr      = "BRIDGE_METRICS_ADDR"
	envEnableLeaderElec = "BRIDGE_ENABLE_LEADER_ELECTION"
	envRateLimit        = "BRIDGE_RATE_LIMIT_PER_SOURCE"
	envRateLimitBurst   = "BRIDGE_RATE_LIMIT_BURST"

	defaultTLSCertFile = "/etc/bridge/tls/tls.crt"
	defaultTLSKeyFile  = "/etc/bridge/tls/tls.key"
	defaultListenAddr  = ":8443"
	defaultHealthAddr  = ":8081"
	defaultMetricsAddr = ":8080"
	defaultRateLimit   = 20
	defaultRateBurst   = 100

	leaderElectionID = "bridge.harbor.aetherize.io"
)

func main() {
	// One-and-only flag: --help. Everything else is BRIDGE_* env vars so
	// the Helm chart can ship a Deployment spec without per-knob args.
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bridge: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Step 1: load BRIDGE_* config and configure logging early so every
	// subsequent component logs through the same sink.
	cfg, err := controlplane.LoadFromEnv()
	if err != nil {
		// Logger isn't up yet; emit to stderr.
		return err
	}
	logger := newLogger(cfg.LogLevel)
	ctrl.SetLogger(logger)
	setupLog := logger.WithName("setup")

	for k, v := range cfg.Sanitized() {
		setupLog.Info("config", "key", k, "value", v)
	}

	// Step 2: build the scheme. clientgo gives us the core resources;
	// harborv1alpha1 is our CRD. Also resolve the rest.Config for the
	// Manager — in-cluster config when running as a Pod, $KUBECONFIG
	// otherwise.
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("get rest config: %w", err)
	}
	// clientgo gives us the core resources; add our CRD so the manager's
	// cached client can decode HarborAccess objects.
	if err := harborv1alpha1.AddToScheme(clientgoscheme.Scheme); err != nil {
		return fmt.Errorf("build scheme: %w", err)
	}

	// Step 3: build the controller-runtime Manager.
	mgrOpts := ctrl.Options{
		Scheme: clientgoscheme.Scheme,
		// /metrics is served by the manager's metrics server on its own
		// port, reachable on the pod network only. It used to share the
		// credential listener, which the NodePort exposes on every node
		// to anything that can reach it. "0" disables it. The data-plane
		// metrics register into the same controller-runtime registry.
		Metrics: metricsserver.Options{BindAddress: envOrDefault(envMetricsAddr, defaultMetricsAddr)},

		LeaderElection:          envBool(envEnableLeaderElec, false),
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: cfg.Namespace,

		HealthProbeBindAddress: envOrDefault(envHealthAddr, defaultHealthAddr),

		// Cache scoping — minimum-privilege RBAC. HarborAccess CRs are
		// cluster-scoped (operators put them in any namespace), so the
		// default cluster-wide watch is correct for those. Secrets, by
		// contrast, are only read from BRIDGE_NAMESPACE (ADR-0011);
		// without this ByObject override the cache would list/watch
		// secrets cluster-wide and require cluster-scoped Secret RBAC.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}: {
					Namespaces: map[string]cache.Config{
						cfg.Namespace: {},
					},
				},
				// ADR-0026: with a selector the reconciler and the data
				// plane only ever see the HarborAccess objects this bridge
				// serves. nil keeps every object.
				&harborv1alpha1.HarborAccess{}: {Label: cfg.HarborAccessSelector},
			},
		},
	}
	mgr, err := ctrl.NewManager(restCfg, mgrOpts)
	if err != nil {
		return fmt.Errorf("build manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz: %w", err)
	}

	// Step 4: load Harbor admin credentials and build the Harbor client.
	adminCreds, err := cfg.LoadAdminCreds()
	if err != nil {
		return fmt.Errorf("load admin creds: %w", err)
	}
	harborTransport, err := harbor.NewTransport(cfg.HarborCAFile)
	if err != nil {
		return fmt.Errorf("build harbor transport: %w", err)
	}
	harborClient, err := harbor.NewClient(cfg.HarborURL, adminCreds.Username, adminCreds.Password, harborTransport,
		harbor.WithRobotPrefix(cfg.HarborRobotPrefix))
	if err != nil {
		return fmt.Errorf("build harbor client: %w", err)
	}

	// Step 5: instantiate Reconciler and register with the manager.
	rec := &controlplane.Reconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Harbor: harborClient,
		Config: cfg,
	}
	if err := rec.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup reconciler: %w", err)
	}

	// Step 6: Janitor as a manager.Runnable.
	if err := mgr.Add(&controlplane.Janitor{
		Client: mgr.GetClient(),
		// Uncached: the janitor must never judge a robot against a spec
		// older than the one the robot was created from.
		Reader: mgr.GetAPIReader(),
		Harbor: harborClient,
		Config: cfg,
	}); err != nil {
		return fmt.Errorf("add janitor: %w", err)
	}

	// Step 7: OIDC Validator. Discovery runs synchronously here so a
	// misconfigured BRIDGE_OIDC_ISSUER fails at startup, not on the
	// first kubelet request. When BRIDGE_OIDC_JWKS_URL is set, discovery
	// is skipped in favour of that URL — see config.go for the local-dev
	// rationale.
	startupCtx := ctrl.SetupSignalHandler()
	validatorCfg := dataplane.Config{
		Issuer: cfg.OIDCIssuer.String(),
	}
	if cfg.OIDCJWKSURL != nil {
		validatorCfg.JWKSURL = cfg.OIDCJWKSURL.String()
	}
	// In-cluster the OIDC issuer is the apiserver: discovery and JWKS
	// fetch need the cluster CA in trust AND an authenticated caller
	// (the apiserver gates /.well-known/openid-configuration behind
	// system:authenticated by default). The client sends the token only
	// to the in-cluster apiserver, re-reads it per request (kubelet
	// rotates it), bounds every fetch and follows no redirects.
	httpClient, err := dataplane.NewOIDCHTTPClient(cfg.OIDCCAFile, cfg.OIDCTokenFile)
	if err != nil {
		return fmt.Errorf("build oidc http client: %w", err)
	}
	validatorCfg.HTTPClient = httpClient
	validator, err := dataplane.NewValidator(startupCtx, validatorCfg)
	if err != nil {
		return fmt.Errorf("build oidc validator: %w", err)
	}

	// Step 8: Handler + HTTPS server.
	metrics := dataplane.NewMetrics(crmetrics.Registry)
	perSource, burst, err := rateLimitFromEnv()
	if err != nil {
		return err
	}
	handler := &dataplane.Handler{
		K8sClient: mgr.GetClient(),
		Validator: validator,
		Config: dataplane.HandlerConfig{
			BridgeNamespace:      cfg.Namespace,
			ForceLocalValidation: cfg.ForceLocalValidation,
			Audience:             cfg.Audience,
		},
		Metrics: metrics,
		Limiter: dataplane.NewSourceLimiter(perSource, burst),
		// Fixed at info: BRIDGE_LOG_LEVEL must not be able to silence the
		// record of who received credentials.
		Audit: newLogger("info").WithName("audit"),
	}

	mux := http.NewServeMux()
	mux.Handle(dataplane.CredentialsPath, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server, err := dataplane.NewServer(dataplane.ServerConfig{
		ListenAddr:   envOrDefault(envListenAddr, defaultListenAddr),
		CertFile:     envOrDefault(envTLSCertFile, defaultTLSCertFile),
		KeyFile:      envOrDefault(envTLSKeyFile, defaultTLSKeyFile),
		ClientCAFile: os.Getenv(envTLSClientCAFile),
		Handler:      mux,
	})
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}
	if err := mgr.Add(server); err != nil {
		return fmt.Errorf("add server: %w", err)
	}
	// Ready means "can serve credentials": the listener is bound (it
	// binds only after the informer caches synced).
	if err := mgr.AddReadyzCheck("dataplane", server.ReadyCheck); err != nil {
		return fmt.Errorf("add readyz: %w", err)
	}

	// Step 9: start the manager. Blocks until SIGTERM/SIGINT.
	setupLog.Info("starting bridge", "leader_election", mgrOpts.LeaderElection)
	if err := mgr.Start(startupCtx); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}
	return nil
}

// rateLimitFromEnv reads the per-source request limit of the credential
// endpoint (requests per second and burst; 0 per second disables it).
func rateLimitFromEnv() (float64, int, error) {
	perSource, burst := float64(defaultRateLimit), defaultRateBurst
	if raw := os.Getenv(envRateLimit); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 {
			return 0, 0, fmt.Errorf("%s %q must be a non-negative number of requests per second", envRateLimit, raw)
		}
		perSource = v
	}
	if raw := os.Getenv(envRateLimitBurst); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 {
			return 0, 0, fmt.Errorf("%s %q must be a positive integer", envRateLimitBurst, raw)
		}
		burst = v
	}
	return perSource, burst, nil
}

// newLogger constructs a zap-backed logr.Logger at the requested level.
// We deliberately bypass zap.UseFlagOptions: BRIDGE_LOG_LEVEL is the
// only knob we expose.
func newLogger(level string) logr.Logger {
	opts := []zap.Opts{zap.UseDevMode(false)}
	switch level {
	case "debug":
		opts = append(opts, zap.Level(zapcore.DebugLevel))
	case "warn":
		opts = append(opts, zap.Level(zapcore.WarnLevel))
	case "error":
		opts = append(opts, zap.Level(zapcore.ErrorLevel))
	}
	return zap.New(opts...)
}

// envOrDefault returns the env var when set non-empty, otherwise def.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBool returns the env var parsed as bool. Falls back to def when
// unset or unparseable; the controlplane config layer fail-fast-validates
// the bools it owns, but the leader-election flag is benign enough that
// "default off" is the right unparseable behaviour.
func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	switch v {
	case "true", "1", "yes":
		return true
	case "false", "0", "no", "":
		return false
	default:
		return def
	}
}
