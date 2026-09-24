// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The aud claim comes from a token anyone can send to the NodePort; it is
// decoded before the signature is checked against a CR's audience.
func FuzzJSONAudience(f *testing.F) {
	for _, seed := range []string{`"a"`, `["a","b"]`, `[]`, `null`, `1`, `{"x":1}`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var a jsonAudience
		if err := json.Unmarshal(raw, &a); err != nil {
			return
		}
		for _, v := range a {
			_ = v
		}
	})
}

// The key set parses every token sent to the NodePort before any trust
// decision; arbitrary input must never panic or verify.
func FuzzCachedKeySet_VerifySignature(f *testing.F) {
	fi := newFixtureIssuer(f)
	srv := httptest.NewServer(http.HandlerFunc(fi.handleJWKS))
	f.Cleanup(srv.Close)
	ks := newCachedKeySet(srv.URL, srv.Client())
	f.Add("")
	f.Add("a.b.c")
	f.Add("eyJhbGciOiJSUzI1NiIsImtpZCI6IngifQ.e30.AAAA")
	f.Add("eyJhbGciOiJub25lIn0.e30.")
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 1<<14 {
			return
		}
		if _, err := ks.VerifySignature(context.Background(), raw); err == nil {
			t.Fatalf("arbitrary input verified: %q", raw)
		}
	})
}
