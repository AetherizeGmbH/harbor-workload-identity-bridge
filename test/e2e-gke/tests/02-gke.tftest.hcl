# End-to-end test on a REAL GKE cluster (ADR-0022). Never runs in CI —
# it creates billed resources in the operator's project; run it via
# `make e2e-gke` with GOOGLE_PROJECT set.
#
# What this proves beyond the kind harness: GKE node images already run
# kubelet with --image-credential-provider-* flags for the out-of-tree
# GCP provider, so `plugin.install.mode: auto` must resolve to MERGE —
# our provider entry is injected into GKE's existing config without
# touching kubelet flags (ADR-0021).
#
# Stages:
#   1. build_images     — working-tree images, cross-built for linux/amd64
#   2. gke              — zonal spot cluster (>=1.34, DPv2) + AR repo + static IP
#   3. push             — docker push to Artifact Registry (no `kind load` on GKE)
#   4. cert_manager     — bridge chart needs cert-manager for its serving cert
#   5. harbor           — official chart, LoadBalancer on the pre-allocated IP,
#                         hostname harbor.<ip>.sslip.io (no cluster-DNS surgery —
#                         GKE runs kube-dns, so the kind CoreDNS trick has no
#                         equivalent; sslip.io resolves publicly for nodes AND pods)
#   6. containerd_trust — TEST-ONLY DaemonSet trusting Harbor's self-signed cert;
#                         pulling its (AR-hosted) image is also the pre-install
#                         smoke test that GKE's own credential provider works
#   7. seed             — projects + test images via crane (same as kind)
#   8. bridge_install   — our chart, install mode auto → expected merge
#   9. harbor_access    — baseline CR + upgrade CR (Ready waits)
#  10. pull_pod         — pull from Harbor via the bridge (the headline)
#  11. pull_pod_ar      — pull from Artifact Registry AFTER the merge: proves the
#                         installer preserved GKE's own provider entry (the
#                         coexistence guarantee, ADR-0021/0022)
#  12. bridge_upgrade   — widen matchImages; pull_pod_upgrade only succeeds if the
#                         installer restarted kubelet on the content change
#  13. file_sleep       — opt-in pause for kubectl inspection
#
# Runtime findings to fold back into ADR-0022 after the first real run:
# the discovered GKE provider config path/format (plugin DaemonSet logs
# show it), whether GKE containerd ships a config_path (trust DS logs),
# and whether the loopback NodePort routes under Dataplane V2 — if
# pull_pod fails with the plugin unable to reach the bridge, set
# bridge_endpoint = "https://$(NODE_IP):31443" on bridge_install (and
# see the R4 note in ADR-0022 about the cert SAN implications).

run "build_images" {
  command = apply
  module {
    source = "../e2e/modules/docker-build"
  }
  variables {
    images = {
      bridge = {
        tag        = "harbor-bridge:e2e"
        dockerfile = "Dockerfile.bridge"
        context    = "../.."
        # GKE nodes are amd64; the local builder may be arm64 (Apple
        # Silicon). The Dockerfiles cross-compile via $BUILDPLATFORM.
        platform = "linux/amd64"
      }
      plugin = {
        tag        = "harbor-bridge-plugin:e2e"
        dockerfile = "Dockerfile.plugin"
        context    = "../.."
        platform   = "linux/amd64"
      }
      seed = {
        tag        = "e2e-seed:e2e"
        dockerfile = "Dockerfile"
        context    = "../e2e/seed"
        platform   = "linux/amd64"
      }
    }
  }
}

run "gke" {
  command = apply
  module {
    source = "./modules/gke-cluster"
  }
  variables {
    project        = var.gcp_project
    zone           = var.gcp_zone
    version_prefix = var.gke_version_prefix
    machine_type   = var.machine_type
    node_count     = var.node_count
  }
}

run "push" {
  command = apply
  module {
    source = "./modules/ar-push"
  }
  variables {
    registry_host = run.gke.ar_registry_host
    target_repo   = run.gke.ar_repo
    # Constructed from build_images outputs so the dependency edge is
    # explicit and the pushed refs always match the just-built images.
    images = {
      bridge = "${run.build_images.image_refs.bridge.repository}:${run.build_images.image_refs.bridge.tag}"
      plugin = "${run.build_images.image_refs.plugin.repository}:${run.build_images.image_refs.plugin.tag}"
      seed   = "${run.build_images.image_refs.seed.repository}:${run.build_images.image_refs.seed.tag}"
    }
  }
}

run "cert_manager" {
  command = apply
  module {
    source = "./modules/cert-manager"
  }
  variables {
    kubeconfig = run.gke.kubeconfig
  }
}

