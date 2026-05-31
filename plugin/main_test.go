// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeFetcher is the stand-in for bridgeClient in run() tests. Pinning a
// fixed response (or a fixed error) keeps the protocol assertions
// independent of any HTTP wire detail.
type fakeFetcher struct {
	wantImage string
	wantToken string
	resp      *bridgeResponse
	err       error
}

func (f *fakeFetcher) fetch(image, token string) (*bridgeResponse, error) {
	f.wantImage = image
	f.wantToken = token
	return f.resp, f.err
}

func TestRun_HappyPath(t *testing.T) {
	req := credentialProviderRequest{
		APIVersion:          credentialProviderAPIVersion,
		Kind:                requestKind,
		Image:               "harbor.example.com/library/alpine:3.20",
		ServiceAccountToken: "the-sa-token",
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &fakeFetcher{resp: &bridgeResponse{
		Username:      "robot$bridge-prod-foo",
		Password:      "s3cret",
		ExpiresInSecs: 3600,
		CacheKeyType:  "ServiceAccount",
	}}

	var stdout bytes.Buffer
	if err := run(bytes.NewReader(body), &stdout, fetcher); err != nil {
		t.Fatalf("run: %v", err)
	}

	if fetcher.wantImage != req.Image || fetcher.wantToken != req.ServiceAccountToken {
		t.Fatalf("fetcher called with image=%q token=%q; want image=%q token=%q",
			fetcher.wantImage, fetcher.wantToken, req.Image, req.ServiceAccountToken)
	}

	var resp credentialProviderResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("decode stdout: %v", err)
	}
	if resp.APIVersion != credentialProviderAPIVersion || resp.Kind != responseKind {
		t.Errorf("TypeMeta wrong: got %q/%q", resp.APIVersion, resp.Kind)
	}
	if resp.CacheKeyType != "ServiceAccount" {
		t.Errorf("CacheKeyType = %q, want ServiceAccount", resp.CacheKeyType)
	}
	if resp.CacheDuration != (time.Hour).String() {
		t.Errorf("CacheDuration = %q, want %q", resp.CacheDuration, (time.Hour).String())
	}
	creds, ok := resp.Auth["harbor.example.com"]
	if !ok {
		t.Fatalf("Auth map missing host harbor.example.com: %v", resp.Auth)
	}
	if creds.Username != "robot$bridge-prod-foo" || creds.Password != "s3cret" {
		t.Errorf("Auth creds wrong: %+v", creds)
	}
}

func TestRun_HostWithPort(t *testing.T) {
	req := credentialProviderRequest{
		Image:               "harbor.example.com:8443/p/r:tag",
		ServiceAccountToken: "tok",
	}
	body, _ := json.Marshal(req)
	fetcher := &fakeFetcher{resp: &bridgeResponse{
		Username: "u", Password: "p", ExpiresInSecs: 60, CacheKeyType: "ServiceAccount",
	}}

	var stdout bytes.Buffer
	if err := run(bytes.NewReader(body), &stdout, fetcher); err != nil {
		t.Fatal(err)
	}
	var resp credentialProviderResponse
	_ = json.Unmarshal(stdout.Bytes(), &resp)
	if _, ok := resp.Auth["harbor.example.com:8443"]; !ok {
		t.Errorf("Auth key should retain port; got keys: %v", keysOf(resp.Auth))
	}
}

func TestRun_BridgeRefused_WritesEmptyAuth(t *testing.T) {
	req := credentialProviderRequest{
		Image:               "harbor.example.com/x:1",
		ServiceAccountToken: "tok",
	}
	body, _ := json.Marshal(req)
	fetcher := &fakeFetcher{err: errBridgeRefused}

	var stdout bytes.Buffer
	if err := run(bytes.NewReader(body), &stdout, fetcher); err != nil {
		t.Fatalf("run: %v", err)
	}

	var resp credentialProviderResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatalf("decode stdout: %v", err)
	}
	if resp.CacheKeyType != cacheKeyTypeImage {
		t.Errorf("CacheKeyType = %q, want Image (no caching of refusal)", resp.CacheKeyType)
	}
	if resp.CacheDuration != "0s" {
		t.Errorf("CacheDuration = %q, want 0s", resp.CacheDuration)
	}
	if len(resp.Auth) != 0 {
		t.Errorf("Auth must be empty on bridge refusal; got %v", resp.Auth)
	}
}

