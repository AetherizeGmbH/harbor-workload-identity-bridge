# Root module for the GKE e2e harness (ADR-0022). Orchestration lives
# in tests/02-gke.tftest.hcl; this file only declares the file-scope
# variables the tftest consumes. The `default`s are documentation only:
# inside a run block `var` holds just CLI/TF_VAR values, so the test file
# reads every optional variable with try(var.x, <same default>). Reuses ../e2e/modules/* wherever the
# kind harness's modules are cluster-agnostic (docker-build, harbor,
# k8s-yaml, test-exec-pod, harbor-bridge-install, test-sleep); the
# GKE-specific modules live in ./modules.
#
# COST WARNING: this harness creates a real GKE cluster (zonal, spot
# nodes), an Artifact Registry repository, and a static external IP in
# YOUR project. `tofu test` destroys everything at the end of the run,
# but an aborted run can leave resources behind — check with
# `gcloud container clusters list` / `gcloud artifacts repositories list`.
#
# Prerequisites (see HOW-TO-TEST.md):
#   - gcloud auth application-default login   (terraform google provider)
#   - gcloud auth login                       (docker push to Artifact Registry)
#   - GOOGLE_PROJECT exported (make e2e-gke maps it to TF_VAR_gcp_project)
#   - Billing enabled on the project; GKE + Artifact Registry APIs enabled.
terraform {
  required_version = ">= 1.12"
}

variable "gcp_project" {
  type        = string
  description = "GCP project the harness creates its (billed!) resources in. No default on purpose — set via TF_VAR_gcp_project or make e2e-gke with GOOGLE_PROJECT."
}

variable "gcp_zone" {
  type        = string
  default     = "europe-west3-a"
  description = "Zone for the (zonal) GKE cluster; its region hosts the Artifact Registry repo and the static IP."
}

variable "gke_version_prefix" {
  type        = string
  default     = "1.34."
  description = "GKE version prefix. Must be >= 1.34: the KubeletServiceAccountTokenForCredentialProviders gate (KEP-4412) is beta+default-on since 1.34 and GKE cannot set custom kubelet feature gates (ADR-0022)."
}

variable "machine_type" {
  type        = string
  default     = "e2-standard-4"
  description = "Node machine type. Harbor plus the bridge need real headroom; e2-standard-4 spot keeps that affordable."
}

variable "node_count" {
  type        = number
  default     = 2
  description = "Nodes in the (spot) pool."
}

variable "version_harbor" {
  type        = string
  default     = null
  description = "Harbor Helm chart version override, same semantics as the kind harness."
}

variable "pause_after_pull" {
  type        = bool
  default     = false
  description = "Pause after all assertions (file-sleep) for kubectl inspection; same semantics as the kind harness."
}
