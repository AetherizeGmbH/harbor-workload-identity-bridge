// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The bridge's side of the /v1/credentials contract, pinned by the bridge's
// handler tests (bridge/dataplane/wire_contract_test.go) against the same
// files. Reading them is not an import: the plugin still compiles without
// any bridge package (ADR-0015).
const (
	bridgeRequestGolden  = "../bridge/dataplane/testdata/credentials-request.golden.json"
	bridgeResponseGolden = "../bridge/dataplane/testdata/credentials-response.golden.json"
)

func secs(n int) *int { return &n }

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

// The body the plugin POSTs must be exactly the request golden.
func TestFetch_RequestBodyMatchesBridgeGolden(t *testing.T) {
	want := jsonObject(t, readGolden(t, bridgeRequestGolden))
	image, ok := want["image"].(string)
	if !ok {
		t.Fatalf("golden has no string image: %v", want)
	}
	okResponse := readGolden(t, bridgeResponseGolden)
	var got []byte
	bc, _ := newTestBridge(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		_, _ = w.Write(okResponse)
	})
	if _, err := bc.fetch(image, "tok"); err != nil {
		t.Fatal(err)
	}
	if g := jsonObject(t, got); !reflect.DeepEqual(g, want) {
		t.Errorf("request body = %v, want the golden %v", g, want)
	}
}

// bridgeResponse must decode every key of the response golden and carry
// no key the bridge does not send. A key renamed here leaves its field
// empty, which fetch refuses; a key renamed on the bridge changes the
// golden, and this round trip no longer matches it.
func TestFetch_DecodesBridgeResponseGolden(t *testing.T) {
	golden := readGolden(t, bridgeResponseGolden)
	bc, _ := newTestBridge(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(golden)
	})
	resp, err := bc.fetch("harbor.example.com/production/myimg:v1", "tok")
	if err != nil {
		t.Fatalf("fetch of the bridge's golden response: %v", err)
	}
	roundTrip, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := jsonObject(t, roundTrip), jsonObject(t, golden); !reflect.DeepEqual(got, want) {
		t.Errorf("bridgeResponse round trip = %v, want the golden %v", got, want)
	}
}

// Kubelet decodes the plugin's stdout strictly (EnableStrict in
// pkg/credentialprovider/plugin/plugin.go): one unknown key fails every
// response. The keys are those of k8s.io/kubelet/pkg/apis/credentialprovider/v1
// (types.go); the request is kubelet's literal JSON, not the plugin's struct.
func TestRun_SpeaksKubeletV1Keys(t *testing.T) {
	req := `{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"CredentialProviderRequest",` +
		`"image":"harbor.example.com/p/app:1","serviceAccountToken":"tok",` +
		`"serviceAccountAnnotations":{"example.com/key":"v"}}`
	fetcher := &fakeFetcher{resp: &bridgeResponse{
		Username: "u", Password: "p", ExpiresInSecs: secs(3600), CacheKeyType: "Registry",
	}}
	var stdout bytes.Buffer
	if err := run(strings.NewReader(req), &stdout, fetcher); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fetcher.gotImage != "harbor.example.com/p/app:1" || fetcher.gotToken != "tok" {
		t.Errorf("fetcher got image %q, token %q", fetcher.gotImage, fetcher.gotToken)
	}
	want := map[string]any{
		"apiVersion":    "credentialprovider.kubelet.k8s.io/v1",
		"kind":          "CredentialProviderResponse",
		"cacheKeyType":  "Registry",
		"cacheDuration": "1h0m0s",
		"auth": map[string]any{
			"harbor.example.com": map[string]any{"username": "u", "password": "p"},
		},
	}
	if got := jsonObject(t, stdout.Bytes()); !reflect.DeepEqual(got, want) {
		t.Errorf("stdout = %v\nwant %v", got, want)
	}
}
