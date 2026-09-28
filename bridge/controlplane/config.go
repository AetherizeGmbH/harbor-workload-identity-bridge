// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Package controlplane contains the bridge's control-plane components:
// runtime configuration, the HarborAccess reconciler, and the orphan-robot
// janitor. See docs/adr/0002-bridge-control-plane-data-plane-split.md.
package controlplane

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/harbor"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/internal/registryhost"
)

// Environment variable names. Constants so wiring (Helm chart, Deployment
// template, tests) has a single source of truth.
const (
	EnvClusterName          = "BRIDGE_CLUSTER_NAME"
	EnvNamespace            = "BRIDGE_NAMESPACE"
	EnvOIDCIssuer           = "BRIDGE_OIDC_ISSUER"
	EnvOIDCJWKSURL          = "BRIDGE_OIDC_JWKS_URL"
	EnvOIDCCAFile           = "BRIDGE_OIDC_CA_FILE"
	EnvOIDCTokenFile        = "BRIDGE_OIDC_TOKEN_FILE"
	EnvHarborURL            = "BRIDGE_HARBOR_URL"
	EnvHarborAdminDir       = "BRIDGE_HARBOR_ADMIN_DIR"
	EnvHarborRobotPrefix    = "BRIDGE_HARBOR_ROBOT_PREFIX"
	EnvHarborCAFile         = "BRIDGE_HARBOR_CA_FILE"
	EnvHarborAllowHTTP      = "BRIDGE_HARBOR_ALLOW_INSECURE_HTTP"
	EnvForceLocalValidation = "BRIDGE_FORCE_LOCAL_VALIDATION"
	EnvLogLevel             = "BRIDGE_LOG_LEVEL"
	EnvAudience             = "BRIDGE_AUDIENCE"
	EnvHarborAccessSelector = "BRIDGE_HARBORACCESS_SELECTOR"
	EnvInstance             = "BRIDGE_INSTANCE"
	EnvTokenMaxLifetime     = "BRIDGE_TOKEN_MAX_LIFETIME"
	EnvRequirePodBoundToken = "BRIDGE_REQUIRE_POD_BOUND_TOKEN"

	// EnvHarborRegistryHosts lists the registry hosts
	// (host[:port][/path-prefix], comma-separated) whose images the data
	// plane serves from HarborAccess objects once a second backend is
	// configured (ADR-0033 decision f). Optional; defaults to the
	// host[:port] of BRIDGE_HARBOR_URL.
	EnvHarborRegistryHosts = "BRIDGE_HARBOR_REGISTRY_HOSTS"

	// Nexus backend (ADR-0033). The backend is enabled exactly when
	// EnvNexusURL is set; the other BRIDGE_NEXUS_* variables are refused
	// without it.
	EnvNexusURL              = "BRIDGE_NEXUS_URL"
	EnvNexusAdminDir         = "BRIDGE_NEXUS_ADMIN_DIR"
	EnvNexusCAFile           = "BRIDGE_NEXUS_CA_FILE"
	EnvNexusAllowHTTP        = "BRIDGE_NEXUS_ALLOW_INSECURE_HTTP"
	EnvNexusRegistryHosts    = "BRIDGE_NEXUS_REGISTRY_HOSTS"
	EnvNexusRateLimitBackoff = "BRIDGE_NEXUS_RATE_LIMIT_BACKOFF"

	// instanceMaxLen keeps the per-instance finalizer's name part
	// ("robot-<instance>") within the 63-character limit.
	instanceMaxLen = 50
	audienceMaxLen = 253

	clusterNamePattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	clusterNameMaxLen  = 63
	defaultLogLevel    = "info"

	// defaultTokenMaxLifetime admits kubelet's credential-provider tokens,
	// which carry the TokenRequest default lifetime of one hour (ADR-0028).
	defaultTokenMaxLifetime = time.Hour

	adminUsernameKey = "username"
	adminPasswordKey = "password"

	// DefaultNexusRateLimitBackoff is how long the Nexus controller stops
	// every call made with the admin credential after Nexus answered 429:
	// Nexus's default nexus.auth.ratelimit.max-delay-seconds (ADR-0033
	// decision j).
	DefaultNexusRateLimitBackoff = 15 * time.Minute
)

