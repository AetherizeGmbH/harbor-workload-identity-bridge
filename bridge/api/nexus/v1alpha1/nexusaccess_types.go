// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	harborv1alpha1 "github.com/aetherize/harbor-workload-identity-bridge/bridge/api/v1alpha1"
)

// ServiceAccountRef identifies the Kubernetes ServiceAccount a NexusAccess
// grants credentials for (ADR-0010). The control plane derives the Nexus
// role and user names from it (bridge-<cluster>.<namespace>.<name>,
// ADR-0033), and the data plane derives the expected sub claim of incoming
// tokens ("system:serviceaccount:<namespace>:<name>").
type ServiceAccountRef struct {
	// Namespace is the namespace of the ServiceAccount. A namespace is an
	// RFC 1123 label, so at most 63 characters; the Nexus naming relies on
	// that bound (ADR-0033).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`

	// Name is the name of the ServiceAccount.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
}

// TrustPolicy describes the OIDC trust this NexusAccess places in incoming
// ServiceAccount tokens. The expected sub claim is derived from
// ServiceAccountRef, so the policy carries only issuer and audience. The
// bridge's data plane enforces it; Nexus has no trust policies of its own.
type TrustPolicy struct {
	// Issuer is the OIDC issuer expected on incoming ServiceAccount tokens.
	// It must equal the bridge's configured issuer byte for byte (chart:
	// bridge.oidcIssuer).
	// +kubebuilder:validation:Pattern=`^https?://.+`
	// +kubebuilder:validation:MinLength=1
	Issuer string `json:"issuer"`

	// Audience must appear in the aud claim of incoming tokens. It must
	// equal the one audience the bridge serves (chart: plugin.audience,
	// ADR-0026).
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
}

// RepositoryAccess is the permission granted on a Nexus repository.
// +kubebuilder:validation:Enum="pull";"push";"pull,push"
type RepositoryAccess string

// Access constants, in the same terms as HarborAccess. "pull" grants the
// repository-view privilege read; "push" grants add and edit; "pull,push"
// grants all three. A pusher declares "pull,push": a push needs read too
// (ADR-0033).
const (
	AccessPull     RepositoryAccess = "pull"
	AccessPush     RepositoryAccess = "push"
	AccessPullPush RepositoryAccess = "pull,push"
)

// RepositoryFormat is the Nexus repository format a grant applies to.
// +kubebuilder:validation:Enum=docker
type RepositoryFormat string

// FormatDocker is the only format v1alpha1 grants. Nexus's "oci" format
// (3.94 and later) authenticates through a separate token realm that has
// not been analysed yet (ADR-0033, question 8); keeping the field makes
// adding it later a non-breaking change.
const FormatDocker RepositoryFormat = "docker"

