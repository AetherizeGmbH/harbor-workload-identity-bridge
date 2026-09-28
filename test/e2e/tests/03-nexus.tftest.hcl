# End-to-end test of the Nexus backend (ADR-0036) on kind, next to Harbor.
# Run it with `make e2e-nexus`; `make e2e` and CI run 01 and 02 only.
#
# Stages (each `run` block is a separate plan+apply; runs that use the same
# module source share one OpenTofu state):
#   1. build_images         — bridge, plugin, seed, and the Nexus image
#                             (test/e2e/nexus: 3.76.1 pinned by digest)
#   2. cluster              — kind cluster "bridge-e2e-nexus"
#   3. harbor               — Harbor, as in 02 (Harbor stays required)
#   4. containerd_trust     — Harbor's cert into every node's containerd
#   5. nexus                — Nexus on a PVC; REST on 8081, one HTTP docker
#                             connector per repository behind NodePorts
#                             30851-30854 (nx-app, nx-other, nx-extra, nx-push)
#   6. containerd_nexus     — containerd reaches the connectors over HTTP
#   7. coredns_rewrite      — harbor.e2e and nexus.e2e resolvable in pods
#   8. seed_image           — Harbor: your-project with the test image
#   9. seed_nexus           — Nexus: realms, anonymous off, repositories,
#                             app:v1 in each, the bridge's least-privilege
#                             user (ADR-0036 decision j)
#  10. bridge_install       — the chart with nexus.enabled, both backends'
#                             registry hosts in matchImages
#  11. nexus_check_lib      — shell helpers the check Jobs share
#  12. nexus_access         — scenario phase "initial"
#  13. nexus_record         — records each checked object's Nexus user id
#  14. pull_nexus*          — kubelet pulls: granted repositories pull, an
#                             ungranted one fails in Nexus although the
#                             bridge issued the user (its audit line)
#  15. pull_harbor_routing  — the same ServiceAccount pulls from Harbor
#  16. nexus_routing        — the bridge answers that ServiceAccount's token
#                             with the Nexus user for a Nexus image, the
#                             Harbor robot for a Harbor image, and refuses
#                             an image of neither (no_backend);
#      robot_check_initial  — Harbor lists that robot under the query
#                             robot_check_teardown relies on
#  17. nexus_edit_baseline* — editor's credentials read both of its
#                             repositories; the bridge serves it
#  18. nexus_push           — the pull,push user pushes to its repository only
#  19. nexus_state_initial  — Nexus: one role and one user per identity
#  20. nexus_access_update  — scenario phase "updated"
#  21. nexus_update_state   — no rotation on a spec edit; editor is
#                             RepositoryNotFound; mover has the new identity
#  22. pull_nexus_revoked   — puller's removed repository fails (grant removal)
#  23. pull_nexus_kept      — puller's remaining repository still pulls
#  24. nexus_edit_revoked   — repository A is revoked at once for editor's
#                             existing password and bearer token
#  25. nexus_edit_refused   — the bridge refuses editor (grants incomplete)
#  26. nexus_state_updated  — Nexus: grants shrunk, old identity gone
#  27. nexus_rotation_forced    — editor's Secret deleted: a user of a new
#                                 generation, the old bearer token refused at
#                                 once, the RepositoryNotFound mark kept
#  28. nexus_rotation_scheduled — puller's rotation promise backdated: a new
#                                 user, the old one retired after the grace,
#                                 its bearer token refused
#  29. nexus_outage         — DeletionBlocked while Nexus is down; released
#                             once it is back
#  30. nexus_outage_state   — Nexus: the deleted object's user and role gone
#  31. file_sleep (opt-in)  — pause for kubectl-poking
#  32. nexus_access_teardown — scenario phase "none" while the bridge runs
#  33. nexus_teardown_check / robot_check_teardown — nothing of cluster "dev"
#                             left in Nexus or Harbor
#
# The rotation stages run after every kubelet pull of their identities:
# they retire users whose credentials kubelet may still cache.
#
# Nexus's connectors speak plain HTTP (containerd_nexus); Harbor keeps TLS.
# A failing Job or check writes diagnostics to test/e2e/.diag/<name>/.

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
      seed = {
        tag        = "e2e-seed:e2e"
        dockerfile = "Dockerfile"
        context    = "seed"
      }
      # Built for the host's platform on purpose; see test/e2e/nexus/Dockerfile.
      nexus = {
        tag        = "nexus-e2e:3.76.1"
        dockerfile = "Dockerfile"
        context    = "nexus"
      }
    }
  }
}

run "cluster" {
  module {
    source = "./modules/kind-cluster"
  }
  variables {
    name                = "bridge-e2e-nexus"
    worker_count        = 2
    http_port           = try(var.host_http_port, 8080)
    https_port          = try(var.host_https_port, 8443)
    api_server_port     = try(var.host_api_server_port, 6443)
    enable_cert_manager = true
    extra_etc_hosts     = ["127.0.0.1 harbor.e2e", "127.0.0.1 nexus.e2e"]
    images_to_load      = run.build_images.image_tags_list
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
    version_harbor    = try(var.version_harbor, null)
  }
}

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

run "nexus" {
  module {
    source = "./modules/nexus"
  }
  variables {
    kubeconfig = run.cluster.kubeconfig
    repositories = {
      "nx-app"   = { connector_port = 5001, node_port = 30851 }
      "nx-other" = { connector_port = 5002, node_port = 30852 }
      "nx-extra" = { connector_port = 5003, node_port = 30853 }
      "nx-push"  = { connector_port = 5004, node_port = 30854 }
    }
  }
}

run "containerd_nexus" {
  command = apply
  module {
    source = "./modules/containerd-registry-http"
  }
  variables {
    cluster_name        = run.cluster.name
    node_names          = run.cluster.node_names
    registry_host_ports = values(run.nexus.registry_hosts)
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
      "nexus.e2e"  = run.harbor.kind_node_ip
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
    projects       = ["your-project"]
  }
}

