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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
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

	// instanceMaxLen keeps the per-instance finalizer's name part
	// ("robot-<instance>") within the 63-character limit.
	instanceMaxLen = 50
	audienceMaxLen = 253

	clusterNamePattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	clusterNameMaxLen  = 63
	defaultLogLevel    = "info"

	// defaultHarborRobotPrefix is Harbor's default robot_name_prefix.
	defaultHarborRobotPrefix = "robot$"

	adminUsernameKey = "username"
	adminPasswordKey = "password"
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
	// per-cluster-system-robot recommendation.
	HarborAdminDir string

	// HarborRobotPrefix is the robot name prefix the Harbor instance is
	// configured with (Harbor's robot_name_prefix, default "robot$").
	// Harbor stores robot names without it and reports them with it; the
	// Harbor client strips it on read paths (ADR-0014). Set it when the
	// Harbor administrator changed the prefix — otherwise the bridge cannot
	// recognise its own robots when it lists them.
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

// Selects reports whether this bridge manages ha.
func (c *Config) Selects(ha *harborv1alpha1.HarborAccess) bool {
	return !c.selective() || c.HarborAccessSelector.Matches(labels.Set(ha.Labels))
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
		HarborRobotPrefix:    defaultHarborRobotPrefix,
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
	} else {
		cfg.OIDCIssuer = v
	}

	if raw := strings.TrimSpace(os.Getenv(EnvOIDCJWKSURL)); raw != "" {
		// Optional — when set, must still parse as a URL with a scheme
		// and host. Same shape as the other URL knobs.
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
	} else if v.Scheme == "http" && !cfg.HarborAllowHTTP {
		errs = append(errs, fmt.Errorf("%s %q uses plain http: the Harbor admin credentials and robot passwords would travel unencrypted. Use https (with %s for a private CA), or set %s=true", EnvHarborURL, v.String(), EnvHarborCAFile, EnvHarborAllowHTTP))
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

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid bridge configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func requireURL(raw, name string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", name, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s %q must use http or https scheme", name, raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%s %q must include a host", name, raw)
	}
	return u, nil
}

// Sanitized returns a representation of the Config suitable for startup
// logging. Admin credentials are deliberately excluded; only the path to the
// secret mount is included.
func (c *Config) Sanitized() map[string]string {
	out := map[string]string{
		EnvClusterName:          c.ClusterName,
		EnvNamespace:            c.Namespace,
		EnvOIDCIssuer:           c.OIDCIssuer.String(),
		EnvHarborURL:            c.HarborURL.String(),
		EnvHarborAdminDir:       c.HarborAdminDir,
		EnvHarborRobotPrefix:    c.HarborRobotPrefix,
		EnvForceLocalValidation: strconv.FormatBool(c.ForceLocalValidation),
		EnvLogLevel:             c.LogLevel,
		EnvAudience:             c.Audience,
		EnvHarborAllowHTTP:      strconv.FormatBool(c.HarborAllowHTTP),
	}
	if c.HarborCAFile != "" {
		out[EnvHarborCAFile] = c.HarborCAFile
	}
	if c.selective() {
		out[EnvHarborAccessSelector] = c.HarborAccessSelector.String()
		out[EnvInstance] = c.Instance
	}
	if c.OIDCJWKSURL != nil {
		out[EnvOIDCJWKSURL] = c.OIDCJWKSURL.String()
	}
	if c.OIDCCAFile != "" {
		out[EnvOIDCCAFile] = c.OIDCCAFile
	}
	if c.OIDCTokenFile != "" {
		out[EnvOIDCTokenFile] = c.OIDCTokenFile
	}
	return out
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
func (c *Config) LoadAdminCreds() (*AdminCreds, error) {
	username, err := readSecretFile(filepath.Join(c.HarborAdminDir, adminUsernameKey))
	if err != nil {
		return nil, err
	}
	password, err := readSecretFile(filepath.Join(c.HarborAdminDir, adminPasswordKey))
	if err != nil {
		return nil, err
	}
	return &AdminCreds{Username: username, Password: password}, nil
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
