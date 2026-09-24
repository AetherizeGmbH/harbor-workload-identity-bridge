# Zonal GKE cluster + spot node pool + ephemeral Artifact Registry
# repo + pre-allocated static IP for Harbor's LoadBalancer (ADR-0022).
#
# The static IP is created BEFORE Harbor so the sslip.io hostname
# (harbor.<ip>.sslip.io) is known upfront — no two-phase helm install.
#
# Dataplane V2 (Cilium) mirrors the kind harness's Cilium dataplane;
# the version prefix must stay >= 1.34 so the KEP-4412 feature gate is
# default-on (GKE cannot set custom kubelet feature gates).
terraform {
  required_version = ">= 1.6"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
    http = {
      source  = "hashicorp/http"
      version = "~> 3.4"
    }
  }
}

variable "project" {
  type = string
}

variable "zone" {
  type = string
}

variable "name_prefix" {
  type    = string
  default = "harbor-bridge-e2e"
}

variable "version_prefix" {
  type        = string
  default     = "1.34."
  description = "GKE master/node version prefix; latest matching release is used."
}

variable "machine_type" {
  type    = string
  default = "e2-standard-4"
}

variable "node_count" {
  type    = number
  default = 2
}

variable "master_authorized_cidrs" {
  type        = list(string)
  default     = []
  description = "CIDRs allowed to reach the cluster API. Empty = the public IPv4 of the machine running tofu (detected via api.ipify.org): the harness drives the API only from there, so nothing else needs access."
}

locals {
  # "europe-west3-a" → "europe-west3"
  region = join("-", slice(split("-", var.zone), 0, 2))
}

provider "google" {
  project = var.project
  region  = local.region
  zone    = var.zone
}

# Random suffix so aborted runs don't collide with the next run's
# cluster/repo names while their deletes are still in flight.
resource "random_id" "suffix" {
  byte_length = 3
}

locals {
  name = "${var.name_prefix}-${random_id.suffix.hex}"
}

data "http" "operator_ip" {
  count = length(var.master_authorized_cidrs) == 0 ? 1 : 0
  url   = "https://api.ipify.org"
}

locals {
  master_authorized_cidrs = length(var.master_authorized_cidrs) > 0 ? var.master_authorized_cidrs : ["${chomp(data.http.operator_ip[0].response_body)}/32"]
}

data "google_container_engine_versions" "this" {
  location       = var.zone
  version_prefix = var.version_prefix
}

resource "google_container_cluster" "this" {
  name     = local.name
  location = var.zone

  min_master_version = data.google_container_engine_versions.this.latest_master_version

  # Static version pin (no channel) so the tested KEP-4412 behaviour
  # doesn't shift mid-investigation when a channel rolls forward.
  release_channel {
    channel = "UNSPECIFIED"
  }

  # Dataplane V2 requires VPC-native; empty ip_allocation_policy lets
  # GKE pick the secondary ranges on the default network.
  datapath_provider = "ADVANCED_DATAPATH"
  networking_mode   = "VPC_NATIVE"
  ip_allocation_policy {}

  # The API endpoint is public; restrict who can reach it (Trivy GCP-0061).
  master_authorized_networks_config {
    dynamic "cidr_blocks" {
      for_each = local.master_authorized_cidrs
      content {
        cidr_block   = cidr_blocks.value
        display_name = "e2e-operator"
      }
    }
  }

  # Workload Identity: with GKE_METADATA on the node pool, pods get the
  # GKE metadata server instead of the node's, so they cannot read the
  # node service account's credentials (Trivy GCP-0057). kubelet — and
  # with it GKE's own credential provider — runs on the host network and
  # keeps the node identity.
  workload_identity_config {
    workload_pool = "${var.project}.svc.id.goog"
  }

  remove_default_node_pool = true
  initial_node_count       = 1
  deletion_protection      = false
}

resource "google_container_node_pool" "this" {
  name     = "e2e"
  cluster  = google_container_cluster.this.name
  location = var.zone
  version  = data.google_container_engine_versions.this.latest_master_version

  node_count = var.node_count

  node_config {
    machine_type = var.machine_type
    spot         = true
    disk_size_gb = 100
    # cloud-platform scope + the node service account's default AR
    # read access is what makes GKE's own credential provider able to
    # pull from the Artifact Registry repo below — the merge-mode
    # coexistence assertion depends on it (ADR-0022).
    oauth_scopes = ["https://www.googleapis.com/auth/cloud-platform"]

    # Legacy (v0.1/v1beta1) metadata endpoints off (Trivy GCP-0048).
    metadata = {
      disable-legacy-endpoints = "true"
    }
    workload_metadata_config {
      mode = "GKE_METADATA"
    }
  }
}

# Ephemeral repo for the working-tree images. Destroyed with the run;
# deleting the repo deletes the pushed images with it.
resource "google_artifact_registry_repository" "this" {
  location      = local.region
  repository_id = local.name
  format        = "DOCKER"
}

# Pre-allocated LoadBalancer IP for Harbor → sslip.io hostname known
# before the Harbor install.
resource "google_compute_address" "harbor" {
  name   = "${local.name}-harbor"
  region = local.region
}

data "google_client_config" "this" {}

# The cluster's ServiceAccount token issuer, read from its own discovery
# document instead of assumed: GKE signs tokens with a
# container.googleapis.com issuer, not https://kubernetes.default.svc…,
# and the bridge and every HarborAccess must name it exactly.
data "http" "oidc_discovery" {
  url         = "https://${google_container_cluster.this.endpoint}/.well-known/openid-configuration"
  ca_cert_pem = base64decode(google_container_cluster.this.master_auth[0].cluster_ca_certificate)
  request_headers = {
    Authorization = "Bearer ${data.google_client_config.this.access_token}"
  }
  depends_on = [google_container_node_pool.this]
}

output "name" {
  value = google_container_cluster.this.name
}

output "kubeconfig" {
  description = "Token-auth connection object, same shape the kind module outputs (client-cert fields stay null). The access token lives ~1h — enough for a full run; a stale-token failure on very long pauses means re-running the stage."
  value = {
    host                   = "https://${google_container_cluster.this.endpoint}"
    cluster_ca_certificate = base64decode(google_container_cluster.this.master_auth[0].cluster_ca_certificate)
    token                  = data.google_client_config.this.access_token
  }
  sensitive = true
}

output "oidc_issuer" {
  value       = jsondecode(data.http.oidc_discovery.response_body).issuer
  description = "ServiceAccount token issuer of this cluster (from /.well-known/openid-configuration)."
}

output "ar_registry_host" {
  value       = "${local.region}-docker.pkg.dev"
  description = "Artifact Registry docker host — the target of `gcloud auth configure-docker`."
}

output "ar_repo" {
  value       = "${local.region}-docker.pkg.dev/${var.project}/${google_artifact_registry_repository.this.repository_id}"
  description = "Full repository prefix working-tree images are pushed under."
}

output "harbor_ip" {
  value = google_compute_address.harbor.address
}

output "harbor_hostname" {
  value       = "harbor.${google_compute_address.harbor.address}.sslip.io"
  description = "Publicly resolvable hostname (sslip.io echoes the embedded IP) for Harbor's LoadBalancer — resolvable by containerd on the nodes AND by pods, with no cluster-DNS surgery (GKE runs kube-dns, so the kind harness's CoreDNS rewrite has no equivalent here)."
}