var (
	clusterNameRegex = regexp.MustCompile(clusterNamePattern)
	validLogLevels   = map[string]struct{}{
		"debug": {}, "info": {}, "warn": {}, "error": {},
	}
)

// Config is the bridge's runtime configuration. Loaded once at startup; no
// hot-reload. See docs/adr/0009-multi-cluster-topology.md for the cluster
// identity model and the rationale for fail-fast loading.
type Config struct {
	// ClusterName is this bridge instance's identity. Required, DNS-label
	// validated. Used as the prefix for every Harbor robot this bridge owns
	// (bridge-<ClusterName>.<saNs>.<saName>, ADR-0018) and as the basis of
	// the ownership-prefix safety invariant.
	ClusterName string

	// Namespace is the namespace this bridge runs in. Required, DNS-label
	// validated. Robot-password Secrets live here, not in the HarborAccess
	// CR's namespace, so workload SAs cannot read them.
	Namespace string

	// OIDCIssuer is the cluster's service-account token issuer. The data
	// plane validates inbound SA tokens against this issuer; the control
	// plane uses it to detect HarborAccess CRs whose trustPolicy.issuer
	// disagrees with the cluster the bridge is running in.
	OIDCIssuer *url.URL

	// OIDCJWKSURL, when non-nil, overrides where the data plane fetches
	// the JSON Web Key Set used to verify SA token signatures. The
	// expected iss claim of incoming tokens remains OIDCIssuer; only
	// the fetch URL changes. Use when the bridge runs outside the
	// cluster (local dev via `kubectl proxy`) or behind a network
	// topology where the cluster-internal URLs do not resolve. Leave
	// unset for the standard in-cluster deployment.
	OIDCJWKSURL *url.URL

	// OIDCCAFile, when non-empty, is the path to a PEM-encoded CA bundle
	// the OIDC validator's HTTP client uses to verify the OIDC issuer's
	// TLS certificate. For an in-cluster bridge the issuer is the
	// apiserver itself, whose cert is signed by the cluster CA — the
	// chart points this at the projected SA volume's ca.crt
	// (/var/run/secrets/kubernetes.io/serviceaccount/ca.crt) by default
	// so OIDC discovery succeeds without trusting the cluster CA at the
	// OS level. Empty means use the OS root store (the laptop / public-
	// issuer case).
	OIDCCAFile string

	// OIDCTokenFile, when non-empty, is the path to a Bearer token the
	// OIDC validator's HTTP client sends on every request to the issuer.
	// Required when the issuer is the in-cluster apiserver, whose
	// /.well-known/openid-configuration endpoint refuses anonymous
	// access by default. The chart points this at the projected SA
	// volume (/var/run/secrets/kubernetes.io/serviceaccount/token); the
	// transport re-reads the file on each request so token rotation
	// (kubelet refreshes the projected token every ~hour) is automatic.
	OIDCTokenFile string

	// HarborURL is the base URL of the Harbor instance this bridge manages
	// robots in.
	HarborURL *url.URL

	// HarborAdminDir is the path to a Kubernetes Secret mounted as a volume,
	// containing files named "username" and "password" with the Harbor admin
	// (or per-cluster system robot) credentials. See ADR-0009 for the
	// per-cluster-system-robot recommendation. The Harbor client reads them
	// on every call (AdminCredsReader), so a rotated Secret needs no restart.
	HarborAdminDir string

	// HarborRobotPrefix is the robot name prefix the Harbor instance is
	// configured with (Harbor's robot_name_prefix, default "robot$").
	// Harbor stores robot names without it and reports them with it; the
	// Harbor client strips it on read paths (ADR-0023). Set it when the
	// Harbor administrator changed the prefix — otherwise the Harbor client
	// fails every read with ErrRobotPrefixMismatch.
	HarborRobotPrefix string

	// ForceLocalValidation gates whether the data plane performs full local
	// OIDC validation. Defaults to true. The "false" path is plumbed for
	// the post-upstream-migration scenario described in ADR-0002 and
	// docs/MIGRATION.md but is not implemented yet.
	ForceLocalValidation bool

	// LogLevel is one of debug, info, warn, error.
	LogLevel string

	// HarborCAFile, when set, is the only trust root for Harbor's TLS
	// certificate (a private CA).
	HarborCAFile string

	// HarborAllowHTTP permits an http:// HarborURL. Over plain HTTP the
	// admin credentials travel on every call and robot passwords come
	// back in responses, so it is an explicit opt-in (e.g. an in-cluster
	// Harbor reached over the pod network in a test cluster).
	HarborAllowHTTP bool

	// Audience is the only token audience this bridge serves (ADR-0026):
	// the audience kubelet requests for the plugin (chart:
	// plugin.audience). A HarborAccess naming another audience is not
	// served.
	Audience string

	// HarborAccessSelector limits the bridge to matching HarborAccess
	// objects (ADR-0026). nil selects every HarborAccess.
	HarborAccessSelector labels.Selector

	// Instance names this bridge among several on a cluster. It is
	// required with a selector and forms the per-instance finalizer.
	Instance string

	// TokenMaxLifetime is the longest lifetime (exp - iat) of a
	// ServiceAccount token the data plane accepts (ADR-0028). Defaults to
	// defaultTokenMaxLifetime; must be positive.
	TokenMaxLifetime time.Duration

	// RequirePodBoundToken makes the data plane refuse tokens without the
	// kubernetes.io pod claim (ADR-0028). Defaults to true. false weakens
	// the bridge and exists for hand-minted tokens in local development.
	RequirePodBoundToken bool

	// HarborRegistryHosts are the registry hosts whose images belong to
	// Harbor (BRIDGE_HARBOR_REGISTRY_HOSTS; by default the host[:port] of
	// HarborURL). The data plane routes by them only when Nexus is
	// configured as well; with Harbor alone the image is audit-only.
	HarborRegistryHosts []registryhost.Host

	// Nexus is the Nexus backend's configuration (ADR-0033); nil when
	// BRIDGE_NEXUS_URL is unset, which leaves the bridge as it was before
	// the backend existed.
	Nexus *NexusConfig
}

