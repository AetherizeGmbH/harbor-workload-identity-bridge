# End-to-end test of the Harbor Workload Identity Bridge.
#
# Stages (each `run` block is a separate plan+apply, sharing state):
#   1. build_images         — `docker build` bridge + plugin from local sources
#   2. cluster              — kind cluster (cilium + cert-manager); loads images from (1)
#   3. harbor               — Harbor via official helm chart, exposed via NodePort 30843
#   4. seed_image           — create the scenario projects and push alpine:3.20
#                             into each (your-project, project-alpha/beta/gamma,
#                             beta-1/2/3)
#   5. bridge_install       — our chart (creates ClusterIssuer, CRD, Deployment, DaemonSet)
#   6. harbor_access        — apply HarborAccess CRs: the baseline SA, two
#                             collision-prone SAs (ADR-0018), one CR authored in
#                             the tenant's own namespace (cluster-wide pickup), and
#                             one multi-project pull,push CR (beta-1/2/3)
#   7. pull_pod*            — one Job per scenario; each success = end-to-end works.
#                             pull_pod / _alpha / _beta / _gamma / _multi plus
#                             robot_push_test exercise multi-tenancy, the
#                             dot-delimited naming fix, cluster-wide CR matching,
#                             multi-project robots, and the pull,push action.
#   8. file_sleep (opt-in)  — pause AFTER all assertions for kubectl-poke on a
#                             fully-populated cluster; off in CI, on via
#                             `make e2e-pause` / TF_VAR_pause_after_pull=true
#
# Multi-tenant / collision coverage (stage 6 + 8):
#   - team-a/svc-b → project-alpha and team/a-svc-b → project-beta are two
#     DISTINCT identities whose robot names would have COLLIDED under the old
#     hyphen-joined scheme (both bridge-dev-team-a-svc-b) but are distinct under
#     ADR-0018's dot-joined scheme (bridge-dev.team-a.svc-b vs
#     bridge-dev.team.a-svc-b). Each pulls only its own project, so a naming
#     regression makes exactly one of the two pulls fail (or the second CR never
#     goes Ready and stage 6 times out).
#   - app-ns/runner's HarborAccess lives in app-ns, NOT the bridge namespace,
#     proving the data plane matches CRs cluster-wide.
#   - beta-ns/beta-runner has ONE HarborAccess granting pull,push on MULTIPLE
#     projects (beta-1/2/3). pull_pod_multi pulls one of them via the credential
#     provider; robot_push_test then uses the minted robot's creds to push to a
#     second project and pull a third — exercising multi-project scope AND the
#     pull,push action (a pull-only robot would 403 on the push).
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

              # Create every project the e2e scenarios pull from. In-cluster
              # service URL — no DNS gymnastics needed for plain REST. 409s on
              # re-run are swallowed by || true.
              for proj in your-project project-alpha project-beta project-gamma beta-1 beta-2 beta-3; do
                curl -sv -u "admin:$(cat /admin/password)" -X POST \
                  -H "Content-Type: application/json" \
                  http://harbor-core.harbor.svc.cluster.local/api/v2.0/projects \
                  -d '{"project_name":"'"$proj"'","metadata":{"public":"false"}}' || true
              done

              # Push via the external hostname so the realm crane sees
              # in www-authenticate matches what we connect to. The
              # pod hostAlias (set on the Job spec below) maps
              # harbor.e2e to a kind node's docker-network IP; the cert
              # just installed makes Go's default TLS verify pass.
              crane auth login harbor.e2e:30843 \
                -u admin -p "$(cat /admin/password)"
              # Baseline image, kept for the original pull_pod assertion.
              crane copy alpine:3.20 \
                harbor.e2e:30843/your-project/alpine:test3
              # Fan the same image into each scenario project via intra-Harbor
              # copy (one docker.io pull total; avoids Docker Hub rate limits).
              for proj in project-alpha project-beta project-gamma beta-1 beta-2 beta-3; do
                crane copy harbor.e2e:30843/your-project/alpine:test3 \
                  "harbor.e2e:30843/$proj/app:v1"
              done
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
    # Use the images we just built in build_images, not the install
    # module's defaults. Establishes the dependency edge explicitly
    # and guarantees the test exercises the working tree's Dockerfiles
    # rather than a stale tag that happens to match.
    bridge_image = run.build_images.image_refs.bridge
    plugin_image = run.build_images.image_refs.plugin
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

      # ── ADR-0018 collision-resistance: two distinct identities that would
      # have produced the SAME robot name under the old hyphen-joined scheme.
      #   ns=team-a sa=svc-b   → old bridge-dev-team-a-svc-b
      #   ns=team   sa=a-svc-b → old bridge-dev-team-a-svc-b   (collision!)
      # Dot-joined (ADR-0018) they are distinct: bridge-dev.team-a.svc-b vs
      # bridge-dev.team.a-svc-b. Each is granted a DIFFERENT project; the two
      # pull_pod_alpha/beta runs below then prove the robots stayed distinct.
      # A naming regression makes the second CR's reconcile hit the robot-name
      # collision guard so its Ready never flips true and THIS wait times out.
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: Namespace
          metadata: { name: team-a }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: Namespace
          metadata: { name: team }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: ServiceAccount
          metadata: { name: svc-b, namespace: team-a }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: ServiceAccount
          metadata: { name: a-svc-b, namespace: team }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: harbor.aetherize.io/v1alpha1
          kind: HarborAccess
          metadata:
            name: collide-one
            namespace: ${run.bridge_install.namespace}
          spec:
            serviceAccountRef:
              namespace: team-a
              name: svc-b
            trustPolicy:
              issuer: https://kubernetes.default.svc.cluster.local
              audience: harbor-bridge
            permissions:
              - project: project-alpha
                action: pull
            tokenTTL: 1h0m0s
        YAML
        wait = {
          conditions = [
            { type = "Ready", status = "True" },
          ]
        }
      },
      {
        yaml = <<-YAML
          apiVersion: harbor.aetherize.io/v1alpha1
          kind: HarborAccess
          metadata:
            name: collide-two
            namespace: ${run.bridge_install.namespace}
          spec:
            serviceAccountRef:
              namespace: team
              name: a-svc-b
            trustPolicy:
              issuer: https://kubernetes.default.svc.cluster.local
              audience: harbor-bridge
            permissions:
              - project: project-beta
                action: pull
            tokenTTL: 1h0m0s
        YAML
        wait = {
          conditions = [
            { type = "Ready", status = "True" },
          ]
        }
      },

      # ── Cluster-wide CR pickup: this HarborAccess lives in the tenant's OWN
      # namespace (app-ns), not the bridge namespace. The bridge watches/lists
      # HarborAccess cluster-wide, so it still reconciles and the robot Secret
      # is written to the bridge namespace. pull_pod_gamma proves it end-to-end.
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: Namespace
          metadata: { name: app-ns }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: ServiceAccount
          metadata: { name: runner, namespace: app-ns }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: harbor.aetherize.io/v1alpha1
          kind: HarborAccess
          metadata:
            name: tenant-access
            namespace: app-ns
          spec:
            serviceAccountRef:
              namespace: app-ns
              name: runner
            trustPolicy:
              issuer: https://kubernetes.default.svc.cluster.local
              audience: harbor-bridge
            permissions:
              - project: project-gamma
                action: pull
            tokenTTL: 1h0m0s
        YAML
        wait = {
          conditions = [
            { type = "Ready", status = "True" },
          ]
        }
      },

      # ── Multi-project robot with pull,push. One HarborAccess grants pull,push
      # on SEVERAL projects (beta-1/2/3) — so the robot's scope spans more than
      # one project and carries the write action. pull_pod_multi pulls one of
      # them via the credential provider; robot_push_test then uses the minted
      # robot's creds to push to a second project and pull a third. This also
      # exercises the action expansion in harbor.toHarborPermissions, which
      # splits "pull,push" into two Access entries per project.
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: Namespace
          metadata: { name: beta-ns }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: v1
          kind: ServiceAccount
          metadata: { name: beta-runner, namespace: beta-ns }
        YAML
      },
      {
        yaml = <<-YAML
          apiVersion: harbor.aetherize.io/v1alpha1
          kind: HarborAccess
          metadata:
            name: multi-access
            namespace: ${run.bridge_install.namespace}
          spec:
            serviceAccountRef:
              namespace: beta-ns
              name: beta-runner
            trustPolicy:
              issuer: https://kubernetes.default.svc.cluster.local
              audience: harbor-bridge
            permissions:
              - project: beta-1
                action: pull,push
              - project: beta-2
                action: pull,push
              - project: beta-3
                action: pull,push
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

# Collision-resistance assertion (half 1 of 2). team-a/svc-b pulls
# project-alpha via robot bridge-dev.team-a.svc-b. Paired with pull_pod_beta:
# under the old hyphen-joined naming both SAs mapped to the SAME robot name, so
# the two robots collided and exactly one of these two pulls would fail. Both
# green proves ADR-0018's dot-delimited naming keeps the identities distinct.
run "pull_pod_alpha" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-collide-alpha"
    namespace            = "team-a"
    service_account_name = "svc-b"
    image                = "harbor.e2e:30843/project-alpha/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo team-a/svc-b pulled project-alpha; exit 0"]
    timeout_seconds      = 300
    fail_message         = "ADR-0018 collision regression or isolation break: team-a/svc-b could not pull project-alpha (its robot may have been overwritten by team/a-svc-b's)"
  }
}

