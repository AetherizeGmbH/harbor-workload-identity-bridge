# End-to-end test on a REAL GKE cluster (ADR-0022). Never runs in CI —
# it creates billed resources in the operator's project; run it via
# `make e2e-gke` with GOOGLE_PROJECT set. It has NOT been run yet.
#
# What this proves beyond the kind harness: GKE node images already run
# kubelet with --image-credential-provider-* flags for the out-of-tree
# GCP provider, so `plugin.install.mode: auto` must resolve to MERGE —
# our provider entry is injected into GKE's existing config without
# touching kubelet flags (ADR-0021) — and GKE's own provider must keep
# working afterwards. On top, the same lifecycle stages as the kind
# harness run against GKE (grant change, ServiceAccount change,
# deletion with finalizers, Harbor checked directly, unbound and long-lived
# tokens refused).
#
# Differences from kind:
#   - images go to an ephemeral Artifact Registry repo (no `kind load`);
#   - Harbor is a LoadBalancer on a pre-allocated IP, hostname
#     harbor.<ip>.sslip.io (resolves publicly for nodes AND pods — GKE
#     runs kube-dns, so the kind CoreDNS rewrite has no equivalent);
#   - containerd trusts Harbor's self-signed cert through a TEST-ONLY
#     DaemonSet (whose AR-hosted image pull is also the pre-install smoke
#     test of GKE's own credential provider);
#   - the ServiceAccount token issuer is GKE's container.googleapis.com
#     URL, read from the cluster's discovery document; the bridge fetches
#     the signing keys from the in-cluster apiserver (oidc_jwks_url);
#   - no kubelet journal in the failure diagnostics (no node access).
#
# Teardown order: as in the kind harness, harbor_access_teardown empties
# the HarborAccess state while the bridge still runs, so cleanup never
# waits on finalizers of an uninstalled bridge.
#
# Runtime findings to fold back into ADR-0022 after the first real run:
# the discovered GKE provider config path/format (installer logs show
# it), whether GKE containerd ships a config_path (trust DS logs), and
# whether the loopback NodePort routes under Dataplane V2 — if pull_pod
# fails with the plugin unable to reach the bridge, set bridge_endpoint =
# "https://$(NODE_IP):31443" on bridge_install/bridge_upgrade (see the R4
# note in ADR-0022 about the cert SAN implications).
#
# Variables: inside a run block `var` holds only CLI/TF_VAR values, so
# every optional one is read with try(var.x, <default>).

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
    zone           = try(var.gcp_zone, "europe-west3-a")
    version_prefix = try(var.gke_version_prefix, "1.34.")
    machine_type   = try(var.machine_type, "e2-standard-4")
    node_count     = try(var.node_count, 2)
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
    version_harbor    = try(var.version_harbor, null)
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

