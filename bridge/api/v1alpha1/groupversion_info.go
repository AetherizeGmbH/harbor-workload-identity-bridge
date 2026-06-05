// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains API Schema definitions for the harbor.aetherize.io
// v1alpha1 API group. See docs/adr/0004-trust-policy-as-crd-field.md for the
// CRD shape rationale.
//
// +kubebuilder:object:generate=true
// +groupName=harbor.aetherize.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the group and version used to register objects in this package.
var GroupVersion = schema.GroupVersion{Group: "harbor.aetherize.io", Version: "v1alpha1"}

// schemeBuilder collects the types declared in this package and is
// invoked from AddToScheme. Using runtime.SchemeBuilder (not the
// controller-runtime wrapper) keeps the api package free of a
// controller-runtime import, which is the standard advice.
var schemeBuilder runtime.SchemeBuilder

// AddToScheme adds the types in this group-version to the given scheme.
var AddToScheme = schemeBuilder.AddToScheme

// addKnownTypes is appended to schemeBuilder from each type-defining
// file's init() via a call to addToSchemeBuilder.
func addToSchemeBuilder(fn func(*runtime.Scheme) error) {
	schemeBuilder = append(schemeBuilder, fn)
}