// NexusConfig configures the Nexus backend (ADR-0033). Harbor stays
// required alongside it (ADR-0033 question 7).
type NexusConfig struct {
	// URL is Nexus's base URL (scheme, host and any context path); the
	// client appends /service/rest.
	URL *url.URL

	// AdminDir is the path of the mounted Secret with the files
	// "username" and "password" of the Nexus credential the bridge
	// manages users and roles with. They are read on every Nexus call
	// (NewNexusAdminCredsReader), so a rotated Secret needs no restart.
	AdminDir string

	// CAFile, when set, is the only trust root for Nexus's TLS
	// certificate (a private CA).
	CAFile string

	// AllowHTTP permits an http:// URL. The admin credential and every new
	// user's password would then travel unencrypted.
	AllowHTTP bool

	// RegistryHosts are the registry hosts (host[:port][/path-prefix])
	// whose images the data plane serves from NexusAccess objects. None
	// shares a host[:port] with Config.HarborRegistryHosts.
	RegistryHosts []registryhost.Host

	// RateLimitBackoff is how long every Nexus call with the admin
	// credential stops after Nexus answered 429, counted from the last
	// 429 (ADR-0033 decision j). Positive.
	RateLimitBackoff time.Duration
}

// LoadAdminCreds reads the Nexus admin credentials from AdminDir, both
// from the same version of the volume (readAdminCredsDir).
func (n *NexusConfig) LoadAdminCreds() (*AdminCreds, error) {
	return readAdminCredsDir(n.AdminDir)
}

// LoadCA reads the PEM bundle at CAFile; nil when CAFile is unset.
func (n *NexusConfig) LoadCA() ([]byte, error) {
	if n.CAFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(n.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", EnvNexusCAFile, err)
	}
	return pem, nil
}

// Finalizer returns the finalizer this bridge sets on the HarborAccess
// objects it manages: the shared FinalizerName without a selector, a
// per-instance one with a selector, so that a bridge can release a CR that
// moved to another bridge without touching that bridge's finalizer.
func (c *Config) Finalizer() string {
	if !c.selective() {
		return FinalizerName
	}
	return FinalizerName + "-" + c.Instance
}