run "seed_image" {
  command = apply
  module {
    source = "../e2e/modules/harbor-seed"
  }
  variables {
    kubeconfig     = run.gke.kubeconfig
    admin_password = run.harbor.admin_password
    registry_host  = run.gke.harbor_hostname
    seed_image     = run.push.image_tags.seed
    projects = [
      "your-project", "project-alpha", "project-beta", "project-gamma",
      "beta-1", "beta-2", "beta-3", "upgrade-only",
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
    oidc_issuer           = run.gke.oidc_issuer
    oidc_jwks_url         = "https://kubernetes.default.svc/openid/v1/jwks"
    token_command         = "gcloud auth print-access-token"
    # install_mode stays "auto" — on GKE it MUST resolve to merge. If it
    # resolved to patch, the installer fails loudly and helm's DaemonSet
    # rollout wait surfaces it here. upgrade-only is added by bridge_upgrade.
    match_images = [
      "${run.gke.harbor_hostname}/your-project",
      "${run.gke.harbor_hostname}/project-alpha",
      "${run.gke.harbor_hostname}/project-beta",
      "${run.gke.harbor_hostname}/project-gamma",
      "${run.gke.harbor_hostname}/beta-1",
      "${run.gke.harbor_hostname}/beta-2",
      "${run.gke.harbor_hostname}/beta-3",
    ]
    bridge_image = run.push.image_refs.bridge
    plugin_image = run.push.image_refs.plugin
  }
}

run "harbor_access" {
  command = apply
  module {
    source = "../e2e/modules/harbor-access-scenario"
  }
  variables {
    kubeconfig       = run.gke.kubeconfig
    phase            = "initial"
    bridge_namespace = run.bridge_install.namespace
    issuer           = run.gke.oidc_issuer
    audience         = "harbor-bridge"
  }
}

# Audit H1 / ADR-0025: every bridge replica, asked at its own address,
# issues credentials (details at bridge_replicas of the kind harness).
run "bridge_replicas" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig               = run.gke.kubeconfig
    name                     = "bridge-replicas"
    namespace                = "token-ns"
    service_account_name     = "token-check"
    image                    = run.push.image_tags.seed
    image_pull_policy        = "IfNotPresent"
    projected_token_audience = "harbor-bridge"
    command                  = ["sh", "-c"]
    args = [<<-SH
      set -eu
      url='${run.bridge_install.credentials_url}'
      pods='${run.bridge_install.bridge_pods_host}'
      want=${run.bridge_install.bridge_replicas}
      robot=bridge-gke-e2e.token-ns.token-check
      host=$${url#https://}; host=$${host%%/*}
      name=$${host%:*}; port=$${host##*:}

      openssl s_client -connect "$host" -servername "$name" </dev/null 2>/dev/null \
        | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' > /tmp/bridge-ca.crt
      test -s /tmp/bridge-ca.crt

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
    timeout_seconds = 180
    fail_message    = "audit H1 / ADR-0025: not every bridge replica serves credentials at its own address (see pod.log and bridge.log)"
  }
}

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
    args                 = ["echo test-pull/image-puller pulled your-project; exit 0"]
    timeout_seconds      = 300
    fail_message         = "GKE bridge pull failed. Check the installer logs (merge target), the bridge logs (issuer / JWKS), and whether the loopback NodePort routes under Dataplane V2 (R4: bridge_endpoint)"
  }
}

run "pull_pod_alpha" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "pull-collide-alpha"
    namespace            = "team-a"
    service_account_name = "svc-b"
    image                = "${run.gke.harbor_hostname}/project-alpha/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo team-a/svc-b pulled project-alpha; exit 0"]
    timeout_seconds      = 300
    fail_message         = "ADR-0018 collision regression: team-a/svc-b could not pull project-alpha"
  }
}

run "pull_pod_beta" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "pull-collide-beta"
    namespace            = "team"
    service_account_name = "a-svc-b"
    image                = "${run.gke.harbor_hostname}/project-beta/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo team/a-svc-b pulled project-beta; exit 0"]
    timeout_seconds      = 300
    fail_message         = "ADR-0018 collision regression: team/a-svc-b could not pull project-beta"
  }
}

run "pull_pod_gamma" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "pull-tenant-gamma"
    namespace            = "app-ns"
    service_account_name = "runner"
    image                = "${run.gke.harbor_hostname}/project-gamma/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo app-ns/runner pulled project-gamma; exit 0"]
    timeout_seconds      = 300
    fail_message         = "cluster-wide CR pickup broke on GKE"
  }
}

run "pull_pod_multi" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "pull-multi-beta1"
    namespace            = "beta-ns"
    service_account_name = "beta-runner"
    image                = "${run.gke.harbor_hostname}/beta-1/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo beta-ns/beta-runner pulled beta-1; exit 0"]
    timeout_seconds      = 300
    fail_message         = "multi-project pull broke on GKE"
  }
}

# Coexistence (the merge-mode guarantee): AFTER our provider entry was
# merged into GKE's credential-provider config, GKE's own provider must
# still authenticate Artifact Registry pulls.
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
    fail_message         = "COEXISTENCE BROKEN: Artifact Registry pull failed after the installer merged our provider entry — GKE's own credential-provider entry was likely damaged"
  }
}