func TestRun_BridgeUnavailable_PropagatesError(t *testing.T) {
	req := credentialProviderRequest{
		Image:               "harbor.example.com/x:1",
		ServiceAccountToken: "tok",
	}
	body, _ := json.Marshal(req)
	fetcher := &fakeFetcher{err: errBridgeUnavailable}

	var stdout bytes.Buffer
	err := run(bytes.NewReader(body), &stdout, fetcher)
	if !errors.Is(err, errBridgeUnavailable) {
		t.Fatalf("want errBridgeUnavailable, got %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("nothing should be written to stdout on transient failure; got %q", stdout.String())
	}
}

func TestRun_InvalidJSON(t *testing.T) {
	var stdout bytes.Buffer
	err := run(strings.NewReader("not json"), &stdout, &fakeFetcher{})
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("want decode error, got %v", err)
	}
}

func TestRun_MissingImage(t *testing.T) {
	body, _ := json.Marshal(credentialProviderRequest{ServiceAccountToken: "tok"})
	err := run(bytes.NewReader(body), &bytes.Buffer{}, &fakeFetcher{})
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("want image-missing error, got %v", err)
	}
}

func TestRun_MissingToken(t *testing.T) {
	body, _ := json.Marshal(credentialProviderRequest{Image: "harbor.example.com/x:1"})
	err := run(bytes.NewReader(body), &bytes.Buffer{}, &fakeFetcher{})
	if err == nil || !strings.Contains(err.Error(), "serviceAccountToken") {
		t.Fatalf("want token-missing error, got %v", err)
	}
}

func TestImageHost(t *testing.T) {
	cases := []struct {
		image string
		want  string
	}{
		{"harbor.example.com/library/alpine:3.20", "harbor.example.com"},
		{"harbor.example.com:8443/p/r:tag", "harbor.example.com:8443"},
		{"quay.io/foo/bar@sha256:deadbeef", "quay.io"},
		{"alpine", ""}, // implicit Docker Hub — not our use case, return empty so writeOKResponse rejects it
		{"", ""},
	}
	for _, tc := range cases {
		if got := imageHost(tc.image); got != tc.want {
			t.Errorf("imageHost(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string // substring; empty means success
	}{
		{
			name: "minimum",
			env:  map[string]string{"HARBOR_BRIDGE_ENDPOINT": "https://bridge.example.com"},
		},
		{
			name:    "missing endpoint",
			env:     map[string]string{},
			wantErr: "HARBOR_BRIDGE_ENDPOINT is required",
		},
		{
			name:    "http rejected",
			env:     map[string]string{"HARBOR_BRIDGE_ENDPOINT": "http://bridge.example.com"},
			wantErr: "must use https",
		},
		{
			name: "mTLS both set",
			env: map[string]string{
				"HARBOR_BRIDGE_ENDPOINT":    "https://bridge.example.com",
				"HARBOR_BRIDGE_CLIENT_CERT": "/etc/foo.crt",
				"HARBOR_BRIDGE_CLIENT_KEY":  "/etc/foo.key",
			},
		},
		{
			name: "mTLS only cert is rejected",
			env: map[string]string{
				"HARBOR_BRIDGE_ENDPOINT":    "https://bridge.example.com",
				"HARBOR_BRIDGE_CLIENT_CERT": "/etc/foo.crt",
			},
			wantErr: "must be set together",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := loadConfig(func(k string) string { return tc.env[k] })
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if c.Endpoint == "" {
					t.Fatal("Endpoint must be set on success")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func keysOf(m map[string]authConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
