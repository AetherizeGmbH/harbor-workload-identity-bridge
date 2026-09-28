// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains API Schema definitions for the nexus.aetherize.io
// v1alpha1 API group: NexusAccess, which grants a Kubernetes ServiceAccount
// access to Sonatype Nexus Repository repositories through the bridge. See
// docs/adr/0036-nexus-repository-backend.md.
//
// +kubebuilder:object:generate=true
// +groupName=nexus.aetherize.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version used to register objects in this package.
var GroupVersion = schema.GroupVersion{Group: "nexus.aetherize.io", Version: "v1alpha1"}

// schemeBuilder collects the types declared in this package and is
// invoked from AddToScheme. runtime.SchemeBuilder (not the
// controller-runtime wrapper) keeps the api package free of a
// controller-runtime import, as in harbor.aetherize.io/v1alpha1.
var schemeBuilder runtime.SchemeBuilder

// AddToScheme adds the types in this group-version to the given scheme.
var AddToScheme = schemeBuilder.AddToScheme

// addToSchemeBuilder is called from each type-defining file's init().
func addToSchemeBuilder(fn func(*runtime.Scheme) error) {
	schemeBuilder = append(schemeBuilder, fn)
}
