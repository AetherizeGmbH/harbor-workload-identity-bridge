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
#   7. bridge_install       — our chart (2 bridge replicas, like the default);
#                             waits until every replica is Ready
#   8. harbor_access        — scenario phase "initial" (modules/harbor-access-scenario)
#  8b. bridge_replicas      — every bridge replica, asked at its own address,
#                             issues credentials (audit H1)
#   9. pull_pod*            — one Job per scenario; success = end-to-end works.
#                             pull_pod_cross_tenant / _before_grant: an
#                             identity is refused on a project it was not
#                             granted (Harbor decides, not the bridge)
#  10. robot_push_test      — the minted pull,push robot really can push
#  11. bridge_upgrade       — helm upgrade widening matchImages; the installer
#                             must restart kubelet (ADR-0021) …
#  12. pull_pod_upgrade     — … or this pull of the new project fails
#  13. harbor_access_update — scenario phase "updated": a permission grant,
#                             grants removed from a robot that stays, and a
#                             ServiceAccount change, applied in place
#  14. pull_pod_granted     — the newly granted project pulls (the grant
#                             reached Harbor; audit C1)
#  14b. robot_narrowed      — the narrowed robot keeps what stayed and is
#                             refused what was removed (ADR-0023)
#  15. pull_pod_renamed     — the new ServiceAccount pulls
#  16. pull_pod_revoked     — the OLD ServiceAccount must now be refused
#  17. robot_check_update   — Harbor itself: old robot gone, new one present,
#                             one robot per HarborAccess, the narrowed
#                             robot's stored grants (audit H2 — revocation,
#                             not just a data-plane 403)
#  18. token_rejection      — ADR-0028: the bridge refuses a token bound to no
#                             pod and one living 2h, serves a pod-bound 1h one
#  19. file_sleep (opt-in)  — pause for kubectl-poking a populated cluster
#  19b. harbor_access_cascade — scenario phase "ns-cascade": the namespace
#                             app-ns deleted with its HarborAccess in it;
#                             the namespace waits for the finalizer
#  19c. robot_check_cascade — Harbor itself: that robot revoked, the others
#                             kept
#  20. harbor_access_teardown — scenario phase "none": every HarborAccess and
#                             tenant namespace deleted WHILE the bridge runs;
#                             tofu waits for each finalizer
#  21. robot_check_teardown — Harbor itself: not one robot of this cluster left
#                             (a Harbor that cannot be asked fails it)
#
# Teardown order. tofu test destroys the states in reverse order of the
# LAST run that touched each. Stages 13, 19b and 20 re-use the
# harbor_access state after bridge_upgrade, and stage 20 empties it, so at
# cleanup time no HarborAccess (and no finalizer) is left for an
# already-uninstalled bridge to release. Before this, bridge_upgrade (the
# last run using the bridge state) made cleanup uninstall the bridge
# FIRST, and deleting the CRs then hung on finalizers nobody could remove.
#
# Multi-tenant / collision coverage:
#   - team-a/svc-b → project-alpha and team/a-svc-b → project-beta collide
#     under the old hyphen-joined robot names but are distinct under
#     ADR-0018's dot-joined scheme; each pulls its own project, and
#     team-a/svc-b is refused on project-beta (pull_pod_cross_tenant).
#   - app-ns/runner's HarborAccess lives in app-ns: cluster-wide CR pickup.
#   - beta-ns/beta-runner: one robot, pull,push on beta-1/2/3; narrowed in
#     place to pull,push on beta-1 and pull on beta-2 (robot_narrowed).
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

