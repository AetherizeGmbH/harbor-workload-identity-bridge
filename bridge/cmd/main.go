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
	"strings"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
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
	envShutdownDelay    = "BRIDGE_SHUTDOWN_DELAY"

	defaultTLSCertFile = "/etc/bridge/tls/tls.crt"
	defaultTLSKeyFile  = "/etc/bridge/tls/tls.key"
	defaultListenAddr  = ":8443"
	defaultHealthAddr  = ":8081"
	defaultMetricsAddr = ":8080"
	defaultRateLimit   = 20
	defaultRateBurst   = 100

	// defaultShutdownDelay keeps the credential listener serving after
	// SIGTERM while kube-proxy on every node stops routing to the pod.
	defaultShutdownDelay = 5 * time.Second

	// gracefulShutdownTimeout is the manager's budget for stopping every
	// runnable after SIGTERM. It is the pod's default termination grace
	// period, which the chart does not change; kubelet kills the process
	// then anyway.
	gracefulShutdownTimeout = 30 * time.Second

	// serverShutdownTimeout bounds the credential listener's graceful
	// shutdown, which starts after the shutdown delay.
	serverShutdownTimeout = 10 * time.Second

	// leaderStopBudget is the part of gracefulShutdownTimeout left for the
	// reconciler and the janitor to finish in-flight work: the manager
	// stops them only after the credential listener has closed.
	leaderStopBudget = 5 * time.Second

	// maxShutdownDelay is the longest shutdown delay that leaves the
	// listener's shutdown and the leader's runnables their budgets. A
	// longer one would run the manager out of time before the reconciler
	// and the janitor are stopped.
	maxShutdownDelay = gracefulShutdownTimeout - serverShutdownTimeout - leaderStopBudget

	leaderElectionID = "bridge.harbor.aetherize.io"

	// kubeletTokenLifetime is the lifetime of the tokens kubelet requests
	// for the credential provider: the TokenRequest default (ADR-0028).
	kubeletTokenLifetime = time.Hour
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
	// Built here so the startup warnings describe the token policy the
	// validator actually receives in step 7 (ADR-0028).
	validatorCfg := validatorConfig(cfg)
	logWeakTokenValidation(setupLog, validatorCfg)

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
	leaderElection, err := leaderElectionFromEnv()
	if err != nil {
		return err
	}
	shutdownDelay, err := shutdownDelayFromEnv()
	if err != nil {
		return err
	}
	mgrOpts := managerOptions(cfg, leaderElection)
	mgr, err := ctrl.NewManager(restCfg, mgrOpts)
	if err != nil {
		return fmt.Errorf("build manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz: %w", err)
	}

	// Step 4: build the Harbor client.
	harborClient, err := newHarborClient(cfg, logger.WithName("harbor-credentials"))
	if err != nil {
		return err
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
	// rationale. validatorCfg (step 1) already holds the issuer, the JWKS
	// URL and the token policy; only the HTTP client is added here.
	startupCtx := ctrl.SetupSignalHandler()
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
			RobotUsername:        robotUsername(cfg),
		},
		Metrics: metrics,
		Limiter: dataplane.NewSourceLimiter(perSource, burst),
		// Fixed at info: BRIDGE_LOG_LEVEL must not be able to silence the
		// record of who received credentials.
		Audit: newLogger("info").WithName("audit"),
	}

	if err := dataplane.IndexHarborAccessBySubject(startupCtx, mgr.GetFieldIndexer()); err != nil {
		return fmt.Errorf("index HarborAccess by subject: %w", err)
	}

	server, err := dataplane.NewServer(serverConfig(credentialMux(handler), shutdownDelay))
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}
	// Ready means "can serve credentials": the listener is bound, and it
	// binds only after the HarborAccess and Secret caches the handler
	// reads have synced. A replica whose caches do not sync within
	// CacheSyncTimeout exits with an error.
	if err := controlplane.AddAfterCacheSync(mgr, "dataplane", server, controlplane.CacheSyncTimeout); err != nil {
		return err
	}

	// Step 9: start the manager. Blocks until SIGTERM/SIGINT. Return
	// right after it: LeaderElectionReleaseOnCancel relies on the process
	// exiting once the manager stops.
	setupLog.Info("starting bridge", "leader_election", mgrOpts.LeaderElection, "shutdown_delay", shutdownDelay.String())
	if err := mgr.Start(startupCtx); err != nil {
		return fmt.Errorf("manager exited with error: %w", err)
	}
	return nil
}