run "seed_nexus" {
  command = apply
  module {
    source = "./modules/nexus-seed"
  }
  variables {
    kubeconfig     = run.cluster.kubeconfig
    admin_password = run.nexus.admin_password
    nexus_url      = run.nexus.internal_url
    repositories   = run.nexus.repositories
  }
}

# Both backends: Harbor's host and every Nexus connector in matchImages
# (kubelet: exact port), and each backend's registry hosts named, so the
# data plane routes by host (and refuses an image of neither,
# nexus_routing).
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
    match_images          = concat(["harbor.e2e:30843"], values(run.nexus.registry_hosts))
    harbor_registry_hosts = ["harbor.e2e:30843"]
    nexus = {
      url            = run.nexus.internal_url
      registry_hosts = values(run.nexus.registry_hosts)
    }
    nexus_admin_username = run.seed_nexus.bridge_admin_username
    nexus_admin_password = run.seed_nexus.bridge_admin_password
    bridge_image         = run.build_images.image_refs.bridge
    plugin_image         = run.build_images.image_refs.plugin
  }
}

run "nexus_check_lib" {
  command = apply
  module {
    source = "./modules/nexus-check-lib"
  }
  variables {
    registry_hosts   = run.nexus.registry_hosts
    credentials_url  = run.bridge_install.credentials_url
    audience         = "harbor-bridge"
    bridge_namespace = run.bridge_install.namespace
    nexus_url        = run.nexus.internal_url
  }
}

run "nexus_access" {
  command = apply
  module {
    source = "./modules/nexus-access-scenario"
  }
  variables {
    kubeconfig       = run.cluster.kubeconfig
    phase            = "initial"
    bridge_namespace = run.bridge_install.namespace
    audience         = "harbor-bridge"
  }
}

# The Nexus user of each object the later stages check, for comparison
# after the spec edit. The status and the Secret must agree.
run "nexus_record" {
  command = apply
  module {
    source = "./modules/kubectl-check"
  }
  variables {
    kubeconfig_path = run.cluster.kubeconfig_path
    name            = "nexus-record"
    timeout_seconds = 60
    fail_message    = "a Ready NexusAccess has no Nexus user in its status, or its Secret names another"
    script          = <<-SH
      for o in nx-pull/puller nx-edit/editor; do
        ns=$${o%/*}; name=$${o#*/}
        id=$(k -n "$ns" get nexusaccess "$name" -o jsonpath='{.status.user.userId}')
        [ -n "$id" ] || fail "$o has no status.user.userId"
        sid=$(k -n "$BRIDGE_NS" get secret "nexususer-$ns.$name" -o jsonpath='{.metadata.annotations.nexus\.aetherize\.io/user-id}')
        [ "$id" = "$sid" ] || fail "$o: the status names user $id, its Secret $sid"
        echo "$id" > "$STATE_DIR/$ns.$name.user-id"
        echo "$o: user $id"
      done
    SH
  }
}

# Load-bearing: kubelet → plugin → bridge (Nexus route) → containerd pulls
# from Nexus with the identity's user.
run "pull_nexus" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-nexus-app"
    namespace            = "nx-pull"
    service_account_name = "puller"
    image                = "${run.nexus.registry_hosts["nx-app"]}/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo nx-pull/puller pulled nx-app; exit 0"]
    timeout_seconds      = 300
    fail_message         = "Nexus pull failed: kubelet → plugin → bridge → Nexus chain broken"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# The repository the edit removes later. Pulled now so that kubelet caches
# credentials for its connector, as a real node would.
run "pull_nexus_extra" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-nexus-extra"
    namespace            = "nx-pull"
    service_account_name = "puller"
    image                = "${run.nexus.registry_hosts["nx-extra"]}/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo nx-pull/puller pulled nx-extra; exit 0"]
    timeout_seconds      = 300
    fail_message         = "Nexus pull of a second granted repository failed"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# The bridge serves puller's user for any Nexus connector; Nexus itself
# must refuse the repository puller's role does not grant. The pull fails
# with an authorization error either way (anonymous access is off), so
# pull_nexus_ungranted_issued proves from the bridge's audit log that it
# issued puller's user for this image: the refusal came from Nexus.
run "pull_nexus_ungranted" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-nexus-other"
    namespace            = "nx-pull"
    service_account_name = "puller"
    image                = "${run.nexus.registry_hosts["nx-other"]}/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo SHOULD NOT RUN; exit 0"]
    timeout_seconds      = 240
    expect_pull_failure  = true
    fail_message         = "nx-pull/puller pulled nx-other, which its NexusAccess does not grant"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# expect_bridge_log cannot be combined with expect_pull_failure; this check
# reads the bridge's audit line for pull_nexus_ungranted's image instead.
run "pull_nexus_ungranted_issued" {
  command = apply
  module {
    source = "./modules/kubectl-check"
  }
  variables {
    kubeconfig_path = run.cluster.kubeconfig_path
    name            = "pull-nexus-ungranted-issued"
    timeout_seconds = 120
    fail_message    = "no audit line shows that the bridge issued nx-pull/puller's Nexus user for nx-other: pull_nexus_ungranted may have failed without credentials, not in Nexus (see check.log)"
    environment = {
      IMAGE = "${run.nexus.registry_hosts["nx-other"]}/app:v1"
    }
    script = <<-SH
      # Kubelet asks the credential provider for the image without its tag,
      # so the audit line carries IMAGE minus ":v1".
      # No grep -q in a pipeline: set -o pipefail would count an early exit
      # of it as a failure of the greps before.
      issued() {
        logs=$(k -n "$BRIDGE_NS" logs -l app.kubernetes.io/component=bridge --all-containers --tail=-1 --since=15m) || return 1
        hit=$(printf '%s\n' "$logs" | grep -F '"logger":"audit"' | grep -F '"msg":"credential issued"' \
          | grep -F '"access_kind":"nexus"' | grep -F '"nexusaccess":"nx-pull/puller"' \
          | grep -F "\"requested_image\":\"$${IMAGE%:*}\"" || true)
        [ -n "$hit" ]
      }
      retry 60 issued || fail "no 'credential issued' audit line of nx-pull/puller for $IMAGE"
      echo "the bridge issued nx-pull/puller's Nexus user for $IMAGE; Nexus refused the pull"
    SH
  }
}

