// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"context"
	"testing"
)

func TestClient_RefusesWildcardProjectGrant(t *testing.T) {
	fake := newFakeHarbor(t)
	srv := fake.server()
	defer srv.Close()
	c := newClientFor(t, srv, "", "")
	if _, err := c.Create(context.Background(), "bridge-c.ns.sa", "", []ProjectPermission{{Project: "*", Action: "pull,push"}}); err == nil {
		t.Fatal("Create sent a wildcard project grant to Harbor")
	}
	if err := c.Update(context.Background(), &Robot{ID: 1, WireName: "robot$bridge-c.ns.sa"}, "", []ProjectPermission{{Project: "*", Action: "pull"}}); err == nil {
		t.Fatal("Update sent a wildcard project grant to Harbor")
	}
}
