// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"encoding/json"
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
