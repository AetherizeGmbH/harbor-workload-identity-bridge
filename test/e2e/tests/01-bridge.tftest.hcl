# End-to-end test of the Harbor Workload Identity Bridge.
#
# Stages (each `run` block is a separate plan+apply, sharing state):
#   1. build_images         — `docker build` bridge + plugin from local sources
#   2. cluster              — kind cluster (cilium + cert-manager); loads images from (1)
#   3. harbor               — Harbor via official helm chart, exposed via NodePort 30843
#   4. seed_image           — push alpine:3.20 → harbor.your-project/alpine:test3
#   5. bridge_install       — our chart (creates ClusterIssuer, CRD, Deployment, DaemonSet)
#   6. harbor_access        — apply a HarborAccess CR for the test SA
#   7. file_sleep (opt-in)  — pause for kubectl-poke; off in CI, on via TF_VAR_pause_before_pull=true
#   8. pull_pod             — Job pulling the image; success = end-to-end works
#
# Image refs use `harbor.e2e:30843` so that:
#   - go-containerregistry (crane in seed_image) accepts the realm host
#     in Harbor's www-authenticate response — it hard-rejects loopback /
#     RFC1918-private hosts.
#   - Containerd on kind nodes routes via /etc/containerd/certs.d/
#     harbor.e2e:30843/hosts.toml to https://127.0.0.1:30843 (NodePort)
#     with skip_verify for the self-signed cert.
#   - Kind nodes also have `127.0.0.1 harbor.e2e` in /etc/hosts so any
#     auth-realm follow on a 401 resolves locally.
#   - The seed pod inside the cluster resolves harbor.e2e via a
#     hostAlias mapping it to a kind node's docker-network IP
#     (run.harbor.kind_node_ip), and trusts Harbor's self-signed cert
#     via a CA bundle replicated from the harbor namespace.
#
# The last stage is the load-bearing assertion. If the kubelet
# silent-abort bug from docs/PHASES.md still bites, the Job will fail
# with ImagePullBackOff and the test fails.

# Build bridge + plugin images directly from the repo's Dockerfiles.
# Paths are relative to test/e2e (the CWD when `tofu test` is invoked),
# so ../.. is the repo root. Each rebuild is gated by a Dockerfile
# content hash inside the module; docker BuildKit caching handles
# unchanged source trees, so re-runs are fast.
run "build_images" {
  command = apply
  module {
    source = "./modules/docker-build"
  }
  variables {
    images = {
      bridge = {
        tag        = "harbor-bridge:e2e"
        dockerfile = "../../Dockerfile.bridge"
        context    = "../.."
      }
      plugin = {
        tag        = "harbor-bridge-plugin:e2e"
        dockerfile = "../../Dockerfile.plugin"
        context    = "../.."
      }
      # alpine + curl + jq + crane — used by the seed_image Job.
      # `gcr.io/go-containerregistry/crane` is distroless (no shell, no
      # curl) so we COPY the crane binary into a tiny alpine base.
      seed = {
        tag        = "e2e-seed:e2e"
        dockerfile = "seed/Dockerfile"
        context    = "seed"
      }
    }
  }
}

run "cluster" {
  module {
    source = "./modules/kind-cluster"
  }
  variables {
    name                        = "bridge-e2e"
    worker_count                = 2
    http_port                   = 8080
    https_port                  = 8443
    api_server_port             = 6443
    enable_cert_manager         = true
    registry_insecure_hostnames = ["harbor.e2e:30843"]
    extra_etc_hosts             = ["127.0.0.1 harbor.e2e"]
    # `kind load docker-image` for each tag, after the cluster is up.
    # Reference into run.build_images establishes the dependency edge
    # (build_images runs first) and means we never drift on image tag.
    images_to_load = run.build_images.image_tags_list
  }
}

run "harbor" {
  module {
    source = "./modules/harbor"
  }
  variables {
    kubeconfig        = run.cluster.kubeconfig
    external_hostname = "harbor.e2e"
    https_node_port   = 30843
    http_node_port    = 30880
  }
}

# Pull Harbor's self-signed leaf cert and install it into each kind
# node's containerd certs.d/<host:port>/ca.crt + write hosts.toml
# pointing at it. We don't rely on `skip_verify = true` (containerd
# v2.x has been observed to silently ignore that even when hosts.toml
# is otherwise loaded). Must run after harbor (cert exists) and
# before any pod whose image kubelet would pull from harbor.e2e.
run "containerd_trust" {
  command = apply
  module {
    source = "./modules/containerd-registry-trust"
  }
  variables {
    cluster_name           = run.cluster.name
    node_names             = run.cluster.node_names
    registry_host_port     = "harbor.e2e:30843"
    extract_from_node_port = "127.0.0.1:30843"
  }
}

# Make harbor.e2e resolvable for every pod in the cluster (not just
# seed_image's pod). CoreDNS gets a hosts-plugin entry mapping
# harbor.e2e to the kind node IP — same as what we put in kind nodes'
# /etc/hosts, but at the cluster DNS layer so debug pods can also
# `curl harbor.e2e:30843` without bespoke hostAliases.
run "coredns_rewrite" {
  command = apply
  module {
    source = "./modules/coredns-cm"
  }
  variables {
    kubeconfig = run.cluster.kubeconfig
    dns_hosts_entries = {
      "harbor.e2e" = run.harbor.kind_node_ip
    }
  }
}