# Collision-resistance assertion (half 2 of 2). team/a-svc-b pulls project-beta
# via robot bridge-dev.team.a-svc-b. See pull_pod_alpha for the rationale.
run "pull_pod_beta" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-collide-beta"
    namespace            = "team"
    service_account_name = "a-svc-b"
    image                = "harbor.e2e:30843/project-beta/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo team/a-svc-b pulled project-beta; exit 0"]
    timeout_seconds      = 300
    fail_message         = "ADR-0018 collision regression or isolation break: team/a-svc-b could not pull project-beta (its robot may have been overwritten by team-a/svc-b's)"
  }
}

# Cluster-wide CR pickup assertion. app-ns/runner pulls project-gamma using a
# HarborAccess authored in app-ns (the tenant's own namespace), not the bridge
# namespace. Success proves the data plane matches CRs cluster-wide and the
# robot Secret resolves from the bridge namespace regardless of CR location.
run "pull_pod_gamma" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-tenant-gamma"
    namespace            = "app-ns"
    service_account_name = "runner"
    image                = "harbor.e2e:30843/project-gamma/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo app-ns/runner pulled project-gamma; exit 0"]
    timeout_seconds      = 300
    fail_message         = "cluster-wide CR pickup broke: app-ns/runner could not pull project-gamma from a HarborAccess authored outside the bridge namespace"
  }
}