// serverConfig builds the credential listener's config.
func serverConfig(handler http.Handler, shutdownDelay time.Duration) dataplane.ServerConfig {
	return dataplane.ServerConfig{
		ListenAddr:      envOrDefault(envListenAddr, defaultListenAddr),
		CertFile:        envOrDefault(envTLSCertFile, defaultTLSCertFile),
		KeyFile:         envOrDefault(envTLSKeyFile, defaultTLSKeyFile),
		ClientCAFile:    os.Getenv(envTLSClientCAFile),
		Handler:         handler,
		ShutdownDelay:   shutdownDelay,
		ShutdownTimeout: serverShutdownTimeout,
	}
}

// newHarborClient builds the Harbor client. It reads the admin credentials
// from BRIDGE_HARBOR_ADMIN_DIR on every Harbor call, so a rotated Secret
// takes effect without a restart; reading them once here fails startup on
// a missing or empty file.
func newHarborClient(cfg *controlplane.Config, log logr.Logger) (harbor.Client, error) {
	adminCreds := controlplane.NewAdminCredsReader(cfg, log)
	username, password, err := adminCreds.Read()
	if err != nil {
		return nil, fmt.Errorf("load admin creds: %w", err)
	}
	transport, err := harbor.NewTransport(cfg.HarborCAFile)
	if err != nil {
		return nil, fmt.Errorf("build harbor transport: %w", err)
	}
	c, err := harbor.NewClient(cfg.HarborURL, username, password, transport,
		harbor.WithRobotPrefix(cfg.HarborRobotPrefix),
		harbor.WithCredentialSource(adminCreds.Read))
	if err != nil {
		return nil, fmt.Errorf("build harbor client: %w", err)
	}
	return c, nil
}

// managerOptions builds the controller-runtime Manager's options.
func managerOptions(cfg *controlplane.Config, leaderElection bool) ctrl.Options {
	return ctrl.Options{
		Scheme: clientgoscheme.Scheme,
		// Stated rather than left to the default, because the shutdown
		// delay is bounded by it (maxShutdownDelay).
		GracefulShutdownTimeout: ptr.To(gracefulShutdownTimeout),
		// /metrics is served by the manager's metrics server on its own
		// port, reachable on the pod network only. It used to share the
		// credential listener, which the NodePort exposes on every node
		// to anything that can reach it. "0" disables it. The data-plane
		// metrics register into the same controller-runtime registry.
		Metrics: metricsserver.Options{BindAddress: envOrDefault(envMetricsAddr, defaultMetricsAddr)},

		LeaderElection:          leaderElection,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: cfg.Namespace,
		// A stopping leader hands the Lease back, so another replica
		// resumes reconciling and sweeping at once instead of after the
		// lease duration (15s). Safe only because the process exits as
		// soon as the manager stops: run returns right after mgr.Start,
		// and nothing may be added after it.
		LeaderElectionReleaseOnCancel: true,

		HealthProbeBindAddress: envOrDefault(envHealthAddr, defaultHealthAddr),

		// Minimum-privilege RBAC: Secrets from BRIDGE_NAMESPACE only
		// (ADR-0011), HarborAccess cluster-wide, limited by the selector
		// (ADR-0026).
		Cache: cfg.CacheOptions(),
	}
}