// ReleasedFinalizers returns the finalizers this bridge removes once it
// has revoked a HarborAccess's robots: its own (Finalizer), and the one
// the same installation set in its other mode, so that adding or removing
// the selector does not leave a finalizer nobody removes. With a selector
// that is the shared finalizer, which only this bridge's pre-selector
// installation can have set (ADR-0026 point 3). Without one it is the
// per-instance finalizer of a selector since removed, which the bridge
// can name only while BRIDGE_INSTANCE still names the instance.
func (c *Config) ReleasedFinalizers() []string {
	switch {
	case c.selective():
		return []string{c.Finalizer(), FinalizerName}
	case c.Instance != "":
		return []string{FinalizerName, FinalizerName + "-" + c.Instance}
	default:
		return []string{FinalizerName}
	}
}

// statusRobotIsOurs reports whether the robot ha's status records, if any,
// has this bridge's clusterName prefix. Every bridge that serves ha
// records its robot there (markReady), so a robot of another clusterName
// means another bridge served ha since.
func (c *Config) statusRobotIsOurs(ha *harborv1alpha1.HarborAccess) bool {
	return ha.Status.Robot == nil ||
		harbor.OwnsRobot(c.ClusterName, strings.TrimPrefix(ha.Status.Robot.Name, c.HarborRobotPrefix))
}

// Selects reports whether this bridge manages obj, a HarborAccess or a
// NexusAccess: BRIDGE_HARBORACCESS_SELECTOR selects both kinds (ADR-0026,
// ADR-0033).
func (c *Config) Selects(obj metav1.Object) bool {
	return !c.selective() || c.HarborAccessSelector.Matches(labels.Set(obj.GetLabels()))
}

// NexusFinalizer is Finalizer for NexusAccess objects: NexusFinalizerName
// without a selector, a per-instance one with a selector.
func (c *Config) NexusFinalizer() string {
	if !c.selective() {
		return NexusFinalizerName
	}
	return NexusFinalizerName + "-" + c.Instance
}

// NexusReleasedFinalizers is ReleasedFinalizers for NexusAccess objects.
func (c *Config) NexusReleasedFinalizers() []string {
	switch {
	case c.selective():
		return []string{c.NexusFinalizer(), NexusFinalizerName}
	case c.Instance != "":
		return []string{NexusFinalizerName, NexusFinalizerName + "-" + c.Instance}
	default:
		return []string{NexusFinalizerName}
	}
}

func (c *Config) selective() bool {
	return c.HarborAccessSelector != nil && !c.HarborAccessSelector.Empty()
}

