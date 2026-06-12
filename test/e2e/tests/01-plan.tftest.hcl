# Plan-only matrix for the harbor-bridge-install module. No real cluster or
# Harbor: all three providers the module declares (helm, kubernetes, kubectl —
# see modules/harbor-bridge-install/main.tf) are mocked, so the plan graph is
# exercised with fabricated provider values. These runs assert on what the
# module DERIVES at plan time — chiefly the yamlencode()'d helm values, the
# namespace, and the ClusterIssuer manifest — across many input combinations.
# This catches variable-plumbing, default-expansion, and validation regressions
# fast, without standing up the kind+Harbor stack that 01-bridge.tftest.hcl does.
#
# Why plan-only assertions work: helm_release.bridge.values[0] is yamlencode()
# of a config-only object, so it is fully known at plan even with a mocked
# provider. We yamldecode() it back and assert on the structured result.

# Un-aliased on purpose: the module uses the DEFAULT provider configuration for
# each of these (no `provider = kubernetes.mock` anywhere), so the mock must
# replace the default. An `alias` here would leave the real providers active and
# the plan would try to dial var.kubeconfig.
mock_provider "kubernetes" {}

mock_provider "kubectl" {}

mock_provider "helm" {}

# Shared baseline for every run. Each run below overrides only the variables it
# is exercising. kubeconfig is required input even with mocked providers (the
# module's provider blocks reference it); the values are structurally-valid
# dummies that never connect anywhere.
variables {
  kubeconfig = {
    host                   = "https://127.0.0.1:6443"
    client_certificate     = "mock-client-cert"
    client_key             = "mock-client-key"
    cluster_ca_certificate = "mock-ca-cert"
  }
  cluster_name          = "dev"
  harbor_url            = "http://harbor-core.harbor.svc.cluster.local"
  harbor_admin_password = "mock-admin-password"
  audience              = "harbor-bridge"
  match_images          = ["harbor.e2e:30843"]
  bridge_image = {
    repository = "harbor-bridge"
    tag        = "e2e"
  }
  plugin_image = {
    repository = "harbor-bridge-plugin"
    tag        = "e2e"
  }
}

# ── Defaults ─────────────────────────────────────────────────────────────────
# Nothing overridden beyond the required baseline. Pins every value the module
# hardcodes or defaults: namespace, issuer, the chart's static knobs (replicas,
# logLevel, patchKubelet, tls.enabled), and the bridge_resources default
# expansion (optional() Burstable defaults: limits.memory ≫ requests.memory).
run "defaults" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }

  assert {
    condition     = output.namespace == "harbor-bridge-system"
    error_message = "default namespace should be harbor-bridge-system"
  }
  assert {
    condition     = strcontains(kubectl_manifest.cluster_issuer.yaml_body, "name: harbor-bridge-ca")
    error_message = "default ClusterIssuer name should be harbor-bridge-ca"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).clusterName == "dev"
    error_message = "clusterName should be wired from var.cluster_name"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).harbor.url == "http://harbor-core.harbor.svc.cluster.local"
    error_message = "harbor.url should be wired from var.harbor_url"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).harbor.adminCredsSecret.name == "harbor-admin"
    error_message = "adminCredsSecret.name should reference the harbor-admin secret the module creates"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).tls.enabled == true
    error_message = "tls.enabled is hardcoded true by the module"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).tls.issuerRef.kind == "ClusterIssuer"
    error_message = "tls.issuerRef.kind should be ClusterIssuer"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).tls.issuerRef.name == "harbor-bridge-ca"
    error_message = "tls.issuerRef.name should track the default issuer_name"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).plugin.patchKubelet == true
    error_message = "plugin.patchKubelet is hardcoded true by the module"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.replicas == 1
    error_message = "bridge.replicas is hardcoded 1 by the module"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.logLevel == "debug"
    error_message = "bridge.logLevel is hardcoded debug by the module"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.resources.requests.cpu == "50m"
    error_message = "default requests.cpu should be 50m"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.resources.requests.memory == "64Mi"
    error_message = "default requests.memory should be 64Mi"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.resources.limits.cpu == "500m"
    error_message = "default limits.cpu should be 500m"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.resources.limits.memory == "256Mi"
    error_message = "default limits.memory should be 256Mi"
  }
}

# ── Custom namespace ─────────────────────────────────────────────────────────
run "custom_namespace" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    namespace = "bridge-prod"
  }

  assert {
    condition     = output.namespace == "bridge-prod"
    error_message = "namespace override should flow to the created namespace + output"
  }
}

