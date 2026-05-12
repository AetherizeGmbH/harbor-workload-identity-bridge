// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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
)

// Label values for bridge_oidc_validation_failures_total{reason}. The
// validator (go-oidc/v3) returns error strings; we substring-match into
// these stable buckets because go-oidc does not expose typed error
// categories. If go-oidc later adds typed errors, swap the classifier in
// classifyOIDCError without churning callers.
const (
	OIDCReasonExpired      = "expired"
	OIDCReasonBadSignature = "bad_signature"
	OIDCReasonWrongIssuer  = "wrong_issuer"
	OIDCReasonMalformed    = "malformed"
	OIDCReasonOther        = "other"
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
}

// NewMetrics constructs the metric collectors and registers them on reg.
// Pass controller-runtime's metrics.Registry from main.go to combine these
// with the reconciler's built-in metrics on a single /metrics endpoint.
// Pass prometheus.NewRegistry() in tests for isolation.
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
	for _, r := range []string{ResultOK, ResultUnauthorized, ResultForbidden, ResultUnavailable, ResultBadRequest, ResultServerError} {
		m.Issuances.WithLabelValues(r)
	}
	for _, r := range []string{OIDCReasonExpired, OIDCReasonBadSignature, OIDCReasonWrongIssuer, OIDCReasonMalformed, OIDCReasonOther} {
		m.OIDCValidationFailures.WithLabelValues(r)
	}
	return m
}

// PromHandler returns an http.Handler that serves the Prometheus exposition
// format for gatherer g. Pass controller-runtime's metrics.Registry to
// expose both data-plane and reconciler metrics under one /metrics path.
func PromHandler(g prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError,
	})
}

// classifyOIDCError buckets a Validate error into one of the OIDCReason*
// label values. go-oidc does not expose typed error categories so we match
// on the message prefix it emits. Anything we don't recognise falls into
// "other" — that bucket should stay near zero in steady state; spikes
// signal a new go-oidc error path worth adding to this classifier.
func classifyOIDCError(err error) string {
	if err == nil {
		return OIDCReasonOther
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "expired"):
		return OIDCReasonExpired
	case strings.Contains(s, "signature"):
		return OIDCReasonBadSignature
	case strings.Contains(s, "issuer"):
		return OIDCReasonWrongIssuer
	case strings.Contains(s, "malformed"),
		strings.Contains(s, "parse"),
		strings.Contains(s, "invalid json"):
		return OIDCReasonMalformed
	default:
		return OIDCReasonOther
	}
}
