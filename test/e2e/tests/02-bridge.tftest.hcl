# End-to-end test of the Harbor Workload Identity Bridge on kind.
#
# Stages (each `run` block is a separate plan+apply; runs that use the same
# module source share one OpenTofu state):
#   1. build_images         — `docker build` bridge + plugin + seed images
#   2. cluster              — kind cluster (cilium + cert-manager); loads images
#   3. harbor               — Harbor via the official chart, NodePort 30843
#   4. containerd_trust     — Harbor's cert into every node's containerd
#   5. coredns_rewrite      — harbor.e2e resolvable cluster-wide
#   6. seed_image           — create the scenario projects, push the test
#                             image into each, and WAIT until that finished
#   7. bridge_install       — our chart (2 bridge replicas, like the default)
#   8. harbor_access        — scenario phase "initial" (modules/harbor-access-scenario)
#   9. pull_pod*            — one Job per scenario; success = end-to-end works
#  10. robot_push_test      — the minted pull,push robot really can push
#  11. bridge_upgrade       — helm upgrade widening matchImages; the installer
#                             must restart kubelet (ADR-0021) …
#  12. pull_pod_upgrade     — … or this pull of the new project fails
#  13. harbor_access_update — scenario phase "updated": a permission grant
#                             and a ServiceAccount change, applied in place
#  14. pull_pod_granted     — the newly granted project pulls (the grant
#                             reached Harbor; audit C1)
#  15. pull_pod_renamed     — the new ServiceAccount pulls
#  16. pull_pod_revoked     — the OLD ServiceAccount must now be refused
#  17. robot_check_update   — Harbor itself: old robot gone, new one present
#                             (audit H2 — revocation, not just a data-plane 403)
#  18. file_sleep (opt-in)  — pause for kubectl-poking a populated cluster
#  19. harbor_access_teardown — scenario phase "none": every HarborAccess and
#                             tenant namespace deleted WHILE the bridge runs;
#                             tofu waits for each finalizer
#  20. robot_check_teardown — Harbor itself: not one robot of this cluster left
#
# Teardown order. tofu test destroys the states in reverse order of the
# LAST run that touched each. Stages 13 and 19 re-use the harbor_access
# state after bridge_upgrade, and stage 19 empties it, so at cleanup time
# no HarborAccess (and no finalizer) is left for an already-uninstalled
# bridge to release. Before this, bridge_upgrade (the last run using the
# bridge state) made cleanup uninstall the bridge FIRST, and deleting the
# CRs then hung on finalizers nobody could remove.
#
# Multi-tenant / collision coverage:
#   - team-a/svc-b → project-alpha and team/a-svc-b → project-beta collide
#     under the old hyphen-joined robot names but are distinct under
#     ADR-0018's dot-joined scheme; each pulls only its own project.
#   - app-ns/runner's HarborAccess lives in app-ns: cluster-wide CR pickup.
#   - beta-ns/beta-runner: one robot, pull,push on beta-1/2/3.
#
# Image refs use `harbor.e2e:30843` so that:
#   - go-containerregistry (crane) accepts the realm host in Harbor's
#     www-authenticate response — it rejects loopback / RFC1918 hosts.
#   - containerd on the kind nodes routes via certs.d/harbor.e2e:30843 to
#     the NodePort, trusting Harbor's self-signed cert (containerd_trust).
#   - kind nodes map harbor.e2e to 127.0.0.1 in /etc/hosts; pods resolve it
#     through CoreDNS (coredns_rewrite).
#
# A failing pull stage leaves diagnostics (pod events, bridge + installer
# logs, kubelet journal, HarborAccess state) under
# test/e2e/.diag/<job>/ before the cluster is destroyed.