# ── Custom issuer name ───────────────────────────────────────────────────────
# issuer_name feeds BOTH the ClusterIssuer manifest AND the chart's
# tls.issuerRef.name — assert they stay in lockstep so the serving cert
# references an issuer that actually exists.
run "custom_issuer" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    issuer_name = "prod-bridge-ca"
  }

  assert {
    condition     = strcontains(kubectl_manifest.cluster_issuer.yaml_body, "name: prod-bridge-ca")
    error_message = "ClusterIssuer manifest should carry the overridden issuer_name"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).tls.issuerRef.name == "prod-bridge-ca"
    error_message = "tls.issuerRef.name must match the ClusterIssuer name"
  }
}

# ── Custom cluster name (robot-name prefix) ──────────────────────────────────
run "custom_cluster_name" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    cluster_name = "eu-west-1"
  }

  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).clusterName == "eu-west-1"
    error_message = "clusterName should reflect the override"
  }
}

# ── Custom audience ──────────────────────────────────────────────────────────
run "custom_audience" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    audience = "harbor.prod.aetherize"
  }

  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).plugin.audience == "harbor.prod.aetherize"
    error_message = "plugin.audience should reflect the override"
  }
}

# ── Single match_images entry ────────────────────────────────────────────────
run "single_match_image" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    match_images = ["registry.internal:5000"]
  }

  assert {
    condition     = length(yamldecode(helm_release.bridge.values[0]).plugin.matchImages) == 1
    error_message = "single-entry match_images should yield a one-element list"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).plugin.matchImages[0] == "registry.internal:5000"
    error_message = "plugin.matchImages should pass through the single registry verbatim"
  }
}

# ── Multiple match_images entries ────────────────────────────────────────────
run "multiple_match_images" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    match_images = ["harbor.e2e:30843", "registry.internal:5000", "ghcr.io"]
  }

  assert {
    condition     = length(yamldecode(helm_release.bridge.values[0]).plugin.matchImages) == 3
    error_message = "plugin.matchImages should carry all three registries"
  }
  assert {
    condition     = contains(yamldecode(helm_release.bridge.values[0]).plugin.matchImages, "ghcr.io")
    error_message = "plugin.matchImages should include each provided registry"
  }
}

# ── Custom images ────────────────────────────────────────────────────────────
# Bridge and plugin images are independent objects; assert they don't cross-wire.
run "custom_images" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    bridge_image = {
      repository = "ghcr.io/aetherizegmbh/harbor-workload-identity-bridge"
      tag        = "v1.2.3"
    }
    plugin_image = {
      repository = "ghcr.io/aetherizegmbh/harbor-bridge-plugin"
      tag        = "v4.5.6"
    }
  }

  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.image.repository == "ghcr.io/aetherizegmbh/harbor-workload-identity-bridge"
    error_message = "bridge.image.repository should reflect the override"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.image.tag == "v1.2.3"
    error_message = "bridge.image.tag should reflect the override"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).plugin.image.repository == "ghcr.io/aetherizegmbh/harbor-bridge-plugin"
    error_message = "plugin.image.repository should reflect the override"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).plugin.image.tag == "v4.5.6"
    error_message = "plugin.image.tag should reflect the override and not the bridge tag"
  }
}

# ── Resources: fully custom, Gi memory ───────────────────────────────────────
run "resources_full_gi" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    bridge_resources = {
      requests = {
        cpu    = "250m"
        memory = "256Mi"
      }
      limits = {
        cpu    = "2"
        memory = "1Gi"
      }
    }
  }

  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.resources.requests.cpu == "250m"
    error_message = "requests.cpu override should pass through"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.resources.limits.memory == "1Gi"
    error_message = "Gi memory should be accepted and passed through"
  }
}

# ── Resources: partial override exercises optional() default-fill ─────────────
# Only requests.memory is set; every other resource field must fall back to its
# optional() default. This is the case most likely to regress if the object
# defaults are restructured.
run "resources_partial_defaults" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    bridge_resources = {
      requests = {
        memory = "128Mi"
      }
    }
  }

  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.resources.requests.memory == "128Mi"
    error_message = "the one provided field should win"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.resources.requests.cpu == "50m"
    error_message = "unset requests.cpu should fall back to the optional() default"
  }
  assert {
    condition     = yamldecode(helm_release.bridge.values[0]).bridge.resources.limits.memory == "256Mi"
    error_message = "unset limits block should fall back to the optional() default"
  }
}

# ── Validation: requests.memory in a rejected format ─────────────────────────
# The module validates requests/limits memory against ^[0-9]+(Mi|Gi)$.
# "512MB" (SI, not binary) must be rejected at plan time.
run "invalid_requests_memory" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    bridge_resources = {
      requests = {
        memory = "512MB"
      }
    }
  }

  expect_failures = [
    var.bridge_resources,
  ]
}

# ── Validation: limits.memory in a rejected format ───────────────────────────
# A plain integer ("1000000000") has no Mi/Gi suffix and must be rejected.
run "invalid_limits_memory" {
  command = plan
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    bridge_resources = {
      limits = {
        memory = "1000000000"
      }
    }
  }

  expect_failures = [
    var.bridge_resources,
  ]
}