run "harbor" {
  command = apply
  module {
    source = "../e2e/modules/harbor"
  }
  variables {
    kubeconfig        = run.gke.kubeconfig
    expose_type       = "loadBalancer"
    load_balancer_ip  = run.gke.harbor_ip
    external_hostname = run.gke.harbor_hostname
    version_harbor    = var.version_harbor
  }
}

run "containerd_trust" {
  command = apply
  module {
    source = "./modules/gke-containerd-trust"
  }
  variables {
    kubeconfig        = run.gke.kubeconfig
    registry_hostname = run.gke.harbor_hostname
    connect_addr      = "${run.gke.harbor_ip}:443"
    image             = run.push.image_tags.seed
  }
}

# Seed the scenario projects + images. Same flow as the kind harness's
# seed stage, minus the hostAliases/CoreDNS gymnastics — sslip.io
# resolves publicly, and pods reach the LoadBalancer IP directly.
run "seed_image" {
  command = apply
  module {
    source = "../e2e/modules/k8s-yaml"
  }
  variables {
    kubeconfig = run.gke.kubeconfig
    manifests = [
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: Namespace
          metadata: { name: e2e-seed }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: Secret
          metadata:
            name: harbor-admin
            namespace: e2e-seed
          data:
            username: ${base64encode("admin")}
            password: ${base64encode(run.harbor.admin_password)}
          type: Opaque
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: ConfigMap
          metadata:
            name: project-bootstrap
            namespace: e2e-seed
          data:
            run.sh: |
              #!/bin/sh
              set -e
              HOST='${run.gke.harbor_hostname}'
              # Trust Harbor's self-signed cert (off the wire — chart
              # secret naming is fragile across versions).
              openssl s_client -connect "$HOST:443" -servername "$HOST" \
                </dev/null 2>/dev/null \
                | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' \
                > /usr/local/share/ca-certificates/harbor-e2e.crt
              test -s /usr/local/share/ca-certificates/harbor-e2e.crt || {
                echo "failed to fetch Harbor cert via openssl s_client"
                exit 1
              }
              update-ca-certificates 2>/dev/null

              for proj in your-project upgrade-only; do
                curl -sv -u "admin:$(cat /admin/password)" -X POST \
                  -H "Content-Type: application/json" \
                  http://harbor-core.harbor.svc.cluster.local/api/v2.0/projects \
                  -d '{"project_name":"'"$proj"'","metadata":{"public":"false"}}' || true
              done

              crane auth login "$HOST" -u admin -p "$(cat /admin/password)"
              crane copy alpine:3.20 "$HOST/your-project/alpine:test3"
              crane copy "$HOST/your-project/alpine:test3" "$HOST/upgrade-only/app:v1"
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: batch/v1
          kind: Job
          metadata:
            name: seed-image
            namespace: e2e-seed
          spec:
            backoffLimit: 2
            template:
              spec:
                restartPolicy: Never
                containers:
                  - name: seed
                    image: ${run.push.image_tags.seed}
                    imagePullPolicy: IfNotPresent
                    command: [sh, /scripts/run.sh]
                    volumeMounts:
                      - name: scripts
                        mountPath: /scripts
                      - name: admin
                        mountPath: /admin
                        readOnly: true
                volumes:
                  - name: scripts
                    configMap:
                      name: project-bootstrap
                      defaultMode: 493
                  - name: admin
                    secret:
                      secretName: harbor-admin
        YAML
      },
    ]
  }
}

run "bridge_install" {
  command = apply
  module {
    source = "../e2e/modules/harbor-bridge-install"
  }
  variables {
    kubeconfig            = run.gke.kubeconfig
    cluster_name          = "gke-e2e"
    harbor_url            = run.harbor.internal_api_url
    harbor_admin_password = run.harbor.admin_password
    audience              = "harbor-bridge"
    # install_mode stays the default "auto" — on GKE this MUST resolve
    # to merge (the node image pre-wires the credential-provider flags
    # for GKE's own provider). If it resolved to patch, the installer
    # fails loudly and helm's DaemonSet-rollout wait surfaces it here.
    #
    # Path-scoped matchImages (literal prefixes; kubelet allows no path
    # globs), deliberately excluding upgrade-only — the bridge_upgrade
    # stage adds it (ADR-0021 convergence assertion).
    match_images = [
      "${run.gke.harbor_hostname}/your-project",
    ]
    bridge_image = run.push.image_refs.bridge
    plugin_image = run.push.image_refs.plugin
  }
}

