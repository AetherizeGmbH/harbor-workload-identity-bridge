// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TrustPolicy describes the OIDC trust this HarborAccess places in incoming
// service-account tokens. The expected sub claim is derived from
// HarborAccessSpec.ServiceAccountRef (canonical Kubernetes SA subject form:
// "system:serviceaccount:<namespace>:<name>"), so the policy carries only
// issuer + audience here. Wildcards and claim matchers are reserved for
// future versions; see docs/adr/0006-oidc-validation-and-audience.md.
//
// Enforced today by the bridge Data Plane; will be enforced by Harbor
// itself once goharbor/harbor#17520 lands
// (see docs/adr/0004-trust-policy-as-crd-field.md).
type TrustPolicy struct {
	// Issuer is the OIDC issuer expected on incoming service-account tokens.
	// Typically the cluster service-account issuer, e.g. https://kubernetes.default.svc.
	// +kubebuilder:validation:Pattern=`^https?://.+`
	// +kubebuilder:validation:MinLength=1
	Issuer string `json:"issuer"`

	// Audience must appear in the aud claim of incoming SA tokens. Must match the
	// kubelet credential-provider config's serviceAccountTokenAudience for the
	// registry hostname being authenticated.
	// +kubebuilder:validation:MinLength=1
	Audience string `json:"audience"`
}

// HarborAction is the permission granted on a Harbor project.
// +kubebuilder:validation:Enum="pull";"push";"pull,push"
type HarborAction string

// Action constants. The "pull,push" value matches Harbor's API convention
// for combined permissions.
const (
	ActionPull     HarborAction = "pull"
	ActionPush     HarborAction = "push"
	ActionPullPush HarborAction = "pull,push"
)

// ProjectPermission grants an action on a single Harbor project.
type ProjectPermission struct {
	// Project is the Harbor project name (case-sensitive, no leading slash).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Project string `json:"project"`

	// Action is the permission granted on the project.
	Action HarborAction `json:"action"`
}

// ServiceAccountRef identifies the Kubernetes ServiceAccount this HarborAccess
// grants credentials for. It is the canonical workload identity for the CR:
// the control plane uses it to name the Harbor robot
// (bridge-<cluster>-<namespace>-<name>) and the data plane uses it to
// construct the expected sub claim on incoming SA tokens
// ("system:serviceaccount:<namespace>:<name>").
// See docs/adr/0010-service-account-ref-as-identity.md.
type ServiceAccountRef struct {
	// Namespace is the namespace of the ServiceAccount.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`

	// Name is the name of the ServiceAccount.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
}

// HarborAccessSpec describes the desired state of a HarborAccess.
type HarborAccessSpec struct {
	// ServiceAccountRef identifies the Kubernetes ServiceAccount this CR grants
	// access for. The reconciler uses this for robot naming; the data plane
	// uses it to validate that incoming tokens match the intended workload.
	ServiceAccountRef ServiceAccountRef `json:"serviceAccountRef"`

	// TrustPolicy declares which SA tokens may receive credentials via this CR.
	TrustPolicy TrustPolicy `json:"trustPolicy"`

	// Permissions are the Harbor project permissions granted to the resulting
	// Harbor robot account. See docs/adr/0003-persistent-robots-per-harboraccess.md.
	// +kubebuilder:validation:MinItems=1
	Permissions []ProjectPermission `json:"permissions"`

	// TokenTTL bounds how long a docker token issued via this HarborAccess lives.
	// Min 5m, max 24h. Defaults to 1h.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Format=duration
	// +kubebuilder:default="1h"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('5m') && duration(self) <= duration('24h')",message="tokenTTL must be between 5m and 24h"
	TokenTTL metav1.Duration `json:"tokenTTL,omitempty"`
}

// RobotRef references the Harbor robot account managed for this HarborAccess.
type RobotRef struct {
	// Name is the Harbor robot name (typically bridge-<namespace>-<serviceaccount>).
	Name string `json:"name,omitempty"`

	// ID is the Harbor numeric ID of the robot.
	ID int64 `json:"id,omitempty"`

	// PasswordSecretRef is the Kubernetes Secret name holding the robot password.
	// The Secret lives in the bridge's namespace, not the HarborAccess namespace,
	// so a workload SA cannot read it.
	PasswordSecretRef string `json:"passwordSecretRef,omitempty"`

	// LastRotated is the timestamp of the last successful password rotation.
	LastRotated *metav1.Time `json:"lastRotated,omitempty"`
}

// TrustPolicyEnforcer indicates which component currently enforces the trust policy.
// "bridge" before the upstream Harbor migration, "harbor" after.
// +kubebuilder:validation:Enum=bridge;harbor
type TrustPolicyEnforcer string

// Enforcer constants.
const (
	EnforcedByBridge TrustPolicyEnforcer = "bridge"
	EnforcedByHarbor TrustPolicyEnforcer = "harbor"
)

// HarborAccessStatus is the observed state of a HarborAccess.
type HarborAccessStatus struct {
	// Robot reports the Harbor robot provisioned for this CR. Empty until the
	// first reconcile succeeds.
	Robot *RobotRef `json:"robot,omitempty"`

	// TrustPolicyEnforcedBy reports who currently enforces the trust policy
	// (see docs/adr/0004-trust-policy-as-crd-field.md and docs/MIGRATION.md).
	TrustPolicyEnforcedBy TrustPolicyEnforcer `json:"trustPolicyEnforcedBy,omitempty"`

	// ObservedGeneration is the metadata.generation of the most recently
	// reconciled spec.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the Kubernetes standard pattern. Expected types:
	// Ready, RobotProvisioned, TrustPolicyApplied.
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Condition type constants used by the reconciler.
const (
	ConditionReady              = "Ready"
	ConditionRobotProvisioned   = "RobotProvisioned"
	ConditionTrustPolicyApplied = "TrustPolicyApplied"
)

// HarborAccess declares that a Kubernetes ServiceAccount, matched by trustPolicy,
// may pull and/or push specific Harbor projects via the bridge.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ha,categories={harbor}
// +kubebuilder:printcolumn:name="SA",type="string",JSONPath=".spec.serviceAccountRef.name"
// +kubebuilder:printcolumn:name="SA-Namespace",type="string",JSONPath=".spec.serviceAccountRef.namespace",priority=1
// +kubebuilder:printcolumn:name="Robot",type="string",JSONPath=".status.robot.name"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Enforced-By",type="string",JSONPath=".status.trustPolicyEnforcedBy"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type HarborAccess struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HarborAccessSpec   `json:"spec,omitempty"`
	Status HarborAccessStatus `json:"status,omitempty"`
}

// HarborAccessList is a list of HarborAccess objects.
//
// +kubebuilder:object:root=true
type HarborAccessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []HarborAccess `json:"items"`
}

func init() {
	addToSchemeBuilder(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &HarborAccess{}, &HarborAccessList{})
		// Required for informer/Watch to decode metav1.WatchEvent under
		// our GroupVersion. controller-runtime's scheme.Builder used to
		// call this for us; runtime.SchemeBuilder does not.
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}
