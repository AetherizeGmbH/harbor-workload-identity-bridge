## Migration Path

The HarborAccess CRD is a Kubernetes-native declarative interface for "this SA gets these Harbor permissions". It is NOT expected to become an upstream Harbor API directly. Upstream Harbor will define its own configuration model for OIDC Trust (likely server-native, configured via Harbor REST API or UI). Our Bridge translates between the two.

This is the same pattern as ExternalDNS (translates K8s Ingress resources to DNS provider APIs) or cert-manager (translates K8s Certificate resources to ACME or Vault APIs). The K8s-native declarative layer is the value we add, regardless of what the upstream registry's native API looks like.

When upstream Harbor implements OIDC Trust Policies (issue #17520):

1. Upgrade Harbor to a supporting version
2. Extend the Control Plane Reconciler with a translator that writes the trust policy to Harbor's new native model, alongside the existing robot management
3. Update kubelet credential provider config to point plugins at Harbor directly instead of at the Bridge Data Plane
4. Decommission Bridge Data Plane (delete the Deployment, keep the Reconciler)
5. HarborAccess CRD apiVersion likely bumps (v1alpha1 → v1alpha2 or v1) as field structure adapts to Harbor's eventual model. Use standard Kubernetes CRD versioning and conversion webhooks if breaking changes are needed.

The Reconciler stays. The CRD remains as the user-facing declarative interface. The Bridge becomes a pure translator from K8s resources to Harbor API calls. Not a rewrite.