// LoadFromEnv reads bridge configuration from BRIDGE_* environment variables
// and validates the result. All validation failures are joined and returned
// at once so operators do not have to fix-restart-fix in a loop.
func LoadFromEnv() (*Config, error) {
	cfg := &Config{
		LogLevel:             defaultLogLevel,
		ForceLocalValidation: true,
		HarborRobotPrefix:    harbor.DefaultRobotPrefix,
		TokenMaxLifetime:     defaultTokenMaxLifetime,
		RequirePodBoundToken: true,
	}
	var errs []error

	cfg.ClusterName = strings.TrimSpace(os.Getenv(EnvClusterName))
	switch {
	case cfg.ClusterName == "":
		errs = append(errs, fmt.Errorf("%s is required", EnvClusterName))
	case len(cfg.ClusterName) > clusterNameMaxLen:
		errs = append(errs, fmt.Errorf("%s %q exceeds %d-char DNS-label limit", EnvClusterName, cfg.ClusterName, clusterNameMaxLen))
	case !clusterNameRegex.MatchString(cfg.ClusterName):
		errs = append(errs, fmt.Errorf("%s %q must match %s", EnvClusterName, cfg.ClusterName, clusterNamePattern))
	case strings.Contains(cfg.ClusterName, "--"):
		// The cluster name is part of every robot name, and Harbor refuses
		// robot names with doubled separators: no robot could be created.
		errs = append(errs, fmt.Errorf("%s %q must not contain consecutive hyphens: Harbor refuses robot names with them, so the bridge could not create any robot", EnvClusterName, cfg.ClusterName))
	}

	cfg.Namespace = strings.TrimSpace(os.Getenv(EnvNamespace))
	switch {
	case cfg.Namespace == "":
		errs = append(errs, fmt.Errorf("%s is required", EnvNamespace))
	case len(cfg.Namespace) > clusterNameMaxLen:
		errs = append(errs, fmt.Errorf("%s %q exceeds %d-char DNS-label limit", EnvNamespace, cfg.Namespace, clusterNameMaxLen))
	case !clusterNameRegex.MatchString(cfg.Namespace):
		errs = append(errs, fmt.Errorf("%s %q must match %s", EnvNamespace, cfg.Namespace, clusterNamePattern))
	}

	if v, err := requireURL(os.Getenv(EnvOIDCIssuer), EnvOIDCIssuer); err != nil {
		errs = append(errs, err)
	} else if v.User != nil {
		// The issuer is compared with each token's iss claim and each
		// HarborAccess's trustPolicy.issuer, neither of which carries
		// credentials: no token could ever match.
		errs = append(errs, fmt.Errorf("%s must not contain credentials (user:password@): a token's iss claim never carries them, so no token would match", EnvOIDCIssuer))
	} else {
		cfg.OIDCIssuer = v
	}

	if raw := strings.TrimSpace(os.Getenv(EnvOIDCJWKSURL)); raw != "" {
		// Optional — when set, must still parse as a URL with a scheme
		// and host. Same shape as the other URL knobs. Unlike them it may
		// carry user:password@: net/http sends that as Basic auth to the
		// JWKS endpoint. Sanitized() hides the whole part (redactURL).
		if v, err := requireURL(raw, EnvOIDCJWKSURL); err != nil {
			errs = append(errs, err)
		} else {
			cfg.OIDCJWKSURL = v
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvOIDCCAFile)); raw != "" {
		// Optional. We don't read the file here — main.go does, and
		// surfaces the read error there so a missing/unreadable bundle
		// fails at startup with a clear message.
		cfg.OIDCCAFile = raw
	}

	if raw := strings.TrimSpace(os.Getenv(EnvOIDCTokenFile)); raw != "" {
		cfg.OIDCTokenFile = raw
	}

	cfg.HarborAllowHTTP = envBoolOr(EnvHarborAllowHTTP, false)
	if v, err := requireURL(os.Getenv(EnvHarborURL), EnvHarborURL); err != nil {
		errs = append(errs, err)
	} else if v.User != nil {
		// The Harbor client never used URL credentials (the SDK takes only
		// scheme, host and path); they would only end up in logs.
		errs = append(errs, fmt.Errorf("%s must not contain credentials (user:password@): the bridge ignores them and authenticates to Harbor with the credentials in %s (chart harbor.adminCredsSecret)", EnvHarborURL, EnvHarborAdminDir))
	} else if v.Scheme == "http" && !cfg.HarborAllowHTTP {
		errs = append(errs, fmt.Errorf("%s %q uses plain http: the Harbor admin credentials and robot passwords would travel unencrypted. Use https (with %s for a private CA), or set %s=true", EnvHarborURL, redactURL(v), EnvHarborCAFile, EnvHarborAllowHTTP))
	} else {
		cfg.HarborURL = v
	}
	cfg.HarborCAFile = strings.TrimSpace(os.Getenv(EnvHarborCAFile))

	cfg.HarborAdminDir = strings.TrimSpace(os.Getenv(EnvHarborAdminDir))
	if cfg.HarborAdminDir == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvHarborAdminDir))
	}

	if raw := strings.TrimSpace(os.Getenv(EnvHarborRobotPrefix)); raw != "" {
		cfg.HarborRobotPrefix = raw
	}

	if raw := os.Getenv(EnvForceLocalValidation); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q must be a boolean", EnvForceLocalValidation, raw))
		} else {
			cfg.ForceLocalValidation = v
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvTokenMaxLifetime)); raw != "" {
		v, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s %q must be a duration such as 1h", EnvTokenMaxLifetime, raw))
		case v <= 0:
			errs = append(errs, fmt.Errorf("%s %q must be positive", EnvTokenMaxLifetime, raw))
		default:
			cfg.TokenMaxLifetime = v
		}
	}

	if raw := os.Getenv(EnvRequirePodBoundToken); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q must be a boolean", EnvRequirePodBoundToken, raw))
		} else {
			cfg.RequirePodBoundToken = v
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvLogLevel)); raw != "" {
		if _, ok := validLogLevels[raw]; !ok {
			errs = append(errs, fmt.Errorf("%s %q must be one of debug, info, warn, error", EnvLogLevel, raw))
		} else {
			cfg.LogLevel = raw
		}
	}

	cfg.Audience = strings.TrimSpace(os.Getenv(EnvAudience))
	switch {
	case cfg.Audience == "":
		errs = append(errs, fmt.Errorf("%s is required (the token audience kubelet requests for the plugin)", EnvAudience))
	case len(cfg.Audience) > audienceMaxLen:
		errs = append(errs, fmt.Errorf("%s exceeds %d characters", EnvAudience, audienceMaxLen))
	}

	if raw := strings.TrimSpace(os.Getenv(EnvHarborAccessSelector)); raw != "" {
		sel, err := labels.Parse(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q: %w", EnvHarborAccessSelector, raw, err))
		} else {
			cfg.HarborAccessSelector = sel
		}
	}
	cfg.Instance = strings.TrimSpace(os.Getenv(EnvInstance))
	switch {
	case cfg.selective() && cfg.Instance == "":
		errs = append(errs, fmt.Errorf("%s is required when %s is set", EnvInstance, EnvHarborAccessSelector))
	case cfg.Instance != "" && (len(cfg.Instance) > instanceMaxLen || !clusterNameRegex.MatchString(cfg.Instance)):
		errs = append(errs, fmt.Errorf("%s %q must be a DNS label of at most %d characters", EnvInstance, cfg.Instance, instanceMaxLen))
	}

	errs = append(errs, cfg.loadRegistryHostsFromEnv()...)

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid bridge configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// loadRegistryHostsFromEnv reads BRIDGE_HARBOR_REGISTRY_HOSTS and the Nexus
// backend (BRIDGE_NEXUS_*, ADR-0033). It runs after HarborURL is parsed,
// whose host is the default Harbor registry host.
func (c *Config) loadRegistryHostsFromEnv() []error {
	var errs []error
	rawURL := strings.TrimSpace(os.Getenv(EnvNexusURL))
	if raw := strings.TrimSpace(os.Getenv(EnvHarborRegistryHosts)); raw != "" {
		hosts, err := registryhost.ParseList(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", EnvHarborRegistryHosts, err))
		}
		c.HarborRegistryHosts = hosts
	} else if c.HarborURL != nil {
		h, err := registryhost.Parse(c.HarborURL.Host)
		switch {
		case err == nil:
			c.HarborRegistryHosts = []registryhost.Host{h}
		case rawURL != "":
			// Only routing needs it; with Harbor alone the image is
			// audit-only and an unusual Harbor host must keep working.
			errs = append(errs, fmt.Errorf("the host of %s is no registry host; set %s: %w", EnvHarborURL, EnvHarborRegistryHosts, err))
		}
	}

	if rawURL == "" {
		var stray []string
		for _, k := range []string{EnvNexusAdminDir, EnvNexusCAFile, EnvNexusAllowHTTP, EnvNexusRegistryHosts, EnvNexusRateLimitBackoff} {
			if strings.TrimSpace(os.Getenv(k)) != "" {
				stray = append(stray, k)
			}
		}
		if len(stray) > 0 {
			// Half a Nexus configuration: the operator meant to enable the
			// backend, and the bridge would silently serve Harbor only.
			errs = append(errs, fmt.Errorf("%s set without %s, which enables the Nexus backend", strings.Join(stray, ", "), EnvNexusURL))
		}
		return errs
	}

	n := &NexusConfig{RateLimitBackoff: DefaultNexusRateLimitBackoff}
	if raw := strings.TrimSpace(os.Getenv(EnvNexusAllowHTTP)); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s %q must be a boolean", EnvNexusAllowHTTP, raw))
		}
		n.AllowHTTP = v
	}
	if v, err := requireURL(rawURL, EnvNexusURL); err != nil {
		errs = append(errs, err)
	} else {
		switch {
		case v.User != nil:
			errs = append(errs, fmt.Errorf("%s must not contain credentials (user:password@): the bridge authenticates to Nexus with the credentials in %s (chart nexus.adminCredsSecret)", EnvNexusURL, EnvNexusAdminDir))
		case v.RawQuery != "" || v.ForceQuery || v.Fragment != "":
			errs = append(errs, fmt.Errorf("%s must not carry a query or fragment: it is Nexus's base URL", EnvNexusURL))
		case v.Scheme == "http" && !n.AllowHTTP:
			errs = append(errs, fmt.Errorf("%s %q uses plain http: the Nexus admin credentials and every new user's password would travel unencrypted. Use https (with %s for a private CA), or set %s=true", EnvNexusURL, redactURL(v), EnvNexusCAFile, EnvNexusAllowHTTP))
		default:
			n.URL = v
		}
	}
	n.AdminDir = strings.TrimSpace(os.Getenv(EnvNexusAdminDir))
	if n.AdminDir == "" {
		errs = append(errs, fmt.Errorf("%s is required when %s is set", EnvNexusAdminDir, EnvNexusURL))
	}
	n.CAFile = strings.TrimSpace(os.Getenv(EnvNexusCAFile))

	if raw := strings.TrimSpace(os.Getenv(EnvNexusRegistryHosts)); raw == "" {
		errs = append(errs, fmt.Errorf("%s is required when %s is set: the data plane routes a request to Nexus by the image's registry host", EnvNexusRegistryHosts, EnvNexusURL))
	} else if hosts, err := registryhost.ParseList(raw); err != nil {
		errs = append(errs, fmt.Errorf("%s: %w", EnvNexusRegistryHosts, err))
	} else {
		n.RegistryHosts = hosts
		if hp, shared := registryhost.SharedHostPort(c.HarborRegistryHosts, hosts); shared {
			errs = append(errs, fmt.Errorf("registry host %s is both Harbor's (%s) and Nexus's (%s): kubelet caches credentials per registry host, so it would hand one backend's credentials to the other's images; give each backend its own host[:port]",
				hp, EnvHarborRegistryHosts, EnvNexusRegistryHosts))
		}
	}

	if raw := strings.TrimSpace(os.Getenv(EnvNexusRateLimitBackoff)); raw != "" {
		v, err := time.ParseDuration(raw)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s %q must be a duration such as 15m", EnvNexusRateLimitBackoff, raw))
		case v <= 0:
			errs = append(errs, fmt.Errorf("%s %q must be positive", EnvNexusRateLimitBackoff, raw))
		default:
			n.RateLimitBackoff = v
		}
	}
	c.Nexus = n
	return errs
}