# The same ServiceAccount through its HarborAccess: with Nexus configured,
# Harbor images still get Harbor credentials.
run "pull_harbor_routing" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-harbor-routing"
    namespace            = "nx-pull"
    service_account_name = "puller"
    image                = "harbor.e2e:30843/your-project/alpine:test3"
    command              = ["sh", "-c"]
    args                 = ["echo nx-pull/puller pulled from Harbor next to Nexus; exit 0"]
    timeout_seconds      = 300
    fail_message         = "Harbor pull failed with the Nexus backend enabled: routing sent a Harbor image elsewhere"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Routing at the data plane (ADR-0036 decision f): one pod-bound token of
# nx-pull/puller, three images. The answers must carry the Nexus user of
# the identity for the Nexus image and the Harbor robot for the Harbor
# image, and refuse an image of neither backend (no_backend): the Nexus
# connectors' host on a port no registry host names. Only the shape of the
# answers is checked; no password is printed.
run "nexus_routing" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-routing"
    namespace            = "nx-pull"
    service_account_name = "puller"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_field_refs = {
      POD_NAME = "metadata.name"
      POD_UID  = "metadata.uid"
      SA_NAME  = "spec.serviceAccountName"
    }
    command = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      identity=bridge-dev.nx-pull.puller
      bridge_ca || fail "no serving certificate from the bridge"
      tok=$(mint_pod_token) || fail "could not mint a pod-bound token"

      c=$(bridge_ask "$tok" "${run.nexus.registry_hosts["nx-app"]}/routing-check:nexus")
      [ "$c" = 200 ] || fail "Nexus image: HTTP $c, want 200: $(cat /tmp/bridge.json)"
      jq -e --arg id "$identity" '(.username | startswith($id + "_") and (ltrimstr($id + "_") | test("^[0-9a-f]{16}$")))
          and (.password | type == "string" and length > 0)
          and (.expires_in | type == "number" and . > 0 and . <= 3600) and .cache_key_type == "Registry"' /tmp/bridge.json >/dev/null \
        || { rm -f /tmp/bridge.json; fail "Nexus image: HTTP 200, but not with a Nexus user of $identity"; }
      nexus_user=$(jq -r .username /tmp/bridge.json); rm -f /tmp/bridge.json

      c=$(bridge_ask "$tok" "harbor.e2e:30843/your-project/routing-check:harbor")
      [ "$c" = 200 ] || fail "Harbor image: HTTP $c, want 200: $(cat /tmp/bridge.json)"
      jq -e --arg id "$identity" '(.username | endswith($id)) and (.password | type == "string" and length > 0)
          and .cache_key_type == "Registry"' /tmp/bridge.json >/dev/null \
        || { rm -f /tmp/bridge.json; fail "Harbor image: HTTP 200, but not with the Harbor robot of $identity"; }
      harbor_user=$(jq -r .username /tmp/bridge.json); rm -f /tmp/bridge.json

      c=$(bridge_ask "$tok" "${split(":", run.nexus.registry_hosts["nx-app"])[0]}:30899/routing-check:none")
      if [ "$c" = 200 ]; then rm -f /tmp/bridge.json; fail "image of no backend: the bridge issued credentials"; fi
      [ "$c" = 403 ] || fail "image of no backend: HTTP $c, want 403: $(cat /tmp/bridge.json)"
      rm -f /tmp/bridge.json

      echo "routing: Nexus image → $nexus_user, Harbor image → $harbor_user, image of no backend refused"
    SH
    ]
    expect_bridge_log = [
      ["\"logger\":\"audit\"", "\"msg\":\"credential issued\"", "\"requested_image\":\"${run.nexus.registry_hosts["nx-app"]}/routing-check:nexus\""],
      ["\"logger\":\"audit\"", "\"msg\":\"credential issued\"", "\"requested_image\":\"harbor.e2e:30843/your-project/routing-check:harbor\"", "\"harboraccess\":\"${run.bridge_install.namespace}/puller\""],
      ["\"logger\":\"audit\"", "\"msg\":\"credential denied\"", "\"access_kind\":\"none\"", "\"reason\":\"no_backend\"", "\"requested_image\":\"${split(":", run.nexus.registry_hosts["nx-app"])[0]}:30899/routing-check:none\""],
    ]
    timeout_seconds  = 180
    fail_message     = "data-plane routing: a Nexus image did not get the identity's Nexus user, a Harbor image not its Harbor robot, or an image of no backend got credentials"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Harbor holds puller's robot, and the fuzzy query robot_check_teardown
# uses lists exactly that one robot of cluster "dev". Without this, the
# empty list that stage accepts could come from a query that lists nothing.
run "robot_check_initial" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "robot-check-initial"
    namespace            = run.seed_image.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = run.seed_image.admin_secret_name
    command              = ["sh", "-c"]
    args = [<<-SH
      set -euo pipefail
      api=http://harbor-core.harbor.svc.cluster.local/api/v2.0
      # A failed request, or an answer that is not exactly one JSON list (an
      # empty body included, which curl -f accepts on any 2xx or 3xx), fails
      # the Job.
      body=$(curl -fsS -m 10 -u "$username:$password" "$api/robots?page_size=100&q=name%3D~bridge-dev.")
      all=$(printf '%s' "$body" | jq -cs 'if length == 1 and (.[0] | type) == "array" then [.[0][].name] else error("Harbor did not answer with one robot list") end')
      echo "robots of cluster dev: $all"
      printf '%s' "$all" | jq -e 'length == 1 and (.[0] | endswith("bridge-dev.nx-pull.puller"))' >/dev/null
    SH
    ]
    timeout_seconds  = 120
    fail_message     = "Harbor does not list exactly puller's robot under the fuzzy name=~bridge-dev. query robot_check_teardown relies on (or Harbor could not be asked; see pod.log)"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Baseline for the edit stages: editor's credentials (the Secret the