# Seed the test image into Harbor. The Job runs crane inside the
# cluster, talking to harbor-core via service DNS over HTTP:80.
run "seed_image" {
  command = apply
  module {
    source = "./modules/k8s-yaml"
  }
  variables {
    kubeconfig = run.cluster.kubeconfig
    manifests = [
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: Namespace
          metadata: { name: e2e-seed }
        YAML
      },
      {
        # `data` (base64) rather than `stringData` (plaintext): the
        # hashicorp/kubernetes_manifest provider can't re-read
        # stringData — the API converts it to data on write, so the
        # post-apply read returns null and the provider errors with
        # "produced an unexpected new value: .object.stringData ... now null".
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
              # Grab Harbor's self-signed leaf cert off the wire and
              # add to the system trust store. Chart secret naming for
              # the cert is fragile across versions; reading from the
              # actual TLS endpoint is always right.
              # `update-ca-certificates` then folds it into the bundle
              # both crane (alpine pull from docker.io + harbor push)
              # and any other Go HTTP client read by default — keeps
              # docker.io's public-CA verification intact too.
              openssl s_client -connect harbor.e2e:30843 -servername harbor.e2e \
                </dev/null 2>/dev/null \
                | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' \
                > /usr/local/share/ca-certificates/harbor-e2e.crt
              test -s /usr/local/share/ca-certificates/harbor-e2e.crt || {
                echo "failed to fetch Harbor cert via openssl s_client"
                exit 1
              }
              update-ca-certificates 2>/dev/null

              # Create the project. In-cluster service URL — no DNS
              # gymnastics needed for plain REST.
              curl -sv -u "admin:$(cat /admin/password)" -X POST \
                -H "Content-Type: application/json" \
                http://harbor-core.harbor.svc.cluster.local/api/v2.0/projects \
                -d '{"project_name":"your-project","metadata":{"public":"false"}}' || true

              # Push via the external hostname so the realm crane sees
              # in www-authenticate matches what we connect to. The
              # pod hostAlias (set on the Job spec below) maps
              # harbor.e2e to a kind node's docker-network IP; the cert
              # just installed makes Go's default TLS verify pass.
              crane auth login harbor.e2e:30843 \
                -u admin -p "$(cat /admin/password)"
              crane copy alpine:3.20 \
                harbor.e2e:30843/your-project/alpine:test3
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
                # No hostAliases needed — the coredns_rewrite run made
                # harbor.e2e resolvable cluster-wide via CoreDNS's
                # hosts plugin, so this pod (and any ad-hoc debug pod)
                # gets the same answer via DNS.
                containers:
                  - name: seed
                    image: e2e-seed:e2e
                    # IfNotPresent so kubelet uses the kind-loaded copy
                    # instead of trying to pull `e2e-seed:e2e` from a
                    # registry (it only exists locally on the kind nodes).
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
                      # 493 decimal = 0o755 octal. yamldecode in HCL
                      # parses `0755` as decimal 755, which the K8s
                      # API rejects ("must be between 0 and 0777
                      # octal"). Stick to the explicit decimal value.
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
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    kubeconfig            = run.cluster.kubeconfig
    cluster_name          = "dev"
    harbor_url            = run.harbor.internal_api_url
    harbor_admin_password = run.harbor.admin_password
    audience              = "harbor-bridge"
    # No path glob — kubelet's matchImages doesn't support globs in
    # the path component (only in the domain), so `harbor.e2e:30843/*`
    # was being interpreted as a literal `/*` path prefix and silently
    # failed to match `harbor.e2e:30843/your-project/alpine:test3`.
    # The bare host:port form matches any image from that registry.
    match_images = ["harbor.e2e:30843"]
  }
}

run "harbor_access" {
  command = apply
  module {
    source = "./modules/k8s-yaml"
  }
  variables {
    kubeconfig = run.cluster.kubeconfig
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
        # Block on the bridge controller's umbrella condition (Ready=True),
        # which only flips true once both RobotProvisioned=True and
        # TrustPolicyApplied=True. If Harbor's robot creation fails or
        # the trust policy is malformed, this wait surfaces it
        # synchronously instead of letting pull_pod race past with
        # a not-yet-reconciled CR.
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
            # Use the canonical Go time.Duration form upfront. The
            # CRD's tokenTTL is unmarshaled via metav1.Duration, which
            # re-serialises "1h" → "1h0m0s". hashicorp/kubernetes_manifest
            # then trips its "inconsistent result after apply" guard
            # comparing the two. Sending the post-normalisation form
            # bypasses the round-trip drift entirely.
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

# Pause-for-inspection between cluster setup and the load-bearing
# pull_pod assertion. Off by default (CI never pauses); flip on for
# local dev with `TF_VAR_pause_before_pull=true tofu test`. While the
# file exists you can kubectl-poke the cluster; `rm` it to continue.
run "file_sleep" {
  command = apply
  module {
    source = "./modules/test-sleep"
  }
  variables {
    enabled = var.pause_before_pull
  }
}


# Load-bearing assertion. Image ref uses harbor.e2e:30843 — containerd
# resolves via the kind node's /etc/hosts entry to 127.0.0.1 and the
# hosts.toml directive routes to https://127.0.0.1:30843 (NodePort)
# with skip_verify for Harbor's self-signed cert.
# Success = kubelet fork+execs the plugin AND the plugin returns valid
# Harbor robot credentials AND containerd accepts them.
run "pull_pod" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "bridge-pull-test"
    namespace            = "test-pull"
    service_account_name = "image-puller"
    image                = "harbor.e2e:30843/your-project/alpine:test3"
    command              = ["sh", "-c"]
    args                 = ["echo bridge-pull-test pulled successfully; exit 0"]
    timeout_seconds      = 300
    fail_message         = "kubelet silent-abort bug: see docs/PHASES.md §kubelet-exec-blocker"
  }
}