run "robot_push_test" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "robot-push-test"
    namespace            = run.bridge_install.namespace
    service_account_name = "default"
    image                = run.push.image_tags.seed
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = "robot-${run.bridge_install.namespace}.multi-access"
    command              = ["sh", "-c"]
    args = [<<-SH
      set -eu
      H='${run.gke.harbor_hostname}'
      openssl s_client -connect "$H:443" -servername "$H" </dev/null 2>/dev/null \
        | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' \
        > /usr/local/share/ca-certificates/harbor-e2e.crt
      test -s /usr/local/share/ca-certificates/harbor-e2e.crt
      update-ca-certificates 2>/dev/null
      crane auth login "$H" -u "$username" -p "$password"
      crane copy "$H/beta-2/app:v1" "$H/beta-2/pushed-by-robot:v1"
      crane digest "$H/beta-2/pushed-by-robot:v1" >/dev/null
      crane digest "$H/beta-3/app:v1" >/dev/null
      echo "robot verified: push to beta-2, pull from beta-3"
    SH
    ]
    timeout_seconds = 300
    fail_message    = "pull,push robot failed on GKE"
  }
}

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
    oidc_issuer           = run.gke.oidc_issuer
    oidc_jwks_url         = "https://kubernetes.default.svc/openid/v1/jwks"
    token_command         = "gcloud auth print-access-token"
    match_images = [
      "${run.gke.harbor_hostname}/your-project",
      "${run.gke.harbor_hostname}/project-alpha",
      "${run.gke.harbor_hostname}/project-beta",
      "${run.gke.harbor_hostname}/project-gamma",
      "${run.gke.harbor_hostname}/beta-1",
      "${run.gke.harbor_hostname}/beta-2",
      "${run.gke.harbor_hostname}/beta-3",
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
    args                 = ["echo upgrade-ns/upgrade-runner pulled upgrade-only; exit 0"]
    timeout_seconds      = 300
    fail_message         = "ADR-0021 regression on GKE: the matchImages change did not reach the running kubelet"
  }
}

# From here on the install module's outputs come from run.bridge_upgrade
# (a run that re-uses a state replaces the earlier run's outputs).
run "harbor_access_update" {
  command = apply
  module {
    source = "../e2e/modules/harbor-access-scenario"
  }
  variables {
    kubeconfig       = run.gke.kubeconfig
    phase            = "updated"
    bridge_namespace = run.bridge_upgrade.namespace
    issuer           = run.gke.oidc_issuer
    audience         = "harbor-bridge"
  }
}

run "pull_pod_granted" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "pull-granted-gamma"
    namespace            = "test-pull"
    service_account_name = "image-puller"
    image                = "${run.gke.harbor_hostname}/project-gamma/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo test-pull/image-puller pulled project-gamma; exit 0"]
    timeout_seconds      = 300
    fail_message         = "permission update did not take effect in Harbor, or a rotation broke kubelet-cached credentials"
  }
}

run "pull_pod_renamed" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "pull-renamed-alpha"
    namespace            = "team-a"
    service_account_name = "svc-renamed"
    image                = "${run.gke.harbor_hostname}/project-alpha/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo team-a/svc-renamed pulled project-alpha; exit 0"]
    timeout_seconds      = 300
    fail_message         = "serviceAccountRef change: the new ServiceAccount could not pull"
  }
}

run "pull_pod_revoked" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "pull-revoked-alpha"
    namespace            = "team-a"
    service_account_name = "svc-b"
    image                = "${run.gke.harbor_hostname}/project-alpha/app:v1"
    command              = ["sh", "-c"]
    args                 = ["echo team-a/svc-b pulled project-alpha; exit 0"]
    timeout_seconds      = 300
    expect_pull_failure  = true
    fail_message         = "revocation failed: team-a/svc-b could still pull project-alpha after its HarborAccess moved to another ServiceAccount"
  }
}