# ── ADR-0025 / audit H1: EVERY bridge replica serves credentials. The
# install already waited until each replica was Ready; kubelet's pulls go
# through the chart's Service to whichever replica it picks, and kubelet
# retries a failed pull, so a replica that does not serve would hide behind
# the others. This Job runs as token-ns/token-check with a kubelet-projected
# token (pod-bound, 1h, bridge audience — what the plugin sends), resolves
# the harness's headless Service to every bridge pod, Ready or not, and
# asks each pod at its own address for credentials: every one must answer
# 200 with that ServiceAccount's robot, and there must be exactly as many
# pods as replicas. curl still verifies the serving certificate against the
# Service name (--resolve pins only the address).
run "bridge_replicas" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig               = run.cluster.kubeconfig
    name                     = "bridge-replicas"
    namespace                = "token-ns"
    service_account_name     = "token-check"
    image                    = "e2e-seed:e2e"
    image_pull_policy        = "IfNotPresent"
    projected_token_audience = "harbor-bridge"
    command                  = ["sh", "-c"]
    args = [<<-SH
      set -eu
      url='${run.bridge_install.credentials_url}'
      pods='${run.bridge_install.bridge_pods_host}'
      want=${run.bridge_install.bridge_replicas}
      robot=bridge-dev.token-ns.token-check
      host=$${url#https://}; host=$${host%%/*}
      name=$${host%:*}; port=$${host##*:}

      # The serving certificate off the wire, as in token_rejection.
      openssl s_client -connect "$host" -servername "$name" </dev/null 2>/dev/null \
        | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' > /tmp/bridge-ca.crt
      test -s /tmp/bridge-ca.crt

      # Every bridge pod's address. Poll: a pod of an earlier rollout can
      # still be terminating, and the DNS answer can trail the endpoints.
      n=0; ips=""
      for i in $(seq 1 20); do
        ips=$(getent ahosts "$pods" | awk '$2 == "STREAM" {print $1}' | sort -u)
        n=$(printf '%s\n' "$ips" | grep -c . || true)
        [ "$n" = "$want" ] && break
        sleep 3
      done
      if [ "$n" != "$want" ]; then
        echo "$pods resolves to $n bridge pods ($(echo $ips)), want $want"; exit 1
      fi

      tok=$(cat /var/run/secrets/tokens/token)
      for ip in $ips; do
        code=$(curl -sS -m 10 -o /tmp/response -w '%%{http_code}' --cacert /tmp/bridge-ca.crt \
          --resolve "$name:$port:$ip" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' \
          --data "{\"image\":\"replica-check/$ip\"}" "$url") || code="no answer (curl exit $?)"
        if [ "$code" != 200 ]; then
          echo "bridge pod $ip: $code, want 200: $(cat /tmp/response 2>/dev/null)"; exit 1
        fi
        if ! jq -e --arg robot "$robot" '(.username | endswith($robot))
            and (.password | type == "string" and length > 0)' /tmp/response >/dev/null; then
          rm -f /tmp/response; echo "bridge pod $ip: HTTP 200, but not with the credentials of $robot"; exit 1
        fi
        rm -f /tmp/response
        echo "bridge pod $ip: 200 with the credentials of $robot"
      done
    SH
    ]
    timeout_seconds  = 180
    fail_message     = "audit H1 / ADR-0025: not every bridge replica serves credentials at its own address (see pod.log and bridge.log)"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
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

# Tenant isolation, checked by Harbor: the bridge ignores the requested
# image (it only logs it) and hands every identity its own robot, so what
# keeps a tenant out of another tenant's project is the robot's grant in
# Harbor alone. team-a/svc-b holds pull on project-alpha only; project-beta
# (collide-two's project, private, in matchImages) must refuse it. A grant
# too broad in Harbor (a system-wide or wrong-project permission, a
# Harbor version reading a grant more widely) passes every positive pull
# and fails here.
run "pull_pod_cross_tenant" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-cross-tenant-beta"
    namespace            = "team-a"
    service_account_name = "svc-b"
    image                = "harbor.e2e:30843/project-beta/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo SHOULD NOT RUN; exit 0"]
    timeout_seconds      = 240
    expect_pull_failure  = true
    fail_message         = "tenant isolation broken: team-a/svc-b (granted project-alpha only) could pull project-beta, or the pull failed for a reason other than authorization"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# … and the same for the identity that gains a grant later: test-pull/
# image-puller may not pull project-gamma yet. pull_pod_granted repeats
# this pull after harbor_access_update added the grant, so between the two
# only the grant changes.
run "pull_pod_before_grant" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-before-grant-gamma"
    namespace            = "test-pull"
    service_account_name = "image-puller"
    image                = "harbor.e2e:30843/project-gamma/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo SHOULD NOT RUN; exit 0"]
    timeout_seconds      = 240
    expect_pull_failure  = true
    fail_message         = "tenant isolation broken: test-pull/image-puller could pull project-gamma before any HarborAccess granted it, or the pull failed for a reason other than authorization"
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

# Revocation by CR edit (ADR-0023): multi-access was narrowed in place —
# beta-3 dropped, beta-2 cut to pull. With the same robot Secret (a spec
# edit rotates no password): what stayed still works, push to beta-2 and
# any access to beta-3 are refused by Harbor. crane asks Harbor for a fresh
# token on every call, so this is Harbor's evaluation of the stored grants,
# not a cached credential. A refusal counts only as an authorization error;
# a TLS or DNS failure, or a missing tag, fails the check.
run "robot_narrowed" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "robot-narrowed"
    namespace            = run.bridge_upgrade.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = "robot-${run.bridge_upgrade.namespace}.multi-access"
    command              = ["sh", "-c"]
    args = [<<-SH
      set -eu
      H=harbor.e2e:30843
      openssl s_client -connect "$H" -servername harbor.e2e </dev/null 2>/dev/null \
        | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' \
        > /usr/local/share/ca-certificates/harbor-e2e.crt
      test -s /usr/local/share/ca-certificates/harbor-e2e.crt
      update-ca-certificates 2>/dev/null
      crane auth login "$H" -u "$username" -p "$password"

      # refused WHAT COMMAND...: COMMAND must fail with an authorization error.
      refused() {
        what=$1; shift
        if out=$("$@" 2>&1); then echo "$what: SUCCEEDED, want an authorization failure"; exit 1; fi
        if ! printf '%s' "$out" | grep -Eqi 'unauthorized|denied|forbidden|insufficient_scope'; then
          echo "$what: failed, but not with an authorization error: $out"; exit 1
        fi
        echo "$what: refused"
      }

      # The grants that stayed, with the unchanged password.
      crane copy "$H/beta-1/app:v1" "$H/beta-1/pushed-after-narrowing:v1"
      crane digest "$H/beta-2/app:v1" >/dev/null
      echo "kept: push to beta-1, pull from beta-2"
      refused "push to beta-2 (push removed)" crane copy "$H/beta-2/app:v1" "$H/beta-2/pushed-after-narrowing:v1"
      refused "pull from beta-3 (project removed)" crane digest "$H/beta-3/app:v1"
    SH
    ]
    timeout_seconds  = 180
    fail_message     = "revocation by CR edit failed (ADR-0023): after multi-access was narrowed, its robot could still push to beta-2 or read beta-3, or lost a grant it kept (see pod.log)"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
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
# robot is gone, the new one exists, the fuzzy query robot_check_teardown
# relies on lists exactly one robot per HarborAccess of this phase, and the
# narrowed robot stores exactly its new grants. Every
# query fails the Job when Harbor cannot be asked or does not answer with a
# robot list: an unreachable Harbor must never read as "no robot".
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
      set -euo pipefail
      api=http://harbor-core.harbor.svc.cluster.local/api/v2.0
      # robots QUERY: the names of the robots Harbor lists for QUERY, as a
      # JSON array. A failed request, or an answer that is not exactly one
      # JSON list, fails the function, and set -e then ends the Job at the
      # assignment. jq -s reads the whole answer: an empty body (curl -f
      # accepts any 2xx or 3xx) is no value at all, not an empty list.
      # busybox sh does not apply set -e inside a command substitution,
      # hence the explicit return.
      robots() {
        body=$(curl -fsS -m 10 -u "$username:$password" "$api/robots?page_size=100&q=$1") || return 1
        printf '%s' "$body" | jq -cs 'if length == 1 and (.[0] | type) == "array" then [.[0][].name] else error("Harbor did not answer with one robot list") end'
      }
      old=$(robots name%3Dbridge-dev.team-a.svc-b)
      new=$(robots name%3Dbridge-dev.team-a.svc-renamed)
      all=$(robots name%3D~bridge-dev.)
      echo "old robot: $old; new robot: $new; robots of cluster dev: $all"
      printf '%s' "$old" | jq -e 'length == 0' >/dev/null
      printf '%s' "$new" | jq -e 'length == 1' >/dev/null
      printf '%s' "$all" | jq -e --argjson want ${length(run.harbor_access_update.harbor_accesses)} \
        'length == $want and any(.[]; endswith("bridge-dev.team-a.svc-renamed"))' >/dev/null

      # The narrowed robot's grants as Harbor stores them: exactly pull,push
      # on beta-1 and pull on beta-2 — replaced, not merged with the old list.
      # The answer must be exactly one JSON list holding that one robot.
      body=$(curl -fsS -m 10 -u "$username:$password" "$api/robots?page_size=100&q=name%3Dbridge-dev.beta-ns.beta-runner")
      grants=$(printf '%s' "$body" | jq -cs 'if length == 1 and (.[0] | type) == "array" and (.[0] | length) == 1 then .[0][0].permissions
          | map({kind, namespace, actions: ([.access[] | "\(.resource):\(.action)"] | sort)}) | sort_by(.namespace)
        else error("want exactly one robot bridge-dev.beta-ns.beta-runner") end')
      echo "narrowed robot's grants: $grants"
      printf '%s' "$grants" | jq -e '. == [
        {kind: "project", namespace: "beta-1", actions: ["repository:pull", "repository:push"]},
        {kind: "project", namespace: "beta-2", actions: ["repository:pull"]}]' >/dev/null
    SH
    ]
    timeout_seconds  = 120
    fail_message     = "Harbor state after harbor_access_update is wrong: the old robot must be deleted, the new one present, the fuzzy name=~bridge-dev. query must list one robot per HarborAccess, and the narrowed robot must hold exactly its new grants (or Harbor could not be asked; see pod.log)"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# ── ADR-0028 against a real apiserver and the deployed bridge. A Job runs as
