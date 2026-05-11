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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group and version used to register objects in this package.
var GroupVersion = schema.GroupVersion{Group: "harbor.aetherize.io", Version: "v1alpha1"}

// SchemeBuilder collects the types in this package for scheme registration.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the types in this group-version to the given scheme.
var AddToScheme = SchemeBuilder.AddToScheme
