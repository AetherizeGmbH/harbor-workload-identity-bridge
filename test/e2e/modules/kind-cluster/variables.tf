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
  description = "Laptop host port that kind binds, forwarding to the HTTP NodePort inside the kind container (default 30880, currently Harbor)."
}

variable "https_port" {
  type        = number
  default     = 8443
  description = "Laptop host port that kind binds, forwarding to the HTTPS NodePort inside the kind container (default 30843, currently Harbor)."
}

variable "api_server_port" {
  type        = number
  default     = 6443
  description = "Host port for kube-apiserver"
}

variable "node_image" {
  type = string
  # renovate: datasource=docker depName=kindest/node
  default     = "kindest/node:v1.36.1"
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
    Registry host:port values for which the module writes a
    skip_verify=true hosts.toml under containerd's certs.d. NOTE:
    the e2e harness now installs a real ca.crt via
    containerd-registry-trust rather than relying on skip_verify
    (containerd v2.2.0 has been seen to silently ignore it). This
    var stays for callers who deliberately want skip_verify.
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