// requireURL parses an http(s) URL setting. Its errors never repeat the
// value or any part of it that could hold a credential: the value may
// carry user:password@, and a malformed one leaks it in ways
// url.URL.Redacted does not hide:
//
//   - without a scheme ("user:password@host") the user name parses as the
//     scheme and the password lands in the opaque part;
//   - a '/', '?' or '#' inside the password ends the host part early, so
//     the parser quotes the password's start as an invalid port
//     ("https://admin:s3cr/et@host": invalid port ":s3cr"), or, when that
//     start is all digits, accepts host "admin:1234" and keeps the rest of
//     the password in the path.
func requireURL(raw, name string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	hasAt := strings.Contains(raw, "@")
	u, err := url.Parse(raw)
	if err != nil {
		if hasAt {
			return nil, fmt.Errorf("%s is not a valid URL (the parser's reason is left out because the value contains '@' "+
				"and the reason could quote a credential); percent-encode any '/', '?', '#' or '@' inside a user:password@ part", name)
		}
		// *url.Error repeats the whole input; keep only the cause.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("%s is not a valid URL: %w", name, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		if hasAt {
			return nil, fmt.Errorf("%s must use http or https scheme", name)
		}
		return nil, fmt.Errorf("%s must use http or https scheme (got scheme %q)", name, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%s must include a host", name)
	}
	// Only a literal '@': one written as %40 cannot have come from a
	// user:password@ part.
	if strings.Contains(u.EscapedPath()+u.RawQuery+u.EscapedFragment(), "@") {
		return nil, fmt.Errorf("%s has an '@' after its host part: a '/', '?' or '#' inside a user:password@ part ends the host early, "+
			"so percent-encode them, and write an '@' that belongs to the path, query or fragment as %%40 "+
			"(the value is left out because it could hold a credential)", name)
	}
	return u, nil
}