# bridge wrote) get a bearer token that reads nx-app and nx-extra, from
# nx-app's connector, and is refused on nx-other. nexus_edit_revoked later
# relies on one token per user across connectors.
run "nexus_edit_baseline" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-edit-baseline"
    namespace            = run.bridge_install.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = "nexususer-nx-edit.editor"
    command              = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      t=$(docker_token nx-app "$username" "$password") || fail "no docker token for editor's user"
      c=$(manifest_code nx-app "$t"); [ "$c" = 200 ] || fail "nx-app with editor's token: HTTP $c, want 200"
      c=$(manifest_code nx-extra "$t"); [ "$c" = 200 ] || fail "nx-extra with the token nx-app's connector issued: HTTP $c, want 200"
      c=$(manifest_code nx-other "$t"); refused "$c" || fail "nx-other, not granted, with editor's token: HTTP $c, want 401 or 403"
      echo "editor's bearer token reads nx-app and nx-extra and is refused on nx-other"
    SH
    ]
    timeout_seconds  = 120
    fail_message     = "editor's credentials do not read exactly its repositories"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# The data plane serves editor while its grants are complete; the refusal
# in nexus_edit_refused is then the edit's doing.
run "nexus_edit_baseline_bridge" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-edit-baseline-bridge"
    namespace            = "nx-edit"
    service_account_name = "editor"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_field_refs = {
      POD_NAME = "metadata.name"
      POD_UID  = "metadata.uid"
      SA_NAME  = "spec.serviceAccountName"
    }
    command = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      bridge_ca || fail "no serving certificate from the bridge"
      tok=$(mint_pod_token) || fail "could not mint a pod-bound token"
      c=$(bridge_ask "$tok" "${run.nexus.registry_hosts["nx-app"]}/edit-check:baseline")
      [ "$c" = 200 ] || fail "HTTP $c, want 200: $(cat /tmp/bridge.json)"
      jq -e '.username | startswith("bridge-dev.nx-edit.editor_")' /tmp/bridge.json >/dev/null \
        || { rm -f /tmp/bridge.json; fail "HTTP 200, but not with editor's Nexus user"; }
      rm -f /tmp/bridge.json
      echo "the bridge serves editor's Nexus user"
    SH
    ]
    timeout_seconds  = 120
    fail_message     = "the bridge does not serve editor's Nexus user while its grants are complete"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# pull,push (ADR-0036 decision b): the user of nx-ci/pusher pushes a new
# tag to nx-push and reads it back, and is refused on nx-app, which its
# NexusAccess does not name.
run "nexus_push" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-push"
    namespace            = run.bridge_install.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = "nexususer-${run.bridge_install.namespace}.pusher"
    command              = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      push=$(reg nx-push); push=$${push#http://}
      app=$(reg nx-app); app=$${app#http://}
      crane auth login "$push" -u "$username" -p "$password"
      crane auth login "$app" -u "$username" -p "$password"
      crane copy --insecure "$push/app:v1" "$push/pushed-by-bridge-user:v1"
      crane digest --insecure "$push/pushed-by-bridge-user:v1" >/dev/null
      if crane copy --insecure "$push/app:v1" "$app/pushed-by-bridge-user:v1" 2>/tmp/err; then
        fail "the pull,push user of nx-push pushed to nx-app"
      fi
      grep -Eqi '401|403|unauthori[sz]ed|denied|forbidden' /tmp/err \
        || fail "the push to nx-app failed, but not with an authorization error: $(cat /tmp/err)"
      if crane digest --insecure "$app/app:v1" 2>/tmp/err; then
        fail "the pull,push user of nx-push read nx-app"
      fi
      echo "pusher's user pushes to nx-push and is refused on nx-app"
    SH
    ]
    timeout_seconds  = 300
    fail_message     = "pull,push: the NexusAccess user could not push to its repository, or reached another one"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Nexus itself (admin credentials from the seed namespace): one role and
# one user per identity, with the markers and privileges of ADR-0036
# decision b.
run "nexus_state_initial" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-state-initial"
    namespace            = run.seed_nexus.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = run.seed_nexus.admin_secret_name
    command              = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      nexus_state || fail "could not read Nexus's users and roles"
      identity bridge-dev.nx-pull.puller nx-pull/puller nx-repository-view-docker-nx-app-read nx-repository-view-docker-nx-extra-read
      identity bridge-dev.nx-ci.pusher ${run.bridge_install.namespace}/pusher nx-repository-view-docker-nx-push-add nx-repository-view-docker-nx-push-edit nx-repository-view-docker-nx-push-read
      identity bridge-dev.nx-edit.editor nx-edit/editor nx-repository-view-docker-nx-app-read nx-repository-view-docker-nx-extra-read
      identity bridge-dev.nx-move.old-sa nx-move/mover nx-repository-view-docker-nx-app-read
      echo "Nexus holds one role and one user per identity"
    SH
    ]
    timeout_seconds  = 120
    fail_message     = "Nexus state after phase initial is wrong (see pod.log)"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# ── Lifecycle: puller loses nx-extra, editor swaps nx-extra (A) for the
