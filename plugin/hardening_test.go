// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWriteOKResponse_ClampsAndValidates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resp    bridgeResponse
		want    string
		wantErr bool
	}{
		{"normal", bridgeResponse{Username: "u", Password: "p", ExpiresInSecs: 3600, CacheKeyType: "Registry"}, "1h0m0s", false},
		{"negative", bridgeResponse{Username: "u", Password: "p", ExpiresInSecs: -5, CacheKeyType: "Registry"}, "0s", false},
		{"beyond 24h", bridgeResponse{Username: "u", Password: "p", ExpiresInSecs: 1 << 40, CacheKeyType: "Image"}, "24h0m0s", false},
		{"unknown cache key type", bridgeResponse{Username: "u", Password: "p", ExpiresInSecs: 60, CacheKeyType: "ServiceAccount"}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := writeOKResponse(&buf, &tc.resp, "harbor.example.com/p/app:1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			var out credentialProviderResponse
			if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.CacheDuration != tc.want {
				t.Errorf("cacheDuration = %q, want %q", out.CacheDuration, tc.want)
			}
		})
	}
}

// The request carries the pod's ServiceAccount token; a redirect must not
// carry it anywhere.
func TestFetch_DoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		elsewhere.Add(1)
	}))
	defer other.Close()
	bridge := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer bridge.Close()

	cfg := &config{Endpoint: bridge.URL}
	c, err := newBridgeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Trust the test servers' certificate, keep the production redirect policy.
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", c.http.Transport)
	}
	tr.TLSClientConfig = bridge.Client().Transport.(*http.Transport).TLSClientConfig.Clone()

	if _, err := c.fetch("harbor.example.com/p/app:1", "sa-token"); err == nil {
		t.Error("fetch succeeded on a redirect")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("redirect target received %d requests", n)
	}
}

func TestLoadConfig_ServerName(t *testing.T) {
	env := map[string]string{"HARBOR_BRIDGE_ENDPOINT": "https://10.0.0.5:31443", "HARBOR_BRIDGE_SERVER_NAME": "harbor-bridge.harbor-bridge-system.svc"}
	c, err := loadConfig(func(k string) string { return env[k] })
	if err != nil || c.ServerName != env["HARBOR_BRIDGE_SERVER_NAME"] {
		t.Fatalf("config = %+v, err = %v", c, err)
	}
	env["HARBOR_BRIDGE_SERVER_NAME"] = "bad name; rm"
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "SERVER_NAME") {
		t.Fatalf("err = %v, want a rejected server name", err)
	}
}
