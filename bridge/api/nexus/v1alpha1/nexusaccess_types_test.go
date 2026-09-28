// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"
)

// One NexusAccess whose tokenTTL is not a Go duration must not abort the
// decoding of a whole list: the bridge's cache reads every object through
// one LIST (see harborv1alpha1.Duration). The value round-trips unchanged
// and Err reports it.
func TestNexusAccessList_ToleratesAnInvalidTokenTTL(t *testing.T) {
	raw := []byte(`{"items":[
		{"metadata":{"name":"bad"},"spec":{"tokenTTL":"1d"}},
		{"metadata":{"name":"good"},"spec":{"tokenTTL":"30m"}}]}`)
	var list NexusAccessList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("decoded %d items, want 2", len(list.Items))
	}
	if err := list.Items[0].Spec.TokenTTL.Err(); err == nil {
		t.Error("an invalid tokenTTL decoded without an error to report")
	}
	if got := list.Items[1].Spec.TokenTTL.Duration; got != 30*time.Minute {
		t.Errorf("tokenTTL = %s, want 30m", got)
	}
	out, err := json.Marshal(list.Items[0].Spec.TokenTTL)
	if err != nil || string(out) != `"1d"` {
		t.Errorf("invalid tokenTTL re-encoded as %s, %v; want it unchanged", out, err)
	}
}

func TestDeepCopy_IsIndependent(t *testing.T) {
	in := &NexusAccess{Spec: NexusAccessSpec{Repositories: []RepositoryGrant{{Name: "a", Access: AccessPull}}}}
	out := in.DeepCopy()
	out.Spec.Repositories[0].Name = "b"
	if in.Spec.Repositories[0].Name != "a" {
		t.Error("DeepCopy shares the repositories slice")
	}
}