// redactURL renders u for logs and error messages with its whole
// user:password@ part replaced. url.URL.Redacted hides only a password,
// so a credential given as the user name alone (https://TOKEN@host,
// which net/http sends as Basic auth "TOKEN:") would be printed in full.
func redactURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.User == nil {
		return u.String()
	}
	c := *u
	c.User = url.User("xxxxx")
	return c.String()
}

// Sanitized returns a representation of the Config suitable for startup
// logging. Admin credentials are deliberately excluded; only the path to the
// secret mount is included. URLs are redacted: the user:password@ part
// of one (only BRIDGE_OIDC_JWKS_URL may carry it) is replaced by "xxxxx".
func (c *Config) Sanitized() map[string]string {
	out := map[string]string{
		EnvClusterName:          c.ClusterName,
		EnvNamespace:            c.Namespace,
		EnvOIDCIssuer:           redactURL(c.OIDCIssuer),
		EnvHarborURL:            redactURL(c.HarborURL),
		EnvHarborAdminDir:       c.HarborAdminDir,
		EnvHarborRobotPrefix:    c.HarborRobotPrefix,
		EnvForceLocalValidation: strconv.FormatBool(c.ForceLocalValidation),
		EnvLogLevel:             c.LogLevel,
		EnvAudience:             c.Audience,
		EnvHarborAllowHTTP:      strconv.FormatBool(c.HarborAllowHTTP),
		EnvTokenMaxLifetime:     c.TokenMaxLifetime.String(),
		EnvRequirePodBoundToken: strconv.FormatBool(c.RequirePodBoundToken),
	}
	if c.HarborCAFile != "" {
		out[EnvHarborCAFile] = c.HarborCAFile
	}
	if c.selective() {
		out[EnvHarborAccessSelector] = c.HarborAccessSelector.String()
		out[EnvInstance] = c.Instance
	}
	if c.OIDCJWKSURL != nil {
		out[EnvOIDCJWKSURL] = redactURL(c.OIDCJWKSURL)
	}
	if c.OIDCCAFile != "" {
		out[EnvOIDCCAFile] = c.OIDCCAFile
	}
	if c.OIDCTokenFile != "" {
		out[EnvOIDCTokenFile] = c.OIDCTokenFile
	}
	if n := c.Nexus; n != nil {
		out[EnvHarborRegistryHosts] = hostList(c.HarborRegistryHosts)
		out[EnvNexusURL] = redactURL(n.URL)
		out[EnvNexusAdminDir] = n.AdminDir
		out[EnvNexusAllowHTTP] = strconv.FormatBool(n.AllowHTTP)
		out[EnvNexusRegistryHosts] = hostList(n.RegistryHosts)
		out[EnvNexusRateLimitBackoff] = n.RateLimitBackoff.String()
		if n.CAFile != "" {
			out[EnvNexusCAFile] = n.CAFile
		}
	}
	return out
}

func hostList(hosts []registryhost.Host) string {
	parts := make([]string, len(hosts))
	for i, h := range hosts {
		parts[i] = h.String()
	}
	return strings.Join(parts, ",")
}

// AdminCreds is the loaded Harbor admin / system-robot credentials.
type AdminCreds struct {
	Username string
	Password string
}

// LoadAdminCreds reads the Harbor admin credentials from the directory
// referenced by HarborAdminDir. Layout matches the standard Kubernetes
// Secret-as-volume convention: each key becomes a file whose contents are
// the corresponding value. We require keys "username" and "password".
// Both come from the same version of the volume (readAdminCredsDir).
func (c *Config) LoadAdminCreds() (*AdminCreds, error) {
	return readAdminCredsDir(c.HarborAdminDir)
}

func readSecretFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	v := strings.TrimRight(string(data), "\r\n")
	if v == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return v, nil
}

// envBoolOr parses a boolean environment variable; unset or unparsable
// values return def.
func envBoolOr(key string, def bool) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return def
	}
	return v
}
