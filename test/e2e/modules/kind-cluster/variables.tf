variable "name" {
  type        = string
  default     = "bridge-e2e"
  description = "kind cluster name"
}

variable "worker_count" {
  type        = number
  default     = 2
  description = "Number of worker nodes (control-plane is always 1)"
}

variable "http_port" {
  type        = number
  default     = 8080
  description = "Host port mapped to Traefik HTTP NodePort"
}

variable "https_port" {
  type        = number
  default     = 8443
  description = "Host port mapped to Traefik HTTPS NodePort"
}

variable "api_server_port" {
  type        = number
  default     = 6443
  description = "Host port for kube-apiserver"
}

variable "node_image" {
  type        = string
  default     = "kindest/node:v1.35.0"
  description = "kind node image (controls kubelet version)"
}

variable "enable_cert_manager" {
  type        = bool
  default     = true
  description = "Install cert-manager via Helm. Required for the bridge chart's TLS cert."
}

variable "registry_insecure_hostnames" {
  type        = list(string)
  default     = []
  description = <<-EOT
    Registry host:port values for which containerd should skip TLS
    verification (e.g. ["harbor.dev.127.0.0.1.nip.io:30843"]). The
    module writes a hosts.toml for each via containerd's certs.d.
    Self-signed Harbor served by Traefik on NodePort is the typical
    use case — see docs/E2E-MANUAL-SETUP.md §4.
  EOT
}

variable "extra_etc_hosts" {
  type        = list(string)
  default     = []
  description = <<-EOT
    Extra `/etc/hosts` lines written to every kind node after the
    cluster is up. Format: "IP HOSTNAME" per entry. Use to map
    synthetic hostnames (e.g. "127.0.0.1 harbor.e2e") so containerd
    can resolve them without a real DNS record — needed because
    go-containerregistry refuses loopback/private realm hosts in
    www-authenticate, forcing us off literal-IP image refs.
  EOT
}

variable "images_to_load" {
  type        = list(string)
  default     = []
  description = <<-EOT
    Local docker image refs (e.g. ["harbor-bridge:e2e",
    "harbor-bridge-plugin:e2e"]) to copy into the cluster via
    `kind load docker-image` after the cluster is up. CI builds
    images locally before tofu test and lists them here so the
    bridge install can use them without a registry.
  EOT
}