# missing nx-missing (C), mover moves to a new ServiceAccount. The phase
# waits for puller and mover at generation 2; editor never gets there.
run "nexus_access_update" {
  command = apply
  module {
    source = "./modules/nexus-access-scenario"
  }
  variables {
    kubeconfig       = run.cluster.kubeconfig
    phase            = "updated"
    bridge_namespace = run.bridge_install.namespace
    audience         = "harbor-bridge"
  }
}

# The control plane's side of the edit:
#   - a spec edit creates no user (ADR-0023: no rotation on an edit): the
#     users recorded by nexus_record are still the ones in the status and
#     the Secrets, and no Secret names a retiring user;
#   - puller is complete again: no grants-incomplete mark;
#   - editor is Ready=False RepositoryNotFound naming nx-missing, its
#     observedGeneration stays 1, and its Secret carries the mark the data
#     plane refuses on (ADR-0036 decision d);
#   - mover's user belongs to the new identity.
run "nexus_update_state" {
  command = apply
  module {
    source = "./modules/kubectl-check"
  }
  variables {
    kubeconfig_path = run.cluster.kubeconfig_path
    name            = "nexus-update-state"
    timeout_seconds = 240
    fail_message    = "the control plane's state after the spec edits is wrong (see check.log)"
    script          = <<-SH
      ann() { k -n "$BRIDGE_NS" get secret "$1" -o jsonpath="{.metadata.annotations.nexus\.aetherize\.io/$2}"; }
      ready() { k -n "$1" get nexusaccess "$2" -o jsonpath="{.status.conditions[?(@.type==\"Ready\")].$3}"; }

      for o in nx-pull/puller nx-edit/editor; do
        ns=$${o%/*}; name=$${o#*/}
        want=$(cat "$STATE_DIR/$ns.$name.user-id")
        got=$(k -n "$ns" get nexusaccess "$name" -o jsonpath='{.status.user.userId}')
        [ "$got" = "$want" ] || fail "$o: user $got after the spec edit, want still $want: an edit must not rotate"
        sid=$(ann "nexususer-$ns.$name" user-id)
        [ "$sid" = "$want" ] || fail "$o: the Secret holds user $sid after the spec edit, want still $want"
        retiring=$(ann "nexususer-$ns.$name" retiring-user-id)
        [ -z "$retiring" ] || fail "$o: the Secret retires user $retiring after a spec edit"
      done

      gi=$(ann nexususer-nx-pull.puller grants-incomplete)
      [ -z "$gi" ] || fail "puller's Secret is marked grants-incomplete ($gi) although every repository it names exists"

      rnf() { [ "$(ready nx-edit editor reason)" = RepositoryNotFound ]; }
      retry 180 rnf || fail "editor: Ready reason $(ready nx-edit editor reason), want RepositoryNotFound"
      [ "$(ready nx-edit editor status)" = False ] || fail "editor: Ready is $(ready nx-edit editor status), want False"
      msg=$(ready nx-edit editor message)
      case "$msg" in *nx-missing*) ;; *) fail "editor's Ready message does not name nx-missing: $msg" ;; esac
      og=$(k -n nx-edit get nexusaccess editor -o jsonpath='{.status.observedGeneration}')
      gen=$(k -n nx-edit get nexusaccess editor -o jsonpath='{.metadata.generation}')
      [ "$gen" = 2 ] || fail "editor: generation $gen, want 2"
      [ "$og" = 1 ] || fail "editor: observedGeneration $og, want 1 (not advanced while a repository is missing)"
      gi=$(ann nexususer-nx-edit.editor grants-incomplete)
      [ "$gi" = nx-missing ] || fail "editor's Secret: grants-incomplete '$gi', want 'nx-missing'"

      id=$(k -n nx-move get nexusaccess mover -o jsonpath='{.status.user.userId}')
      case "$id" in bridge-dev.nx-move.new-sa_*) ;; *) fail "mover's user is '$id', want a user of bridge-dev.nx-move.new-sa" ;; esac

      echo "no rotation on the edits; editor RepositoryNotFound ($msg); mover now $id"
    SH
  }
}

# Grant removal: puller's role lost nx-extra. kubelet on the node that ran
# pull_nexus_extra may still cache puller's credentials for that connector
# (the password did not change), so this passes only if Nexus itself
# refuses the repository.
run "pull_nexus_revoked" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-nexus-extra-revoked"
    namespace            = "nx-pull"
    service_account_name = "puller"
    image                = "${run.nexus.registry_hosts["nx-extra"]}/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo SHOULD NOT RUN; exit 0"]
    timeout_seconds      = 240
    expect_pull_failure  = true
    fail_message         = "grant removal failed: nx-pull/puller still pulls nx-extra after its NexusAccess dropped it"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# … while the repository it kept still pulls: the edit changed the role,
# not the user or its password.
run "pull_nexus_kept" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "pull-nexus-app-kept"
    namespace            = "nx-pull"
    service_account_name = "puller"
    image                = "${run.nexus.registry_hosts["nx-app"]}/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo nx-pull/puller still pulls nx-app after the edit; exit 0"]
    timeout_seconds      = 300
    fail_message         = "the spec edit broke the repository puller kept"
    node_log_command     = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# ADR-0036 decision d: the edit that removed nx-extra (A) and named the
# missing nx-missing (C) revokes A at once for editor's existing password
# and bearer token (one persistent token per user, ADR-0036 Context 1),
# while nx-app, still named and existing, stays readable.
run "nexus_edit_revoked" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-edit-revoked"
    namespace            = run.bridge_install.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = "nexususer-nx-edit.editor"
    command              = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      t=$(docker_token nx-app "$username" "$password") || fail "editor's unchanged password gets no docker token"
      extra_refused() { refused "$(manifest_code nx-extra "$t")"; }
      retry 60 extra_refused || fail "nx-extra: HTTP $(manifest_code nx-extra "$t") with editor's existing token, want 401 or 403"
      c=$(manifest_code nx-app "$t"); [ "$c" = 200 ] || fail "nx-app: HTTP $c with editor's existing token, want 200"
      echo "the edit revoked nx-extra for editor's existing credentials; nx-app stays"
    SH
    ]
    timeout_seconds  = 180
    fail_message     = "ADR-0036 decision d: removing a repository while naming a missing one did not revoke it at once, or revoked more"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# … and the data plane issues nothing for editor while its grants are
