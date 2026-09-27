// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The plugin duplicates Request and Response instead of importing them
// (ADR-0015), so no compiler links the JSON keys of the two sides. These
// golden files do: the tests here and in plugin/wire_contract_test.go read
// the same files. A key renamed on one side fails that side's test; a
// golden edited to match fails the other side's.
const (
	credentialsRequestGolden  = "testdata/credentials-request.golden.json"
	credentialsResponseGolden = "testdata/credentials-response.golden.json"
)

// jsonObject decodes b as one JSON object, keeping numbers exact.
func jsonObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode JSON object %q: %v", b, err)
	}
	return m
}

func readGolden(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The response for the fixture must be exactly the golden: same keys, no
// extra ones, same values. The plugin reads expires_in as the kubelet
// cache duration; a renamed key there silently turned caching off.
func TestHandler_ResponseMatchesWireGolden(t *testing.T) {
	fx := newHandlerFixture(t)
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, bearerReq(t, "harbor.example.com/production/myimg:v1"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	got := jsonObject(t, w.Body.Bytes())
	want := jsonObject(t, readGolden(t, credentialsResponseGolden))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("response = %v\nwant the golden %s: %v", got, credentialsResponseGolden, want)
	}
}

// The request body the plugin sends (the golden) must reach the audit line.
func TestHandler_DecodesWireGoldenRequest(t *testing.T) {
	fx := newHandlerFixture(t)
	var audit captured
	fx.Handler.Audit = audit.logger()
	r := httptest.NewRequest(http.MethodPost, CredentialsPath,
		bytes.NewReader(readGolden(t, credentialsRequestGolden)))
	r.Header.Set("Authorization", "Bearer some-sa-token")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	fx.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	want := `"requested_image"="harbor.example.com/production/myimg:v1"`
	if got := audit.joined(); !strings.Contains(got, want) {
		t.Errorf("audit log %q lacks %s", got, want)
	}
}