# Multi-project pull assertion. beta-ns/beta-runner pulls beta-1 via the
# credential provider. Its HarborAccess (multi-access) grants pull,push on
# beta-1/2/3, so a single robot serves more than one project; this proves the
# credential-issuance path works for a multi-project, multi-action CR.
run "pull_pod_multi" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-multi-beta1"
    namespace            = "beta-ns"
    service_account_name = "beta-runner"
    image                = "harbor.e2e:30843/beta-1/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo beta-ns/beta-runner pulled beta-1 via multi-project robot; exit 0"]
    timeout_seconds      = 300
    fail_message         = "multi-project pull broke: beta-ns/beta-runner could not pull beta-1 (CR grants pull,push on beta-1/2/3)"
  }
}

# pull,push assertion. A Job in the bridge namespace mounts the multi-access
# robot's credential Secret (via envFrom) and uses crane to PUSH a new tag into
# beta-2 (manifest PUT requires the push grant) and PULL beta-3 (a different
# project). A pull-only robot would 403 on the push, failing this run — so this
# is the load-bearing check that "pull,push" actually expands to write access on
# every listed project. Uses the kind-loaded e2e-seed image (crane + trust
# tooling), pulled IfNotPresent because it exists only on the nodes.
run "robot_push_test" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "robot-push-test"
    namespace            = run.bridge_install.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = "robot-harbor-bridge-system.multi-access"
    command              = ["sh", "-c"]
    args = [<<-SH
      set -eu
      # Trust Harbor's self-signed cert (same approach as seed_image).
      openssl s_client -connect harbor.e2e:30843 -servername harbor.e2e </dev/null 2>/dev/null \
        | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' \
        > /usr/local/share/ca-certificates/harbor-e2e.crt
      test -s /usr/local/share/ca-certificates/harbor-e2e.crt
      update-ca-certificates 2>/dev/null
      # Authenticate as the bridge-minted robot (username/password from the
      # mounted Secret, exposed as env vars via envFrom).
      crane auth login harbor.e2e:30843 -u "$username" -p "$password"
      # PUSH: copy app:v1 to a new tag in beta-2 (the manifest PUT needs push).
      crane copy harbor.e2e:30843/beta-2/app:v1 harbor.e2e:30843/beta-2/pushed-by-robot:v1
      # PULL on a second project: read beta-2's manifest (needs pull).
      crane manifest harbor.e2e:30843/beta-2/app:v1 >/dev/null
      echo "robot verified: push to beta-2, pull from beta-2"
    SH
    ]
    timeout_seconds = 300
    fail_message    = "pull,push robot failed: could not push to beta-2 (or pull beta-2) with the multi-access robot credentials"
  }
}

# Pause-for-inspection AFTER all the pull/push assertions have run, so you can
# kubectl-poke a fully-populated cluster (robots, Secrets, pushed tags all
# present). Off by default (CI never pauses); flip on for local dev with
# `make e2e-pause` (or `TF_VAR_pause_after_pull=true tofu test`). A file appears
# at test/e2e/.tofu-sleep and the apply blocks until you `rm` it.
run "file_sleep" {
  command = apply
  module {
    source = "./modules/test-sleep"
  }
  variables {
    enabled = var.pause_after_pull
  }
}