# incomplete: 403, and the audit log says so.
run "nexus_edit_refused" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-edit-refused"
    namespace            = "nx-edit"
    service_account_name = "editor"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_field_refs = {
      POD_NAME = "metadata.name"
      POD_UID  = "metadata.uid"
      SA_NAME  = "spec.serviceAccountName"
    }
    command = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      bridge_ca || fail "no serving certificate from the bridge"
      tok=$(mint_pod_token) || fail "could not mint a pod-bound token"
      c=$(bridge_ask "$tok" "${run.nexus.registry_hosts["nx-app"]}/edit-check:refused")
      if [ "$c" = 200 ]; then rm -f /tmp/bridge.json; fail "the bridge issued credentials for a NexusAccess whose grants are incomplete"; fi
      [ "$c" = 403 ] || fail "HTTP $c, want 403: $(cat /tmp/bridge.json)"
      echo "the bridge refuses editor: $(cat /tmp/bridge.json)"
    SH
    ]
    expect_bridge_log = [
      ["\"logger\":\"audit\"", "\"msg\":\"credential denied\"", "\"access_kind\":\"nexus\"", "\"reason\":\"grants_incomplete\"", "\"nexusaccess\":\"nx-edit/editor\"", "\"missing_repositories\":\"nx-missing\"", "\"requested_image\":\"${run.nexus.registry_hosts["nx-app"]}/edit-check:refused\""],
    ]
    timeout_seconds  = 120
    fail_message     = "ADR-0036 decision f: the bridge did not refuse a NexusAccess whose Secret is marked grants-incomplete"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Nexus after the edits: puller's and editor's roles hold nx-app only (no
# nx-extra, nothing of nx-missing), each identity still has one user, and
# the old identity of mover has neither role nor user.
run "nexus_state_updated" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-state-updated"
    namespace            = run.seed_nexus.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = run.seed_nexus.admin_secret_name
    command              = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      nexus_state || fail "could not read Nexus's users and roles"
      identity bridge-dev.nx-pull.puller nx-pull/puller nx-repository-view-docker-nx-app-read
      identity bridge-dev.nx-edit.editor nx-edit/editor nx-repository-view-docker-nx-app-read
      identity bridge-dev.nx-ci.pusher ${run.bridge_install.namespace}/pusher nx-repository-view-docker-nx-push-add nx-repository-view-docker-nx-push-edit nx-repository-view-docker-nx-push-read
      identity bridge-dev.nx-move.new-sa nx-move/mover nx-repository-view-docker-nx-app-read
      gone bridge-dev.nx-move.old-sa
      echo "Nexus reflects the edits"
    SH
    ]
    timeout_seconds  = 120
    fail_message     = "Nexus state after phase updated is wrong (see pod.log)"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# ── Rotation replaces the user under a new id (ADR-0036 decision c).
#
# Forced: editor's Secret is deleted. The reconciler creates a user of a
# new generation and deletes the previous one at once (implementation note
# 4). editor is RepositoryNotFound: rotation does not depend on the grants
# (decision c), and the new Secret keeps the mark. The previous user's
# bearer token and password must be refused.
run "nexus_rotation_forced" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-rotation-forced"
    namespace            = run.bridge_install.namespace
    service_account_name = "nexus-check"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = "nexususer-nx-edit.editor"
    command              = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      secret=nexususer-nx-edit.editor
      old_user=$username
      old_token=$(docker_token nx-app "$old_user" "$password") || fail "no docker token for the current user"
      c=$(manifest_code nx-app "$old_token"); [ "$c" = 200 ] || fail "nx-app with the current token: HTTP $c, want 200"

      c=$(kapi DELETE "$(secret_path $secret)"); rm -f /tmp/kapi.json
      [ "$c" = 200 ] || fail "delete $secret: HTTP $c"
      new_user() {
        [ "$(kapi GET "$(secret_path $secret)")" = 200 ] || return 1
        _n=$(ann nexus.aetherize.io/user-id)
        [ -n "$_n" ] && [ "$_n" != "$old_user" ] && [ "$(data username)" = "$_n" ] && [ -n "$(data password)" ]
      }
      retry 180 new_user || fail "no Secret with a new user within 180 s of deleting it"
      user=$(data username); pass=$(data password)
      marked=$(ann nexus.aetherize.io/grants-incomplete); retiring=$(ann nexus.aetherize.io/retiring-user-id)
      rm -f /tmp/kapi.json
      case "$user" in bridge-dev.nx-edit.editor_*) ;; *) fail "the new user $user is not one of editor's identity" ;; esac
      [ "$marked" = nx-missing ] || fail "the rotated Secret lost the grants-incomplete mark (got '$marked')"
      [ -z "$retiring" ] || fail "a forced rotation kept the previous user $retiring for a grace"

      old_refused() { [ "$(v2_code nx-app "$old_token")" = 401 ]; }
      retry 60 old_refused || fail "the previous user's bearer token still authenticates after the forced rotation"
      c=$(token_code nx-app "$old_user" "$password"); rm -f /tmp/nx-token.json
      [ "$c" = 401 ] || fail "the previous user's password: HTTP $c, want 401"
      new_token=$(docker_token nx-app "$user" "$pass") || fail "no docker token for the new user"
      [ "$new_token" != "$old_token" ] || fail "the new user got the previous user's token"
      c=$(manifest_code nx-app "$new_token"); [ "$c" = 200 ] || fail "nx-app with the new user's token: HTTP $c, want 200"
      c=$(manifest_code nx-extra "$new_token"); refused "$c" || fail "nx-extra with the new user's token: HTTP $c, want 401 or 403"
      echo "forced rotation: $old_user → $user; the old bearer token and password are refused"
    SH
    ]
    timeout_seconds  = 300
    fail_message     = "forced rotation (Secret deleted) did not replace the user under a new id, or left the old bearer token valid"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Scheduled: puller's rotation promise is backdated, as if a day had