# Build bridge + plugin images directly from the repo's Dockerfiles.
# Paths are relative to test/e2e (the CWD when `tofu test` is invoked),
# so ../.. is the repo root.
run "build_images" {
  command = apply
  module {
    source = "./modules/docker-build"
  }
  variables {
    images = {
      bridge = {
        tag        = "harbor-bridge:e2e"
        dockerfile = "Dockerfile.bridge"
        context    = "../.."
      }
      plugin = {
        tag        = "harbor-bridge-plugin:e2e"
        dockerfile = "Dockerfile.plugin"
        context    = "../.."
      }
      # alpine + curl + jq + openssl + crane — the seed and check Jobs.
      seed = {
        tag        = "e2e-seed:e2e"
        dockerfile = "Dockerfile"
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
    name         = "bridge-e2e"
    worker_count = 2
    # Host-side ports kind binds. Overridable (TF_VAR_host_*) so the
    # harness can run next to another local kind cluster that already
    # holds the defaults. try(): inside a run block `var` holds only the
    # CLI/TF_VAR values, never the root module's defaults.
    http_port           = try(var.host_http_port, 8080)
    https_port          = try(var.host_https_port, 8443)
    api_server_port     = try(var.host_api_server_port, 6443)
    enable_cert_manager = true
    # No registry_insecure_hostnames: containerd_trust writes the
    # hosts.toml for harbor.e2e (with the real CA) before anything pulls
    # from Harbor; a skip_verify file here was overwritten immediately and
    # cost one extra containerd restart per node.
    extra_etc_hosts = ["127.0.0.1 harbor.e2e"]
    images_to_load  = run.build_images.image_tags_list
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
    # null on a normal run → the harbor module's renovate-pinned default.
    # The harbor-compat matrix sets TF_VAR_version_harbor (ADR-0020).
    version_harbor = try(var.version_harbor, null)
  }
}

# Harbor's self-signed leaf cert into each node's containerd certs.d, so
# containerd verifies Harbor instead of relying on skip_verify (which
# containerd v2.x has been observed to ignore).
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

run "seed_image" {
  command = apply
  module {
    source = "./modules/harbor-seed"
  }
  variables {
    kubeconfig     = run.cluster.kubeconfig
    admin_password = run.harbor.admin_password
    projects = [
      "your-project", "project-alpha", "project-beta", "project-gamma",
      "beta-1", "beta-2", "beta-3", "upgrade-only",
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
    # No path glob — kubelet's matchImages supports globs in the domain
    # only. LITERAL path prefixes are supported ("host/project"): scope the
    # initial install to the per-project prefixes and leave upgrade-only
    # out — bridge_upgrade adds it and asserts kubelet picked it up.
    match_images = [
      "harbor.e2e:30843/your-project",
      "harbor.e2e:30843/project-alpha",
      "harbor.e2e:30843/project-beta",
      "harbor.e2e:30843/project-gamma",
      "harbor.e2e:30843/beta-1",
      "harbor.e2e:30843/beta-2",
      "harbor.e2e:30843/beta-3",
    ]
    bridge_image = run.build_images.image_refs.bridge
    plugin_image = run.build_images.image_refs.plugin
  }
}

run "harbor_access" {
  command = apply
  module {
    source = "./modules/harbor-access-scenario"
  }
  variables {
    kubeconfig       = run.cluster.kubeconfig
    phase            = "initial"
    bridge_namespace = run.bridge_install.namespace
    audience         = "harbor-bridge"
  }
}

# Load-bearing assertion: kubelet execs the plugin, the plugin gets robot
# credentials from the bridge, containerd pulls with them.
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
    fail_message         = "baseline pull failed: kubelet → plugin → bridge → Harbor chain broken"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Collision-resistance (half 1 of 2): team-a/svc-b pulls project-alpha via
# robot bridge-dev.team-a.svc-b.
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
    fail_message         = "ADR-0018 collision regression or isolation break: team-a/svc-b could not pull project-alpha"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Collision-resistance (half 2 of 2): team/a-svc-b pulls project-beta via
# robot bridge-dev.team.a-svc-b.
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
    fail_message         = "ADR-0018 collision regression or isolation break: team/a-svc-b could not pull project-beta"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Cluster-wide CR pickup: the HarborAccess lives in app-ns.
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
    fail_message         = "cluster-wide CR pickup broke: app-ns/runner could not pull project-gamma"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Multi-project robot (pull,push on beta-1/2/3) serves a pull.
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
    fail_message         = "multi-project pull broke: beta-ns/beta-runner could not pull beta-1"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# pull,push assertion: a Job in the bridge namespace uses the multi-access
# robot's credentials (the Secret the bridge wrote, via envFrom) to PUSH a
# new tag into beta-2 — a manifest PUT that needs the push grant — and to
# read beta-3. A pull-only robot is refused on the push.
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
    env_from_secret      = "robot-${run.bridge_install.namespace}.multi-access"
    command              = ["sh", "-c"]
    args = [<<-SH
      set -eu
      openssl s_client -connect harbor.e2e:30843 -servername harbor.e2e </dev/null 2>/dev/null \
        | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' \
        > /usr/local/share/ca-certificates/harbor-e2e.crt
      test -s /usr/local/share/ca-certificates/harbor-e2e.crt
      update-ca-certificates 2>/dev/null
      crane auth login harbor.e2e:30843 -u "$username" -p "$password"
      crane copy harbor.e2e:30843/beta-2/app:v1 harbor.e2e:30843/beta-2/pushed-by-robot:v1
      crane digest harbor.e2e:30843/beta-2/pushed-by-robot:v1 >/dev/null
      crane digest harbor.e2e:30843/beta-3/app:v1 >/dev/null
      echo "robot verified: push to beta-2, pull from beta-3"
    SH
    ]
    timeout_seconds  = 300
    fail_message     = "pull,push robot failed: could not push to beta-2 (or read beta-3) with the multi-access robot credentials"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# ── ADR-0021 upgrade convergence. Re-apply the bridge install (same helm
# release → in-place `helm upgrade`) with matchImages widened by the
# upgrade-only project. The rendered credential-provider ConfigMap changes →
# the DaemonSet re-rolls → the installer sees different effective config
# content → restarts kubelet on every node.
run "bridge_upgrade" {
  module {
    source = "./modules/harbor-bridge-install"
  }
  variables {
    kubeconfig            = run.cluster.kubeconfig
    cluster_name          = "dev"
    harbor_url            = run.harbor.internal_api_url
    harbor_admin_password = run.harbor.admin_password
    audience              = "harbor-bridge"
    match_images = [
      "harbor.e2e:30843/your-project",
      "harbor.e2e:30843/project-alpha",
      "harbor.e2e:30843/project-beta",
      "harbor.e2e:30843/project-gamma",
      "harbor.e2e:30843/beta-1",
      "harbor.e2e:30843/beta-2",
      "harbor.e2e:30843/beta-3",
      "harbor.e2e:30843/upgrade-only",
    ]
    bridge_image = run.build_images.image_refs.bridge
    plugin_image = run.build_images.image_refs.plugin
  }
}

# This image was NOT covered by the initial matchImages; its HarborAccess
# and robot existed all along. The one variable is whether kubelet runs
# the upgraded credential-provider config.
run "pull_pod_upgrade" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-upgrade-only"
    namespace            = "upgrade-ns"
    service_account_name = "upgrade-runner"
    image                = "harbor.e2e:30843/upgrade-only/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo upgrade-ns/upgrade-runner pulled upgrade-only after helm upgrade; exit 0"]
    timeout_seconds      = 300
    fail_message         = "ADR-0021 regression: helm upgrade changed matchImages but kubelet kept the old credential-provider config"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# ── Lifecycle: edit two HarborAccess objects in place. The wait inside the
# scenario module blocks until each is Ready at its NEW generation.
# (From here on the install module's outputs come from run.bridge_upgrade:
# a run that re-uses a state replaces the earlier run's outputs.)
run "harbor_access_update" {
  command = apply
  module {
    source = "./modules/harbor-access-scenario"
  }
  variables {
    kubeconfig       = run.cluster.kubeconfig
    phase            = "updated"
    bridge_namespace = run.bridge_upgrade.namespace
    audience         = "harbor-bridge"
  }
}

# The grant added to test-access must reach Harbor. image-puller may still
# have its credentials cached by kubelet from pull_pod: the password must
# not have changed (no rotation on a spec edit), only the robot's grants.
run "pull_pod_granted" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-granted-gamma"
    namespace            = "test-pull"
    service_account_name = "image-puller"
    image                = "harbor.e2e:30843/project-gamma/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo test-pull/image-puller pulled newly granted project-gamma; exit 0"]
    timeout_seconds      = 300
    fail_message         = "permission update did not take effect in Harbor (audit C1), or a rotation broke kubelet-cached credentials"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# collide-one now names team-a/svc-renamed: the new identity pulls.
run "pull_pod_renamed" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-renamed-alpha"
    namespace            = "team-a"
    service_account_name = "svc-renamed"
    image                = "harbor.e2e:30843/project-alpha/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo team-a/svc-renamed pulled project-alpha; exit 0"]
    timeout_seconds      = 300
    fail_message         = "serviceAccountRef change: the new ServiceAccount could not pull"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# … and the old identity must be refused. kubelet on the node that ran
