// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// metricsFixture builds a Handler wired to a fresh Prometheus registry so
// tests can assert metric values without colliding with the global
// default registry.
func metricsFixture(t *testing.T) (*handlerFixture, *Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	fx := newHandlerFixture(t)
	fx.Handler.Metrics = m
	return fx, m, reg
}

// counter pulls a single CounterVec sample by label and asserts it.
func counter(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, mtr := range mf.GetMetric() {
			match := true
			for k, v := range labels {
				found := false
				for _, lp := range mtr.GetLabel() {
					if lp.GetName() == k && lp.GetValue() == v {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
			if match {
				return mtr.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestMetrics_OK_IncrementsOKCounterAndDurationHistogram(t *testing.T) {
	fx, _, reg := metricsFixture(t)
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "harbor.example.com/production/x:v1"))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got := counter(t, reg, "bridge_credential_issuances_total", map[string]string{"result": ResultOK}); got != 1 {
		t.Errorf("issuances{result=ok} = %v, want 1", got)
	}
	// Histogram should have one sample.
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() == "bridge_credential_issuance_duration_seconds" {
			if c := mf.GetMetric()[0].GetHistogram().GetSampleCount(); c != 1 {
				t.Errorf("duration sample_count = %d, want 1", c)
			}
		}
	}
}

func TestMetrics_Forbidden_OnSubjectAudienceMismatch(t *testing.T) {
	fx, _, reg := metricsFixture(t)
	fx.Validator.claims.Subject = "system:serviceaccount:wrong:wrong"
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "img"))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d", w.Code)
	}
	if got := counter(t, reg, "bridge_credential_issuances_total", map[string]string{"result": ResultForbidden}); got != 1 {
		t.Errorf("issuances{result=forbidden} = %v, want 1", got)
	}
}

func TestMetrics_OIDCFailureClassification(t *testing.T) {
	cases := []struct {
		err       error
		reason    string
		caseLabel string
	}{
		{errors.New("oidc: token is expired"), OIDCReasonExpired, "expired"},
		{errors.New("oidc: failed to verify signature"), OIDCReasonBadSignature, "bad_signature"},
		{errors.New("oidc: issuer did not match"), OIDCReasonWrongIssuer, "wrong_issuer"},
		{errors.New("oidc: malformed jwt"), OIDCReasonMalformed, "malformed"},
		{errors.New("something else entirely"), OIDCReasonOther, "other"},
	}
	for _, c := range cases {
		t.Run(c.caseLabel, func(t *testing.T) {
			fx, _, reg := metricsFixture(t)
			fx.Validator.err = c.err
			w := httptest.NewRecorder()
			fx.Handler.ServeHTTP(w, bearerReq(t, ""))
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d", w.Code)
			}
			if got := counter(t, reg, "bridge_oidc_validation_failures_total", map[string]string{"reason": c.reason}); got != 1 {
				t.Errorf("oidc_failures{reason=%s} = %v, want 1", c.reason, got)
			}
			if got := counter(t, reg, "bridge_credential_issuances_total", map[string]string{"result": ResultUnauthorized}); got != 1 {
				t.Errorf("issuances{result=unauthorized} = %v, want 1", got)
			}
		})
	}
}

func TestMetrics_SecretMissing_IncrementsBoth503Counters(t *testing.T) {
	// HA but no Secret.
	ha := newTestHA()
	fx, _, reg := metricsFixture(t)
	_ = fx.K8s.Delete(t.Context(), newTestRobotSecret())
	_ = ha
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, ""))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", w.Code)
	}
	if got := counter(t, reg, "bridge_robot_secret_missing_total", nil); got != 1 {
		t.Errorf("robot_secret_missing = %v, want 1", got)
	}
	if got := counter(t, reg, "bridge_credential_issuances_total", map[string]string{"result": ResultUnavailable}); got != 1 {
		t.Errorf("issuances{result=unavailable} = %v, want 1", got)
	}
}

func TestMetrics_NoMetrics_HandlerStillWorks(t *testing.T) {
	// Confirm the metrics-is-nil branch in the handler keeps it usable
	// from tests that don't care about metrics.
	fx := newHandlerFixture(t)
	// fx.Handler.Metrics is intentionally nil
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

func TestPromHandler_ServesExpositionFormat(t *testing.T) {
	reg := prometheus.NewRegistry()
	_ = NewMetrics(reg)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	PromHandler(reg).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Time-series exist as zero before any request because NewMetrics
	// touches every label value.
	for _, want := range []string{
		"bridge_credential_issuances_total",
		"bridge_oidc_validation_failures_total",
		"bridge_harboraccess_lookup_failures_total",
		"bridge_robot_secret_missing_total",
		"bridge_credential_issuance_duration_seconds_bucket",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics output missing %q", want)
		}
	}
}
