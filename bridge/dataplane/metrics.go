// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"errors"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// Label values for bridge_credential_issuances_total{result}. One increment
// per HTTP request, mutually exclusive with each other.
const (
	ResultOK           = "ok"
	ResultUnauthorized = "unauthorized"
	ResultForbidden    = "forbidden"
	ResultUnavailable  = "unavailable"
	ResultBadRequest   = "bad_request"
	ResultServerError  = "server_error"
	ResultRateLimited  = "rate_limited"
)

// Label values for bridge_oidc_validation_failures_total{reason}, also
// the audit line's category for invalid_token (keys_unavailable is a
// "credential unavailable" line instead). go-oidc/v3 returns error
// strings; we substring-match into these stable buckets because go-oidc
// does not expose typed error categories. The validator's own checks
// (ADR-0028) return sentinel errors and are matched with errors.Is. If
// go-oidc later adds typed errors, swap the classifier in
// classifyOIDCError without churning callers.
const (
	OIDCReasonExpired           = "expired"
	OIDCReasonBadSignature      = "bad_signature"
	OIDCReasonWrongIssuer       = "wrong_issuer"
	OIDCReasonMalformed         = "malformed"
	OIDCReasonExcessiveLifetime = "excessive_lifetime"
	OIDCReasonNotPodBound       = "not_pod_bound"
	// OIDCReasonKeysUnavailable is not a bad token: the bridge could not
	// fetch the signing keys to judge it (ErrSigningKeysUnavailable).
	OIDCReasonKeysUnavailable = "keys_unavailable"
	OIDCReasonOther           = "other"
)

// Metrics is the set of Prometheus collectors exported by the data plane.
// Construct with NewMetrics and pass into the Handler. Registration is
// done at construction time so a programming error (duplicate name)
// surfaces at startup, not on first request.
type Metrics struct {
	Issuances                  *prometheus.CounterVec
	OIDCValidationFailures     *prometheus.CounterVec
	HarborAccessLookupFailures prometheus.Counter
	RobotSecretMissing         prometheus.Counter
	IssuanceDuration           prometheus.Histogram

	// Nexus holds the Nexus backend's collectors (NewNexusMetrics); nil
	// without the backend.
	Nexus *NexusMetrics
}

// NexusMetrics are the data plane's collectors of the Nexus backend
// (ADR-0033). main registers them only with the backend, so a Harbor-only
// bridge exports exactly the series it did before. The metrics above
// count every request, the Nexus ones included; these count the requests
// whose image routed to Nexus, from the routing on.
type NexusMetrics struct {
	// Issuances is bridge_nexus_credential_issuances_total{result}: the
	// Nexus share of bridge_credential_issuances_total, with the same
	// result values. Requests refused before routing (rate limit, missing
	// bearer, bad body, forceLocalValidation off) cannot be attributed and
	// count only in the total.
	Issuances *prometheus.CounterVec

	// NexusAccessLookupFailures counts Kubernetes API failures while
	// listing NexusAccess objects.
	NexusAccessLookupFailures prometheus.Counter

	// UserSecretMissing counts requests whose matched NexusAccess had no
	// usable nexususer Secret: absent, or holding no complete password
	// yet (HTTP 503).
	UserSecretMissing prometheus.Counter
}

// nexusResults are the result values a request routed to Nexus can end
// with.
var nexusResults = []string{ResultOK, ResultUnauthorized, ResultForbidden, ResultUnavailable, ResultServerError}

// NewNexusMetrics constructs the Nexus backend's collectors and registers
// them on reg. Set the result as Metrics.Nexus.
func NewNexusMetrics(reg prometheus.Registerer) *NexusMetrics {
	m := &NexusMetrics{
		Issuances: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bridge_nexus_credential_issuances_total",
			Help: "Credential-issuance HTTP requests whose image belongs to the Nexus backend, labelled by outcome (a subset of bridge_credential_issuances_total).",
		}, []string{"result"}),
		NexusAccessLookupFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "bridge_nexusaccess_lookup_failures_total",
			Help: "Kubernetes API failures while listing NexusAccess objects in the credential-issuance hot path.",
		}),
		UserSecretMissing: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "bridge_nexus_user_secret_missing_total",
			Help: "Requests where the matched NexusAccess's user Secret was absent from the bridge namespace or held no complete password yet (returned HTTP 503).",
		}),
	}
	reg.MustRegister(m.Issuances, m.NexusAccessLookupFailures, m.UserSecretMissing)
	for _, r := range nexusResults {
		m.Issuances.WithLabelValues(r)
	}
	return m
}

