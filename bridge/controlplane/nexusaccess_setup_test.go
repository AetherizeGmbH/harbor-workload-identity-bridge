// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/labels"

	nexusv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/nexus/v1alpha1"
	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
	"github.com/aetherize/harbor-workload-identity-bridge/bridge/controlplane/nexus/nexustest"
)

func TestNewNexusClient(t *testing.T) {
	nx := nexustest.New(t)
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "username"), nexustest.AdminUsername)
	mustWrite(t, filepath.Join(dir, "password"), nexustest.AdminPassword)
	n := &NexusConfig{URL: nx.URL, AdminDir: dir, AllowHTTP: true}
	c, err := NewNexusClient(n, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListRoles(context.Background(), "bridge-"); err != nil {
		t.Errorf("ListRoles with the credentials from the directory: %v", err)
	}

	// The credentials are read per call: a rotated password is used at
	// once, and a wrong one fails the next call.
	mustWrite(t, filepath.Join(dir, "password"), "rotated-away")
	if _, err := c.ListRoles(context.Background(), "bridge-"); err == nil {
		t.Error("the client kept using the previous password")
	}

	for name, bad := range map[string]*NexusConfig{
		"plain http not allowed": {URL: nx.URL, AdminDir: dir},
		"missing credentials":    {URL: nx.URL, AdminDir: t.TempDir(), AllowHTTP: true},
		"CA file missing":        {URL: nx.URL, AdminDir: dir, AllowHTTP: true, CAFile: filepath.Join(dir, "absent.pem")},
		"CA without certificate": {URL: nx.URL, AdminDir: dir, AllowHTTP: true, CAFile: filepath.Join(dir, "username")},
		"not configured":         {},
	} {
		if _, err := NewNexusClient(bad, logr.Discard()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestSetupNexus_WithoutNexusDoesNothing(t *testing.T) {
	// A nil manager proves nothing is touched.
	if err := SetupNexus(nil, testReconcilerConfig(), logr.Discard(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestConfig_CachesNexusAccessOnlyWithNexus(t *testing.T) {
	cfg := testReconcilerConfig()
	for obj := range cfg.CacheOptions().ByObject {
		if _, ok := obj.(*nexusv1alpha1.NexusAccess); ok {
			t.Error("NexusAccess cached without the Nexus backend")
		}
	}
	if got := len(cfg.CachedObjects()); got != len(cachedObjects()) {
		t.Errorf("CachedObjects without Nexus has %d types", got)
	}
	cfg.HarborAccessSelector, cfg.Instance = mustSelector(t, "bridge=eu"), "eu"
	cfg.Nexus = &NexusConfig{}
	var found bool
	for obj, by := range cfg.CacheOptions().ByObject {
		switch obj.(type) {
		case *nexusv1alpha1.NexusAccess:
			found = true
			if by.Label == nil || by.Label.String() != "bridge=eu" {
				t.Errorf("NexusAccess cache selector = %v", by.Label)
			}
		case *harborv1alpha1.HarborAccess:
			if by.Label == nil || by.Label.String() != "bridge=eu" {
				t.Errorf("HarborAccess cache selector = %v", by.Label)
			}
		}
	}
	if !found {
		t.Error("NexusAccess not cached with the Nexus backend")
	}
	objs := cfg.CachedObjects()
	if len(objs) != len(cachedObjects())+1 {
		t.Fatalf("CachedObjects with Nexus = %d types", len(objs))
	}
	if _, ok := objs[len(objs)-1].(*nexusv1alpha1.NexusAccess); !ok {
		t.Errorf("last cached object = %T", objs[len(objs)-1])
	}
}

func mustSelector(t *testing.T, s string) labels.Selector {
	t.Helper()
	sel, err := labels.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}
