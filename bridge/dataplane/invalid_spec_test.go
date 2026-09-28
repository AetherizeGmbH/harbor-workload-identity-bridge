// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// A HarborAccess whose tokenTTL an older CRD admitted but Go cannot parse
// (e.g. "1d") is refused as InvalidSpec by the reconciler, which stops
// converging its robot and suspends it, deleting its Secret (ADR-0030).
// Until that has happened the robot Secret from before is still there; the
// data plane must not hand it out, although the object otherwise matches
// the token.
func TestHandler_InvalidTokenTTL_NotServed(t *testing.T) {
	for _, raw := range []string{`"1d"`, `"3 hours"`, `"PT10M"`} {
		t.Run(raw, func(t *testing.T) {
			ha := newTestHA()
			if err := json.Unmarshal([]byte(raw), &ha.Spec.TokenTTL); err != nil {
				t.Fatal(err)
			}
			if ha.Spec.TokenTTL.Err() == nil {
				t.Fatalf("tokenTTL %s decoded as valid; the test needs an invalid one", raw)
			}
			reg := prometheus.NewRegistry()
			var audit captured
			h := &Handler{
				K8sClient: newFakeClientBuilder().WithObjects(ha, newTestRobotSecret()).Build(),
				Validator: &stubValidator{claims: newTestClaims()},
				Config:    HandlerConfig{BridgeNamespace: hTestBridgeNS, ForceLocalValidation: true, Audience: hTestAudience},
				Metrics:   NewMetrics(reg),
				Audit:     audit.logger(),
			}

			w := httptest.NewRecorder()
			h.ServeHTTP(w, bearerReq(t, "harbor.example.com/production/img:v1"))

			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
			}
			if body := w.Body.String(); strings.Contains(body, hTestRobotPass) || strings.Contains(body, hTestRobotUser) {
				t.Fatalf("response carries the robot credentials: %s", body)
			}
			out := audit.joined()
			for _, want := range []string{
				`"reason"="invalid_harboraccess_spec"`,
				`"harboraccess"="` + hTestHANs + "/" + hTestHAName + `"`,
				"spec.tokenTTL",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("audit line lacks %s:\n%s", want, out)
				}
			}
			if got := counter(t, reg, "bridge_credential_issuances_total", map[string]string{"result": ResultForbidden}); got != 1 {
				t.Errorf("forbidden issuances = %v, want 1", got)
			}
		})
	}
}