# token-ns/token-check (HarborAccess token-check; the ServiceAccount may
# create tokens for itself and nothing else), asks the TokenRequest API for
# three tokens with the bridge audience and sends each to the credential
# endpoint through the bridge's Service:
#   - bound to the Job's own pod, 1h → 200 with this ServiceAccount's robot
#     credentials. The control: same ServiceAccount, audience and pod as
#     the two tokens below;
#   - bound to no pod, 1h            → 401, audit category not_pod_bound;
#   - bound to the pod, 2h           → 401, audit category excessive_lifetime.
# Before sending a token the Job checks the claims the apiserver really
# issued (subject, audience, exp - iat, pod binding), so each 401 has the
# one cause it is meant to test: an apiserver that lengthened the unbound
# token would get it refused for its lifetime instead, and one that capped
# the 2h token would fail the stage as a bridge bug. After the Job, the
# bridge's audit log must hold each decision, found by the marker the Job
# sent as the image. The Job goes with token-ns in harbor_access_teardown.
run "token_rejection" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "token-rejection"
    namespace            = "token-ns"
    service_account_name = "token-check"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_field_refs = {
      POD_NAME = "metadata.name"
      POD_UID  = "metadata.uid"
      SA_NAME  = "spec.serviceAccountName"
    }
    command = ["sh", "-c"]
    args = [<<-SH
      set -eu
      url='${run.bridge_upgrade.credentials_url}'
      aud=harbor-bridge
      robot=bridge-dev.token-ns.token-check
      sa=/var/run/secrets/kubernetes.io/serviceaccount
      ns=$(cat "$sa/namespace")

      # The bridge's serving certificate, off the wire like Harbor's in
      # robot_push_test. The harness's issuer is selfSigned, so it is the
      # CA cert-manager writes to ca.crt for the plugin; curl still checks
      # that it names the Service host.
      host=$${url#https://}; host=$${host%%/*}
      openssl s_client -connect "$host" -servername "$${host%:*}" </dev/null 2>/dev/null \
        | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' > /tmp/bridge-ca.crt
      test -s /tmp/bridge-ca.crt

      # mint SECONDS BINDING: a token for this pod's ServiceAccount with the
      # bridge audience, bound to this pod if BINDING is "pod". Never printed.
      mint() {
        jq -n --arg aud "$aud" --argjson exp "$1" --arg bind "$2" --arg pod "$POD_NAME" --arg uid "$POD_UID" '
          {apiVersion: "authentication.k8s.io/v1", kind: "TokenRequest",
           spec: ({audiences: [$aud], expirationSeconds: $exp}
             + (if $bind == "pod" then {boundObjectRef: {apiVersion: "v1", kind: "Pod", name: $pod, uid: $uid}} else {} end))}' \
          > /tmp/tokenrequest.json
        code=$(curl -sS -o /tmp/tokenresponse.json -w '%%{http_code}' -X POST --cacert "$sa/ca.crt" \
          -H "Authorization: Bearer $(cat "$sa/token")" -H 'Content-Type: application/json' \
          --data @/tmp/tokenrequest.json \
          "https://kubernetes.default.svc/api/v1/namespaces/$ns/serviceaccounts/$SA_NAME/token") || true
        case "$code" in
          200|201) jq -er .status.token /tmp/tokenresponse.json ;;
          *) echo "TokenRequest ($1s, $2): HTTP $code" >&2; cat /tmp/tokenresponse.json >&2; return 1 ;;
        esac
      }

      # claims TOKEN: the payload (base64url without padding) as JSON.
      claims() {
        p=$(printf '%s' "$1" | cut -d. -f2 | tr '_-' '/+')
        case $(( $${#p} % 4 )) in 2) p="$p==" ;; 3) p="$p=" ;; esac
        printf '%s' "$p" | base64 -d
      }

      # issued NAME TOKEN SECONDS BINDING: the apiserver issued exactly what
      # was asked, or the bridge's answer would prove nothing.
      issued() {
        claims "$2" | jq -e --arg sub "system:serviceaccount:$ns:$SA_NAME" --arg aud "$aud" \
            --argjson life "$3" --arg bind "$4" --arg pod "$POD_NAME" --arg uid "$POD_UID" '
          .sub == $sub
          and (.aud | if type == "array" then any(.[]; . == $aud) else . == $aud end)
          and .exp - .iat == $life
          and (if $bind == "pod"
               then .["kubernetes.io"].pod.name == $pod and .["kubernetes.io"].pod.uid == $uid
               else .["kubernetes.io"].pod == null end)' >/dev/null \
          || { echo "$1: the apiserver did not issue it as requested (subject, audience, $3s lifetime, binding $4)"; exit 1; }
      }

      # post MARKER TOKEN: ask the bridge, print the HTTP status. The image
      # is only an audit marker; the harness finds it in the bridge log.
      post() {
        curl -sS -o /tmp/response -w '%%{http_code}' --cacert /tmp/bridge-ca.crt \
          -H "Authorization: Bearer $2" -H 'Content-Type: application/json' \
          --data "{\"image\":\"token-check/$1\"}" "$url"
      }

      # The control. The response carries a live password: only its shape
      # is checked, and it is never printed.
      tok=$(mint 3600 pod)
      issued "pod-bound 1h token" "$tok" 3600 pod
      code=$(post bound-1h "$tok") || true
      if [ "$code" != 200 ]; then
        echo "pod-bound 1h token: HTTP $code, want 200: $(cat /tmp/response 2>/dev/null)"; exit 1
      fi
      if ! jq -e --arg robot "$robot" '(.username | endswith($robot))
          and (.password | type == "string" and length > 0)
          and (.expires_in | type == "number") and .cache_key_type == "Registry"' /tmp/response >/dev/null; then
        rm -f /tmp/response; echo "pod-bound 1h token: HTTP 200, but not with the credentials of $robot"; exit 1
      fi
      rm -f /tmp/response
      echo "pod-bound 1h token: 200 with the credentials of $robot"

      # refused MARKER NAME SECONDS BINDING
      refused() {
        tok=$(mint "$3" "$4")
        issued "$2" "$tok" "$3" "$4"
        code=$(post "$1" "$tok") || true
        if [ "$code" = 200 ]; then
          rm -f /tmp/response; echo "$2: ACCEPTED with HTTP 200, want 401"; exit 1
        fi
        body=$(cat /tmp/response 2>/dev/null || true)
        if [ "$code" != 401 ] || [ "$body" != "invalid token" ]; then
          echo "$2: HTTP $code ($body), want 401 invalid token"; exit 1
        fi
        echo "$2: 401 invalid token"
      }
      refused unbound-1h "unbound 1h token" 3600 none
      refused bound-2h "pod-bound 2h token" 7200 pod
    SH
    ]
    expect_bridge_log = [
      ["\"logger\":\"audit\"", "\"msg\":\"credential issued\"", "\"requested_image\":\"token-check/bound-1h\"", "\"harboraccess\":\"${run.bridge_upgrade.namespace}/token-check\"", "\"pod\":\"token-rejection-"],
      ["\"logger\":\"audit\"", "\"msg\":\"credential denied\"", "\"requested_image\":\"token-check/unbound-1h\"", "\"category\":\"not_pod_bound\""],
      ["\"logger\":\"audit\"", "\"msg\":\"credential denied\"", "\"requested_image\":\"token-check/bound-2h\"", "\"category\":\"excessive_lifetime\""],
    ]
    timeout_seconds  = 180
    fail_message     = "ADR-0028: the bridge accepted an unbound or longer-than-1h token, refused a pod-bound 1h token, or did not log the decision with its category (see pod.log and bridge.log)"
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

# ── Lifecycle: `kubectl delete namespace` on a tenant. Scenario phase
# "ns-cascade" drops only the Namespace app-ns; its HarborAccess
# (tenant-access) and ServiceAccount stay in the phase, so the namespace
# controller, not tofu, deletes them while the namespace is Terminating.
# tofu waits until the namespace is gone, which it is only once the bridge
# released tenant-access's finalizer.
run "harbor_access_cascade" {
  command = apply
  module {
    source = "./modules/harbor-access-scenario"
  }
  variables {
    kubeconfig       = run.cluster.kubeconfig
    phase            = "ns-cascade"
    bridge_namespace = run.bridge_upgrade.namespace
    audience         = "harbor-bridge"
  }
}

# The finalizer released the namespace only after it revoked the robot: the
# robot of app-ns/runner is gone from Harbor, every other one is still there.
run "robot_check_cascade" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "robot-check-cascade"
    namespace            = run.seed_image.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = run.seed_image.admin_secret_name
    command              = ["sh", "-c"]
    args = [<<-SH
      set -euo pipefail
      api=http://harbor-core.harbor.svc.cluster.local/api/v2.0
      # As in robot_check_update: a failed request or an answer that is
      # not a list fails the Job, never reads as "no robot".
      robots() {
        body=$(curl -fsS -m 10 -u "$username:$password" "$api/robots?page_size=100&q=$1") || return 1
        printf '%s' "$body" | jq -c 'if type == "array" then [.[].name] else error("Harbor did not answer with a robot list") end'
      }
      gone=$(robots name%3Dbridge-dev.app-ns.runner)
      all=$(robots name%3D~bridge-dev.)
      echo "robot of app-ns/runner: $gone; robots of cluster dev: $all"
      printf '%s' "$gone" | jq -e 'length == 0' >/dev/null
      printf '%s' "$all" | jq -e --argjson want ${length(run.harbor_access_cascade.harbor_accesses)} 'length == $want' >/dev/null
    SH
    ]
    timeout_seconds  = 120
    fail_message     = "namespace deletion released tenant-access without revoking its robot, or took other robots with it (or Harbor could not be asked; see pod.log)"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
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
      # away, but give Harbor's list a moment to reflect the delete. Only
      # an empty LIST passes: a failed request is retried and, if Harbor
      # never answers, fails the Job; an answer that is not exactly one
      # JSON list (an empty body included, which curl -f accepts on any
      # 2xx or 3xx) fails it at once. robot_check_update proved this query
      # lists the robots.
      state="Harbor was never asked"
      for i in $(seq 1 30); do
        if body=$(curl -fsS -m 10 -u "$username:$password" "$api/robots?page_size=100&q=name%3D~bridge-dev."); then
          left=$(printf '%s' "$body" | jq -rs 'if length == 1 and (.[0] | type) == "array" then .[0][].name else error("Harbor did not answer with one robot list") end')
          if [ -z "$left" ]; then echo "no robots of cluster dev left in Harbor"; exit 0; fi
          state="robots left behind: $left"
        else
          state="the Harbor query failed (curl exit $?)"
        fi
        sleep 3
      done
      echo "$state"; exit 1
    SH
    ]
    timeout_seconds  = 150
    fail_message     = "finalizer cleanup incomplete: robots of cluster dev are still in Harbor after every HarborAccess was deleted, or Harbor could not be asked (see pod.log)"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}