run "robot_check_update" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "robot-check-update"
    namespace            = run.seed_image.namespace
    service_account_name = "default"
    image                = run.push.image_tags.seed
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = run.seed_image.admin_secret_name
    command              = ["sh", "-c"]
    args = [<<-SH
      set -euo pipefail
      api=http://harbor-core.harbor.svc.cluster.local/api/v2.0
      # robots QUERY: the names of the robots Harbor lists for QUERY, as a
      # JSON array. A failed request, or an answer that is not exactly one
      # JSON list (an empty body included), ends the Job (details at
      # robot_check_update of the kind harness).
      robots() {
        body=$(curl -fsS -m 10 -u "$username:$password" "$api/robots?page_size=100&q=$1") || return 1
        printf '%s' "$body" | jq -cs 'if length == 1 and (.[0] | type) == "array" then [.[0][].name] else error("Harbor did not answer with one robot list") end'
      }
      old=$(robots name%3Dbridge-gke-e2e.team-a.svc-b)
      new=$(robots name%3Dbridge-gke-e2e.team-a.svc-renamed)
      all=$(robots name%3D~bridge-gke-e2e.)
      echo "old robot: $old; new robot: $new; robots of cluster gke-e2e: $all"
      printf '%s' "$old" | jq -e 'length == 0' >/dev/null
      printf '%s' "$new" | jq -e 'length == 1' >/dev/null
      printf '%s' "$all" | jq -e --argjson want ${length(run.harbor_access_update.harbor_accesses)} \
        'length == $want and any(.[]; endswith("bridge-gke-e2e.team-a.svc-renamed"))' >/dev/null
    SH
    ]
    timeout_seconds = 120
    fail_message    = "Harbor state after the serviceAccountRef change is wrong: the old robot must be deleted, the new one present, and the fuzzy name=~bridge-gke-e2e. query must list one robot per HarborAccess (or Harbor could not be asked; see pod.log)"
  }
}

# ADR-0028: the bridge refuses a token bound to no pod and one living 2h,
# and serves a pod-bound 1h token of the same ServiceAccount (details at the
# token_rejection stage of the kind harness, tests/02-bridge.tftest.hcl).
run "token_rejection" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "token-rejection"
    namespace            = "token-ns"
    service_account_name = "token-check"
    image                = run.push.image_tags.seed
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
      robot=bridge-gke-e2e.token-ns.token-check
      sa=/var/run/secrets/kubernetes.io/serviceaccount
      ns=$(cat "$sa/namespace")

      # The bridge's serving certificate, off the wire. The bridge install's
      # issuer is selfSigned, so it is the CA cert-manager writes to ca.crt
      # for the plugin; curl still checks that it names the Service host.
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
    timeout_seconds = 180
    fail_message    = "ADR-0028: the bridge accepted an unbound or longer-than-1h token, refused a pod-bound 1h token, or did not log the decision with its category (see pod.log and bridge.log)"
  }
}

run "file_sleep" {
  command = apply
  module {
    source = "../e2e/modules/test-sleep"
  }
  variables {
    enabled = try(var.pause_after_pull, false)
  }
}

run "harbor_access_teardown" {
  command = apply
  module {
    source = "../e2e/modules/harbor-access-scenario"
  }
  variables {
    kubeconfig       = run.gke.kubeconfig
    phase            = "none"
    bridge_namespace = run.bridge_upgrade.namespace
    issuer           = run.gke.oidc_issuer
    audience         = "harbor-bridge"
  }
}

run "robot_check_teardown" {
  command = apply
  module {
    source = "../e2e/modules/test-exec-pod"
  }
  variables {
    kubeconfig           = run.gke.kubeconfig
    name                 = "robot-check-teardown"
    namespace            = run.seed_image.namespace
    service_account_name = "default"
    image                = run.push.image_tags.seed
    image_pull_policy    = "IfNotPresent"
    env_from_secret      = run.seed_image.admin_secret_name
    command              = ["sh", "-c"]
    args = [<<-SH
      set -eu
      api=http://harbor-core.harbor.svc.cluster.local/api/v2.0
      # Only an empty LIST passes (details at robot_check_teardown of the
      # kind harness): a failed request is retried and fails the Job if
      # Harbor never answers; an answer that is not exactly one JSON list
      # (an empty body included) fails it at once.
      state="Harbor was never asked"
      for i in $(seq 1 30); do
        if body=$(curl -fsS -m 10 -u "$username:$password" "$api/robots?page_size=100&q=name%3D~bridge-gke-e2e."); then
          left=$(printf '%s' "$body" | jq -rs 'if length == 1 and (.[0] | type) == "array" then .[0][].name else error("Harbor did not answer with one robot list") end')
          if [ -z "$left" ]; then echo "no robots of cluster gke-e2e left in Harbor"; exit 0; fi
          state="robots left behind: $left"
        else
          state="the Harbor query failed (curl exit $?)"
        fi
        sleep 3
      done
      echo "$state"; exit 1
    SH
    ]
    timeout_seconds = 150
    fail_message    = "finalizer cleanup incomplete: robots of cluster gke-e2e are still in Harbor after every HarborAccess was deleted, or Harbor could not be asked (see pod.log)"
  }
}