run "harbor_access" {
  command = apply
  module {
    source = "../e2e/modules/k8s-yaml"
  }
  variables {
    kubeconfig = run.gke.kubeconfig
    manifests = [
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: Namespace
          metadata: { name: test-pull }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: ServiceAccount
          metadata:
            name: image-puller
            namespace: test-pull
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: harbor.aetherize.io/v1alpha1
          kind: HarborAccess
          metadata:
            name: test-access
            namespace: ${run.bridge_install.namespace}
          spec:
            serviceAccountRef:
              namespace: test-pull
              name: image-puller
            trustPolicy:
              issuer: https://kubernetes.default.svc.cluster.local
              audience: harbor-bridge
            permissions:
              - project: your-project
                action: pull
            tokenTTL: 1h0m0s
        YAML
        wait = {
          conditions = [
            { type = "Ready", status = "True" },
          ]
        }
      },

      # Upgrade-convergence prep: CR + robot exist from the start; the
      # only missing piece pre-upgrade is the kubelet-side matchImages
      # entry (see bridge_upgrade below).
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: Namespace
          metadata: { name: upgrade-ns }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: ServiceAccount
          metadata: { name: upgrade-runner, namespace: upgrade-ns }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: harbor.aetherize.io/v1alpha1
          kind: HarborAccess
          metadata:
            name: upgrade-access
            namespace: ${run.bridge_install.namespace}
          spec:
            serviceAccountRef:
              namespace: upgrade-ns
              name: upgrade-runner
            trustPolicy:
              issuer: https://kubernetes.default.svc.cluster.local
              audience: harbor-bridge
            permissions:
              - project: upgrade-only
                action: pull
            tokenTTL: 1h0m0s
        YAML
        wait = {
          conditions = [
            { type = "Ready", status = "True" },
          ]
        }
      },
    ]
  }
}

# The headline: kubelet execs our plugin (merged into GKE's provider
# config), the plugin reaches the bridge, the bridge mints robot creds,
# containerd pulls from Harbor.
run "pull_pod" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "bridge-pull-test"
    namespace            = "test-pull"
    service_account_name = "image-puller"
    image                = "${run.gke.harbor_hostname}/your-project/alpine:test3"
    command              = ["sh", "-c"]
    args                 = ["echo bridge-pull-test pulled successfully on GKE; exit 0"]
    timeout_seconds      = 300
    fail_message         = "GKE bridge pull failed. Check: plugin DaemonSet logs (merge target/paths), bridge logs, and whether the loopback NodePort routes under Dataplane V2 (R4 — escape hatch: bridge_endpoint variable)"
  }
}

# Coexistence assertion (the merge-mode guarantee): AFTER our provider
# entry was merged into GKE's credential-provider config, GKE's own
# provider must still authenticate Artifact Registry pulls. A mangled
# merge (dropped entry / broken file) makes this pull fail. Fresh
# namespace + never-pulled tag so kubelet cannot serve it from cache.
run "pull_pod_ar" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "ar-coexistence-test"
    namespace            = "default"
    service_account_name = "default"
    image                = run.push.image_tags.seed
    command              = ["sh", "-c"]
    args                 = ["echo artifact-registry pull still works after the merge; exit 0"]
    timeout_seconds      = 300
    fail_message         = "COEXISTENCE BROKEN: Artifact Registry pull failed after the installer merged our provider entry — GKE's own credential-provider entry was likely damaged (ADR-0021 merge bug)"
  }
}

# ADR-0021 upgrade convergence, same pattern as the kind harness:
# widen matchImages → ConfigMap changes → DaemonSet re-rolls → the
# installer re-merges and restarts kubelet on the content change.
run "bridge_upgrade" {
  command = apply
  module {
    source = "../e2e/modules/harbor-bridge-install"
  }
  variables {
    kubeconfig            = run.gke.kubeconfig
    cluster_name          = "gke-e2e"
    harbor_url            = run.harbor.internal_api_url
    harbor_admin_password = run.harbor.admin_password
    audience              = "harbor-bridge"
    match_images = [
      "${run.gke.harbor_hostname}/your-project",
      "${run.gke.harbor_hostname}/upgrade-only",
    ]
    bridge_image = run.push.image_refs.bridge
    plugin_image = run.push.image_refs.plugin
  }
}

run "pull_pod_upgrade" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "pull-upgrade-only"
    namespace            = "upgrade-ns"
    service_account_name = "upgrade-runner"
    image                = "${run.gke.harbor_hostname}/upgrade-only/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo upgrade-only pulled after helm upgrade on GKE; exit 0"]
    timeout_seconds      = 300
    fail_message         = "ADR-0021 regression on GKE: matchImages change did not reach the running kubelet — the installer failed to re-merge and restart after the helm upgrade"
  }
}

run "file_sleep" {
  command = apply
  module {
    source = "../e2e/modules/test-sleep"
  }
  variables {
    enabled = var.pause_after_pull
  }
}