// credentialMux routes the credential listener, which the NodePort exposes
// on every node: it serves the credential endpoint and nothing else
// (ADR-0025). Health, readiness and /metrics have their own ports.
func credentialMux(credentials http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(dataplane.CredentialsPath, credentials)
	return mux
}

// validatorConfig maps the bridge config onto the validator's, all but the
// HTTP client. BRIDGE_REQUIRE_POD_BOUND_TOKEN and AllowNonPodBoundTokens
// mean opposite things (the validator's zero value is the strict one), so
// the mapping lives here, under test (ADR-0028).
func validatorConfig(cfg *controlplane.Config) dataplane.Config {
	vc := dataplane.Config{
		Issuer:                 cfg.OIDCIssuer.String(),
		MaxTokenLifetime:       cfg.TokenMaxLifetime,
		AllowNonPodBoundTokens: !cfg.RequirePodBoundToken,
	}
	if cfg.OIDCJWKSURL != nil {
		vc.JWKSURL = cfg.OIDCJWKSURL.String()
	}
	return vc
}

// robotUsername maps a ServiceAccount to the username the control plane
// stores in its robot Secret: the Harbor robot prefix plus the robot name
// (ADR-0018), as Harbor reports it. The data plane serves a Secret only to
// the identity it was minted for.
func robotUsername(cfg *controlplane.Config) func(saNamespace, saName string) (string, error) {
	return func(saNamespace, saName string) (string, error) {
		name, err := harbor.RobotName(cfg.ClusterName, saNamespace, saName)
		if err != nil {
			return "", err
		}
		return cfg.HarborRobotPrefix + name, nil
	}
}

// logWeakTokenValidation warns about token-validation settings that either
// weaken the bridge or refuse kubelet's own tokens (ADR-0028). It reads the
// validator's config, not the bridge's, so it warns about what is enforced.
func logWeakTokenValidation(log logr.Logger, vc dataplane.Config) {
	if vc.AllowNonPodBoundTokens {
		log.Info("tokens not bound to a pod are accepted; this weakens the bridge and is meant for local development only",
			"env", controlplane.EnvRequirePodBoundToken)
	}
	if vc.MaxTokenLifetime < kubeletTokenLifetime {
		log.Info("the maximum token lifetime is below the lifetime of kubelet's tokens; every kubelet request will be refused unless the token issuer caps lifetimes lower",
			"env", controlplane.EnvTokenMaxLifetime, "max", vc.MaxTokenLifetime.String(), "kubelet", kubeletTokenLifetime.String())
	}
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

// shutdownDelayFromEnv reads how long the credential listener keeps
// serving after SIGTERM (a Go duration from 0, which closes it at once, to
// maxShutdownDelay).
func shutdownDelayFromEnv() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(envShutdownDelay))
	if raw == "" {
		return defaultShutdownDelay, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 || d > maxShutdownDelay {
		return 0, fmt.Errorf("%s %q must be a duration from 0s to %s: the listener's shutdown and the reconciler's must still fit into the %s the process has after SIGTERM",
			envShutdownDelay, raw, maxShutdownDelay, gracefulShutdownTimeout)
	}
	return d, nil
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

// leaderElectionFromEnv reads BRIDGE_ENABLE_LEADER_ELECTION. Unset or
// empty means off (one replica). A value that is not a boolean fails
// startup instead of meaning "off": with several replicas, election
// silently off runs the reconciler and the janitor on every replica, which
// ADR-0025 keeps leader-only; two reconcilers race on robot creation and
// password rotation and can leave a Secret holding a password Harbor
// already replaced. "yes" and "no" stay accepted, as before.
func leaderElectionFromEnv() (bool, error) {
	raw := strings.TrimSpace(os.Getenv(envEnableLeaderElec))
	switch raw {
	case "":
		return false, nil
	case "yes":
		return true, nil
	case "no":
		return false, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s %q must be true or false", envEnableLeaderElec, raw)
	}
	return v, nil
}