# passed (ADR-0023: the reconciler rotates after rotation-not-before plus
# the safety margin). The new user's credentials work at once; the
# previous user stays for NexusUserRetireGrace (5 minutes, implementation
# note 4) and is then deleted, which ends its bearer token. Every
# /v2/token call of a user returns the same token (ADR-0036 Context 1).
run "nexus_rotation_scheduled" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-rotation-scheduled"
    namespace            = run.bridge_install.namespace
    service_account_name = "nexus-check"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = "nexususer-nx-pull.puller"
    command              = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      secret=nexususer-nx-pull.puller
      old_user=$username
      old_token=$(docker_token nx-app "$old_user" "$password") || fail "no docker token for the current user"
      again=$(docker_token nx-app "$old_user" "$password") || fail "no second docker token for the current user"
      [ "$again" = "$old_token" ] || fail "Nexus returned another docker token for the same user: ADR-0036 Context 1 no longer holds"
      c=$(manifest_code nx-app "$old_token"); [ "$c" = 200 ] || fail "nx-app with the current token: HTTP $c, want 200"

      c=$(kapi PATCH "$(secret_path $secret)" application/merge-patch+json \
        '{"metadata":{"annotations":{"harbor.aetherize.io/rotation-not-before":"2000-01-01T00:00:00Z"}}}')
      rm -f /tmp/kapi.json
      [ "$c" = 200 ] || fail "backdate rotation-not-before: HTTP $c"
      new_user() {
        [ "$(kapi GET "$(secret_path $secret)")" = 200 ] || return 1
        _n=$(ann nexus.aetherize.io/user-id)
        [ -n "$_n" ] && [ "$_n" != "$old_user" ] && [ "$(data username)" = "$_n" ] && [ -n "$(data password)" ]
      }
      retry 180 new_user || fail "no new user within 180 s of the rotation promise passing"
      user=$(data username); pass=$(data password)
      retiring=$(ann nexus.aetherize.io/retiring-user-id); after=$(ann nexus.aetherize.io/retire-after)
      rm -f /tmp/kapi.json
      case "$user" in bridge-dev.nx-pull.puller_*) ;; *) fail "the new user $user is not one of puller's identity" ;; esac
      [ "$retiring" = "$old_user" ] || fail "the Secret retires '$retiring', want the previous user $old_user"
      [ -n "$after" ] || fail "the Secret names no retire-after"

      c=$(manifest_code nx-app "$old_token"); [ "$c" = 200 ] || fail "the previous user's token failed within the grace: HTTP $c"
      new_token=$(docker_token nx-app "$user" "$pass") || fail "no docker token for the new user"
      [ "$new_token" != "$old_token" ] || fail "the new user got the previous user's token"
      c=$(manifest_code nx-app "$new_token"); [ "$c" = 200 ] || fail "nx-app with the new user's token: HTTP $c, want 200"
      echo "scheduled rotation: $old_user → $user; the previous user retires after $after"

      old_refused() { [ "$(v2_code nx-app "$old_token")" = 401 ]; }
      retry 480 old_refused || fail "the previous user's bearer token still authenticates 8 minutes after the rotation (retire-after $after)"
      c=$(token_code nx-app "$old_user" "$password"); rm -f /tmp/nx-token.json
      [ "$c" = 401 ] || fail "the previous user's password: HTTP $c, want 401"
      cleared() { [ "$(kapi GET "$(secret_path $secret)")" = 200 ] && [ -z "$(ann nexus.aetherize.io/retiring-user-id)" ]; }
      retry 60 cleared || fail "the Secret still names a retiring user after the retirement"
      rm -f /tmp/kapi.json
      c=$(manifest_code nx-app "$new_token"); [ "$c" = 200 ] || fail "nx-app with the new user's token after the retirement: HTTP $c, want 200"
      echo "the previous user is retired: its bearer token and password are refused"
    SH
    ]
    timeout_seconds  = 720
    fail_message     = "scheduled rotation did not replace the user under a new id, or the retired user's bearer token stayed valid"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# ── DeletionBlocked while Nexus is down (ADR-0036 decision i). The check