// RepositoryGrant grants an access on one Nexus repository.
type RepositoryGrant struct {
	// Name is the Nexus repository name, following Nexus's own rule:
	// letters, digits and '-', then also '_' and '.'. Names are
	// case-sensitive. Wildcards such as "*" (every repository) are
	// rejected. At most 167 characters, so that every privilege name
	// derived from it stays within Nexus's 200-character limit.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=167
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9-][a-zA-Z0-9_.-]*$`
	Name string `json:"name"`

	// Access is the permission granted on the repository.
	Access RepositoryAccess `json:"access"`

	// Format is the repository's format. Only docker is supported.
	// +kubebuilder:default=docker
	// +optional
	Format RepositoryFormat `json:"format,omitempty"`
}

// NexusAccessSpec describes the desired state of a NexusAccess.
type NexusAccessSpec struct {
	// ServiceAccountRef identifies the Kubernetes ServiceAccount this
	// object grants access for.
	ServiceAccountRef ServiceAccountRef `json:"serviceAccountRef"`

	// TrustPolicy declares which ServiceAccount tokens may receive
	// credentials through this object.
	TrustPolicy TrustPolicy `json:"trustPolicy"`

	// Repositories are the Nexus repositories the ServiceAccount's Nexus
	// role grants access to. A repository that does not exist is not
	// granted, and the bridge issues no credentials for this object until
	// it exists or leaves the list (Ready=False, reason RepositoryNotFound;
	// ADR-0033).
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=name
	Repositories []RepositoryGrant `json:"repositories"`

	// TokenTTL is the longest time kubelet may cache the credentials the
	// bridge returns for this NexusAccess (the credential provider's
	// cacheDuration). The bridge shortens it further so no cache outlives
	// the next scheduled rotation (ADR-0023). Min 5m, max 24h. Defaults to
	// 1h. A Go duration: units h, m, s, ms, us or ns, e.g. 30m, 1h or
	// 1h30m. Days ("1d") and ISO 8601 ("PT1H") are rejected.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:default="1h"
	// +kubebuilder:validation:XValidation:rule="(oldSelf.hasValue() && self == oldSelf.value()) || (duration(self) >= duration('5m') && duration(self) <= duration('24h'))",message="tokenTTL must be a Go duration between 5m and 24h such as 30m, 1h or 1h30m (units h, m, s, ms, us, ns; days are not supported)",optionalOldSelf=true
	TokenTTL harborv1alpha1.Duration `json:"tokenTTL,omitempty"`
}

// UserRef reports the Nexus user and role the bridge manages for a
// NexusAccess.
type UserRef struct {
	// UserID is the id of the Nexus user whose password the Secret holds:
	// bridge-<cluster>.<saNamespace>.<saName>_<generation>. It changes at
	// every password rotation, because the bridge replaces the user under
	// a new generation instead of changing its password (ADR-0033).
	UserID string `json:"userId,omitempty"`

	// RoleID is the id of the Nexus role that holds the repository
	// privileges: bridge-<cluster>.<saNamespace>.<saName>. It persists
	// across rotations.
	RoleID string `json:"roleId,omitempty"`

	// PasswordSecretRef is the name of the Secret holding the user's
	// password. It lives in the bridge's namespace, not in the
	// NexusAccess's, so a workload cannot read it.
	PasswordSecretRef string `json:"passwordSecretRef,omitempty"`

	// LastRotated is when the current user was created.
	LastRotated *metav1.Time `json:"lastRotated,omitempty"`
}

// NexusAccessStatus is the observed state of a NexusAccess.
type NexusAccessStatus struct {
	// User reports the Nexus user and role provisioned for this object.
	// Empty until the first reconcile succeeds.
	User *UserRef `json:"user,omitempty"`

	// ObservedGeneration is the metadata.generation the bridge last applied
	// in full. It does not advance while the generation is only partly
	// applied (for example a repository that does not exist).
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the Kubernetes standard pattern. Expected types:
	// Ready, UserProvisioned, TrustPolicyApplied.
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Condition types.
const (
	ConditionReady              = "Ready"
	ConditionUserProvisioned    = "UserProvisioned"
	ConditionTrustPolicyApplied = "TrustPolicyApplied"
)

// NexusAccess declares that a Kubernetes ServiceAccount, matched by
// trustPolicy, may pull and/or push specific Nexus repositories via the
// bridge.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=nxa,categories={nexus}
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 characters: it is stamped as a label value on the user Secret"
// +kubebuilder:validation:XValidation:rule="has(self.spec) || (oldSelf.hasValue() && !has(oldSelf.value().spec))",message="spec is required: serviceAccountRef, trustPolicy and repositories",fieldPath=".spec",reason="FieldValueRequired",optionalOldSelf=true
// +kubebuilder:printcolumn:name="SA",type="string",JSONPath=".spec.serviceAccountRef.name"
// +kubebuilder:printcolumn:name="SA-Namespace",type="string",JSONPath=".spec.serviceAccountRef.namespace",priority=1
// +kubebuilder:printcolumn:name="User",type="string",JSONPath=".status.user.userId"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type NexusAccess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is required by the CEL rule above, not by +required, for the
	// same reason as on HarborAccess: the apiserver does not ratchet
	// `required`, and a rule can be relaxed for stored objects later.

	Spec   NexusAccessSpec   `json:"spec,omitempty"`
	Status NexusAccessStatus `json:"status,omitempty"`
}

// NexusAccessList is a list of NexusAccess objects.
//
// +kubebuilder:object:root=true
type NexusAccessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []NexusAccess `json:"items"`
}

func init() {
	addToSchemeBuilder(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &NexusAccess{}, &NexusAccessList{})
		// Required for informer/Watch to decode metav1.WatchEvent under
		// this GroupVersion; runtime.SchemeBuilder does not add it.
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}
