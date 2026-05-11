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
)

// Environment variable names. Constants so wiring (Helm chart, Deployment
// template, tests) has a single source of truth.
const (
	EnvClusterName          = "BRIDGE_CLUSTER_NAME"
	EnvOIDCIssuer           = "BRIDGE_OIDC_ISSUER"
	EnvHarborURL            = "BRIDGE_HARBOR_URL"
	EnvHarborAdminDir       = "BRIDGE_HARBOR_ADMIN_DIR"
	EnvForceLocalValidation = "BRIDGE_FORCE_LOCAL_VALIDATION"
	EnvLogLevel             = "BRIDGE_LOG_LEVEL"

	clusterNamePattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	clusterNameMaxLen  = 63
	defaultLogLevel    = "info"

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
	// (bridge-<ClusterName>-<saNs>-<saName>) and as the basis of the
	// ownership-prefix safety invariant.
	ClusterName string

	// OIDCIssuer is the cluster's service-account token issuer. The data
	// plane validates inbound SA tokens against this issuer; the control
	// plane uses it to detect HarborAccess CRs whose trustPolicy.issuer
	// disagrees with the cluster the bridge is running in.
	OIDCIssuer *url.URL

	// HarborURL is the base URL of the Harbor instance this bridge manages
	// robots in.
	HarborURL *url.URL

	// HarborAdminDir is the path to a Kubernetes Secret mounted as a volume,
	// containing files named "username" and "password" with the Harbor admin
	// (or per-cluster system robot) credentials. See ADR-0009 for the
	// per-cluster-system-robot recommendation.
	HarborAdminDir string

	// ForceLocalValidation gates whether the data plane performs full local
	// OIDC validation. Defaults to true. The "false" path is plumbed for
	// the post-upstream-migration scenario described in ADR-0002 and
	// docs/MIGRATION.md but is not implemented yet.
	ForceLocalValidation bool

	// LogLevel is one of debug, info, warn, error.
	LogLevel string
}

// LoadFromEnv reads bridge configuration from BRIDGE_* environment variables
// and validates the result. All validation failures are joined and returned
// at once so operators do not have to fix-restart-fix in a loop.
func LoadFromEnv() (*Config, error) {
	cfg := &Config{
		LogLevel:             defaultLogLevel,
		ForceLocalValidation: true,
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

	if v, err := requireURL(os.Getenv(EnvOIDCIssuer), EnvOIDCIssuer); err != nil {
		errs = append(errs, err)
	} else {
		cfg.OIDCIssuer = v
	}

	if v, err := requireURL(os.Getenv(EnvHarborURL), EnvHarborURL); err != nil {
		errs = append(errs, err)
	} else {
		cfg.HarborURL = v
	}

	cfg.HarborAdminDir = strings.TrimSpace(os.Getenv(EnvHarborAdminDir))
	if cfg.HarborAdminDir == "" {
		errs = append(errs, fmt.Errorf("%s is required", EnvHarborAdminDir))
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
	return map[string]string{
		EnvClusterName:          c.ClusterName,
		EnvOIDCIssuer:           c.OIDCIssuer.String(),
		EnvHarborURL:            c.HarborURL.String(),
		EnvHarborAdminDir:       c.HarborAdminDir,
		EnvForceLocalValidation: strconv.FormatBool(c.ForceLocalValidation),
		EnvLogLevel:             c.LogLevel,
	}
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