# pull_pod_alpha may still cache the OLD robot's credentials for up to the
# tokenTTL — this passes only if that robot was revoked in Harbor (a
# data-plane 403 alone would not stop a cached password).
run "pull_pod_revoked" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-revoked-alpha"
    namespace            = "team-a"
    service_account_name = "svc-b"
    image                = "harbor.e2e:30843/project-alpha/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo SHOULD NOT RUN; exit 0"]
    timeout_seconds      = 240
    expect_pull_failure  = true
    fail_message         = "revocation failed: team-a/svc-b could still pull project-alpha after its HarborAccess moved to another ServiceAccount (audit H2)"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Ask Harbor directly (admin credentials from the seed namespace): the old
# robot is gone, the new one exists.
run "robot_check_update" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "robot-check-update"
    namespace            = run.seed_image.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = run.seed_image.admin_secret_name
    command              = ["sh", "-c"]
    args = [<<-SH
      set -eu
      api=http://harbor-core.harbor.svc.cluster.local/api/v2.0
      count() { curl -fsS -u "$username:$password" "$api/robots?page_size=100&q=name%3D$1" | jq 'length'; }
      old=$(count bridge-dev.team-a.svc-b); new=$(count bridge-dev.team-a.svc-renamed)
      echo "old robot: $old, new robot: $new"
      test "$old" = 0
      test "$new" = 1
    SH
    ]
    timeout_seconds  = 120
    fail_message     = "Harbor state after the serviceAccountRef change is wrong: the old robot must be deleted and the new one present"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Pause-for-inspection on a fully populated cluster (before the teardown
# stages empty it). Off by default; `make e2e-pause` turns it on.
run "file_sleep" {
  command = apply
  module {
    source = "./modules/test-sleep"
  }
  variables {
    enabled = try(var.pause_after_pull, false)
  }
}

# ── Lifecycle: delete every HarborAccess (and the tenant namespaces) while
# the bridge runs. Each deletion blocks on the bridge's finalizer, so this
# run passes only if every finalizer is released.
run "harbor_access_teardown" {
  command = apply
  module {
    source = "./modules/harbor-access-scenario"
  }
  variables {
    kubeconfig       = run.cluster.kubeconfig
    phase            = "none"
    bridge_namespace = run.bridge_upgrade.namespace
    audience         = "harbor-bridge"
  }
}

# The finalizers must have REVOKED the robots, not just let the CRs go:
# not one robot of cluster "dev" may remain in Harbor.
run "robot_check_teardown" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "robot-check-teardown"
    namespace            = run.seed_image.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = run.seed_image.admin_secret_name
    command              = ["sh", "-c"]
    args = [<<-SH
      set -eu
      api=http://harbor-core.harbor.svc.cluster.local/api/v2.0
      # Poll briefly: the finalizer deletes the robot before the CR goes
      # away, but give Harbor's list a moment to reflect the delete.
      for i in $(seq 1 30); do
        left=$(curl -fsS -u "$username:$password" "$api/robots?page_size=100&q=name%3D~bridge-dev." | jq -r '.[].name')
        if [ -z "$left" ]; then echo "no robots of cluster dev left in Harbor"; exit 0; fi
        sleep 3
      done
      echo "robots left behind:"; echo "$left"; exit 1
    SH
    ]
    timeout_seconds  = 120
    fail_message     = "finalizer cleanup incomplete: robots of cluster dev are still in Harbor after every HarborAccess was deleted"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}