# creates its own NexusAccess, waits until it is Ready, scales Nexus to
# zero, deletes the object and expects Ready=False DeletionBlocked with the
# finalizer held; then it scales Nexus back and expects the bridge to
# release the object without anyone touching it. Nexus keeps its data on
# the PVC. The EXIT trap scales Nexus back up if the check fails half-way
# (kubectl-check turns its own timeout into an exit too).
run "nexus_outage" {
  command = apply
  module {
    source = "./modules/kubectl-check"
  }
  variables {
    kubeconfig_path = run.cluster.kubeconfig_path
    name            = "nexus-outage"
    # Above the sum of the script's own waits (2340 s), so a stuck step
    # fails with its own message.
    timeout_seconds = 2400
    fail_message    = "DeletionBlocked: the bridge released a NexusAccess while Nexus was down, did not report DeletionBlocked, or did not release it once Nexus was back"
    environment = {
      NEXUS_NS       = run.nexus.namespace
      NEXUS_DEPLOY   = run.nexus.deployment_name
      NEXUS_SELECTOR = run.nexus.pod_selector
    }
    script = <<-SH
      k apply -f - <<'YAML'
      apiVersion: nexus.aetherize.io/v1alpha1
      kind: NexusAccess
      metadata:
        name: outage-probe
        namespace: nx-outage
      spec:
        serviceAccountRef: {namespace: nx-outage, name: probe}
        trustPolicy: {issuer: "https://kubernetes.default.svc.cluster.local", audience: harbor-bridge}
        repositories:
          - {name: nx-app, access: pull, format: docker}
        tokenTTL: 1h0m0s
      YAML
      probe() { k -n nx-outage get nexusaccess outage-probe -o jsonpath="$1"; }
      ready() { [ "$(probe '{.status.observedGeneration}')" = 1 ]; }
      retry 180 ready || fail "outage-probe never became Ready: $(probe '{.status.conditions}')"
      echo "outage-probe Ready with user $(probe '{.status.user.userId}')"

      restore() { k -n "$NEXUS_NS" scale deployment "$NEXUS_DEPLOY" --replicas=1 >/dev/null 2>&1 || true; }
      trap restore EXIT
      k -n "$NEXUS_NS" scale deployment "$NEXUS_DEPLOY" --replicas=0
      nopods() { [ -z "$(k -n "$NEXUS_NS" get pods -l "$NEXUS_SELECTOR" -o name)" ]; }
      retry 180 nopods || fail "the Nexus pod did not go away"
      echo "Nexus is down"

      k -n nx-outage delete nexusaccess outage-probe --wait=false
      blocked() {
        k -n nx-outage get nexusaccess outage-probe >/dev/null 2>&1 \
          || fail "outage-probe is gone although Nexus is down: the finalizer was released without the revocation"
        [ "$(probe '{.status.conditions[?(@.type=="Ready")].reason}')" = DeletionBlocked ]
      }
      retry 180 blocked || fail "no DeletionBlocked while Nexus is down: $(probe '{.status.conditions}')"
      finalizers=$(probe '{.metadata.finalizers}')
      case "$finalizers" in *nexus.aetherize.io/user*) ;; *) fail "the finalizer went while Nexus was down: $finalizers" ;; esac
      echo "DeletionBlocked: $(probe '{.status.conditions[?(@.type=="Ready")].message}')"

      k -n "$NEXUS_NS" scale deployment "$NEXUS_DEPLOY" --replicas=1
      k -n "$NEXUS_NS" rollout status deployment "$NEXUS_DEPLOY" --timeout=900s
      echo "Nexus is back"
      released() { ! k -n nx-outage get nexusaccess outage-probe >/dev/null 2>&1; }
      retry 900 released || fail "outage-probe was not released within 15 minutes of Nexus coming back: $(probe '{.status.conditions}')"
      echo "the bridge released outage-probe once Nexus was reachable again"
    SH
  }
}

# The release came after the revocation: nothing of the probe's identity
# is left in Nexus.
run "nexus_outage_state" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-outage-state"
    namespace            = run.seed_nexus.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = run.seed_nexus.admin_secret_name
    command              = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      nexus_state || fail "could not read Nexus's users and roles"
      gone bridge-dev.nx-outage.probe
      echo "the deleted NexusAccess left no user or role"
    SH
    ]
    timeout_seconds  = 120
    fail_message     = "the NexusAccess deleted during the outage left its user or role in Nexus"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

run "file_sleep" {
  command = apply
  module {
    source = "./modules/test-sleep"
  }
  variables {
    enabled = try(var.pause_after_pull, false)
  }
}

# ── Lifecycle: every NexusAccess and HarborAccess (and their namespaces)
# deleted while the bridge runs; each deletion waits for the finalizer.
run "nexus_access_teardown" {
  command = apply
  module {
    source = "./modules/nexus-access-scenario"
  }
  variables {
    kubeconfig       = run.cluster.kubeconfig
    phase            = "none"
    bridge_namespace = run.bridge_install.namespace
    audience         = "harbor-bridge"
  }
}

# The finalizers revoked, not just let go: no user or role of cluster
# "dev" is left in Nexus, and the bridge's own Nexus user, which the
# janitor must never touch, is still there.
run "nexus_teardown_check" {
  command = apply
  module {
    source = "./modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.cluster.kubeconfig
    name                 = "nexus-teardown-check"
    namespace            = run.seed_nexus.namespace
    service_account_name = "default"
    image                = "e2e-seed:e2e"
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = run.seed_nexus.admin_secret_name
    command              = ["sh", "-c"]
    args = [<<-SH
      ${run.nexus_check_lib.sh}
      empty() {
        nexus_state || return 1
        [ "$(jq length /tmp/users.json)" = 0 ] && [ "$(jq length /tmp/roles.json)" = 0 ]
      }
      retry 60 empty || fail "left in Nexus: users $(jq -c '[.[].userId]' /tmp/users.json), roles $(jq -c '[.[].id]' /tmp/roles.json)"
      c=$(nx GET "/v1/security/users?source=default&userId=hwib-e2e-"); [ "$c" = 200 ] || fail "list users: HTTP $c"
      jq -e '[.[] | select(.userId | startswith("hwib-e2e-"))] | length == 1' /tmp/nx.json >/dev/null \
        || fail "the bridge's own Nexus user is gone"
      c=$(nx GET "/v1/security/roles/${run.seed_nexus.bridge_admin_role}?source=default")
      [ "$c" = 200 ] || fail "the bridge's own Nexus role is gone: HTTP $c"
      echo "no user or role of cluster dev left in Nexus; the bridge's own user and role are intact"
    SH
    ]
    timeout_seconds  = 180
    fail_message     = "finalizer cleanup incomplete: users or roles of cluster dev are still in Nexus after every NexusAccess was deleted"
    node_log_command = "docker exec {node} journalctl -u kubelet --no-pager --since -20min"
  }
}

# Harbor too: puller's HarborAccess went with the teardown.
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
      # 2xx or 3xx) fails it at once. robot_check_initial proved this query
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