// recordNexusResult counts a request routed to Nexus in the Nexus
// metrics.
func (h *Handler) recordNexusResult(result string) {
	if h.Metrics == nil || h.Metrics.Nexus == nil {
		return
	}
	h.Metrics.Nexus.Issuances.WithLabelValues(result).Inc()
}

// NewMetrics constructs the metric collectors and registers them on reg.
// Pass controller-runtime's metrics.Registry from main.go: the manager's
// metrics server serves it with the reconciler's metrics, on its own port
// and never on the credential listener (ADR-0025). Pass
// prometheus.NewRegistry() in tests for isolation.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Issuances: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bridge_credential_issuances_total",
			Help: "Total credential-issuance HTTP requests, labelled by outcome.",
		}, []string{"result"}),
		OIDCValidationFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "bridge_oidc_validation_failures_total",
			Help: "OIDC token validation failures, labelled by reason. Reasons are bucketed from go-oidc error strings.",
		}, []string{"reason"}),
		HarborAccessLookupFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "bridge_harboraccess_lookup_failures_total",
			Help: "Kubernetes API failures while listing HarborAccess CRs in the credential-issuance hot path.",
		}),
		RobotSecretMissing: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "bridge_robot_secret_missing_total",
			Help: "Requests where the matched HarborAccess CR's robot Secret was absent from the bridge namespace (returned HTTP 503).",
		}),
		IssuanceDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "bridge_credential_issuance_duration_seconds",
			Help:    "End-to-end credential-issuance latency, including OIDC validation and Kubernetes API calls.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms .. ~4s
		}),
	}
	reg.MustRegister(
		m.Issuances,
		m.OIDCValidationFailures,
		m.HarborAccessLookupFailures,
		m.RobotSecretMissing,
		m.IssuanceDuration,
	)
	// Touch every label value so the time series exist as zero before the
	// first request. Without this, dashboards using rate() on a never-yet-
	// incremented series have to special-case missing data.
	for _, r := range []string{ResultOK, ResultUnauthorized, ResultForbidden, ResultUnavailable, ResultBadRequest, ResultServerError, ResultRateLimited} {
		m.Issuances.WithLabelValues(r)
	}
	for _, r := range []string{OIDCReasonExpired, OIDCReasonBadSignature, OIDCReasonWrongIssuer, OIDCReasonMalformed, OIDCReasonExcessiveLifetime, OIDCReasonNotPodBound, OIDCReasonKeysUnavailable, OIDCReasonOther} {
		m.OIDCValidationFailures.WithLabelValues(r)
	}
	return m
}

// classifyOIDCError buckets a Validate error into one of the OIDCReason*
// label values. The validator's own checks are matched by their sentinel
// errors first. go-oidc does not expose typed error categories so we match
// on the message prefix it emits. Anything we don't recognise falls into
// "other" — that bucket should stay near zero in steady state; spikes
// signal a new go-oidc error path worth adding to this classifier.
func classifyOIDCError(err error) string {
	switch {
	case err == nil:
		return OIDCReasonOther
	case errors.Is(err, ErrTokenLifetime):
		return OIDCReasonExcessiveLifetime
	case errors.Is(err, ErrTokenNotPodBound):
		return OIDCReasonNotPodBound
	case errors.Is(err, ErrSigningKeysUnavailable):
		return OIDCReasonKeysUnavailable
	}
	s := strings.ToLower(err.Error())
	switch {
	// First: go-oidc's "oidc: malformed jwt: ..." for a token go-jose
	// cannot parse quotes the token's alg header, which the sender
	// chooses (alg "expired" must not count as an expired token), and for
	// an alg=none or HS256 probe it names the signature too.
	case strings.Contains(s, "malformed"):
		return OIDCReasonMalformed
	case strings.Contains(s, "expired"):
		return OIDCReasonExpired
	case strings.Contains(s, "signature"):
		return OIDCReasonBadSignature
	// go-oidc's real message is "oidc: id token issued by a different
	// provider, expected %q got %q" — it never contains "issuer".
	case strings.Contains(s, "different provider"), strings.Contains(s, "issuer"):
		return OIDCReasonWrongIssuer
	case strings.Contains(s, "parse"),
		strings.Contains(s, "invalid json"):
		return OIDCReasonMalformed
	default:
		return OIDCReasonOther
	}
}
