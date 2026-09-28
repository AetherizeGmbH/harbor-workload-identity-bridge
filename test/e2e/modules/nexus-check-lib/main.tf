# Shell helpers the Nexus check Jobs of tests/03-nexus.tftest.hcl share.
# No resources: the output `sh` is prepended to each Job's script (the
# e2e-seed image: busybox sh, curl, jq, openssl, crane).
#
#   fail MESSAGE             print MESSAGE, exit 1
#   retry SECONDS CMD...     run CMD every 3 s until it succeeds; 1 after SECONDS
#   reg REPO                 http base URL of the docker connector of REPO
#   token_code REPO U P      HTTP status of U's docker token request at REPO
#   docker_token REPO U P    U's docker bearer token (stdout; never print it)
#   manifest_code REPO TOK   HTTP status of GET app:v1's manifest with TOK
#   v2_code REPO TOK         HTTP status of GET /v2/ with TOK (authentication only)
#   refused CODE             CODE is 401 or 403
#   kapi METHOD PATH [TYPE BODY]
#                            Kubernetes API call as the Job's ServiceAccount;
#                            HTTP status on stdout, body in /tmp/kapi.json
#   secret_path NAME         API path of Secret NAME in the bridge namespace
#   ann KEY / data KEY       annotation / decoded data key of the object in
#                            /tmp/kapi.json
#   bridge_ca                the bridge's serving CA to /tmp/bridge-ca.crt
#   mint_pod_token           1h token of the Job's ServiceAccount for the
#                            bridge audience, bound to the Job's pod (needs
#                            POD_NAME, POD_UID, SA_NAME and RBAC for the
#                            ServiceAccount's own token)
#   bridge_ask TOK IMAGE     POST IMAGE to the bridge; HTTP status on
#                            stdout, the answer in /tmp/bridge.json
#   nx GET PATH              Nexus REST call with the admin credentials of
#                            the Job's env ($username, $password); HTTP
#                            status on stdout, body in /tmp/nx.json
#   nexus_state              the cluster's users and roles to /tmp/users.json
#                            and /tmp/roles.json (needs nx's credentials)
#   identity ID OWNER PRIV...
#                            exactly one role ID with exactly PRIV... and one
#                            user ID_<generation> holding only that role, both
#                            marked for NexusAccess OWNER (ADR-0036 decision b)
#   gone ID                  neither role ID nor any user ID_* exists
#
# Nexus hands every user one persistent docker bearer token (ADR-0036
# Context 1), whichever connector issues it.
terraform {
  required_version = ">= 1.6"
}

variable "registry_hosts" {
  type        = map(string)
  description = "Nexus docker repository → host:port of its HTTP connector."

  validation {
    condition = alltrue([
      for repo, host in var.registry_hosts :
      can(regex("^[a-z0-9][a-z0-9-]*$", repo)) && can(regex("^[a-z0-9][a-z0-9.-]*:[0-9]+$", host))
    ])
    error_message = "Repository names must be lower-case letters, digits and '-'; hosts host:port."
  }
}

variable "credentials_url" {
  type        = string
  description = "The bridge's credential endpoint through its Service (harbor-bridge-install's credentials_url)."
}

variable "audience" {
  type = string
}

variable "bridge_namespace" {
  type = string
}

variable "nexus_url" {
  type        = string
  description = "Nexus base URL inside the cluster, without /service/rest."
}

variable "cluster_name" {
  type        = string
  default     = "dev"
  description = "The bridge's clusterName: the ownership prefix bridge-<cluster>. and the marker of its Nexus users and roles."
}

locals {
  registry_cases = join("\n", [for repo, host in var.registry_hosts : "    ${repo}) echo 'http://${host}' ;;"])
}

output "sh" {
  value = <<-SH
    # --- test/e2e/modules/nexus-check-lib ---
    set -eu
    fail() { echo "FAIL: $*" >&2; exit 1; }
    retry() {
      _deadline=$(( $(date +%s) + $1 )); shift
      until "$@"; do
        [ "$(date +%s)" -lt "$_deadline" ] || return 1
        sleep 3
      done
    }
    reg() {
      case "$1" in
    ${local.registry_cases}
        *) echo "no connector for repository $1" >&2; return 1 ;;
      esac
    }
    ACCEPT='application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.index.v1+json'
    token_code() {
      _b=$(reg "$1") || return 1
      _c=$(curl -sS -o /tmp/nx-token.json -w '%%{http_code}' -G -u "$2:$3" \
        --data-urlencode "account=$2" --data-urlencode "service=$_b/v2/token" "$_b/v2/token") || _c=000
      echo "$_c"
    }
    docker_token() {
      _c=$(token_code "$1" "$2" "$3")
      if [ "$_c" != 200 ]; then rm -f /tmp/nx-token.json; echo "docker token of $2 from $1: HTTP $_c" >&2; return 1; fi
      if ! jq -er '.token | select(type == "string" and length > 0)' /tmp/nx-token.json; then
        rm -f /tmp/nx-token.json; echo "docker token of $2 from $1: no token in the answer" >&2; return 1
      fi
      rm -f /tmp/nx-token.json
    }
    manifest_code() {
      _b=$(reg "$1") || return 1
      _c=$(curl -sS -o /dev/null -w '%%{http_code}' -H "Authorization: Bearer $2" -H "Accept: $ACCEPT" "$_b/v2/app/manifests/v1") || _c=000
      echo "$_c"
    }
    v2_code() {
      _b=$(reg "$1") || return 1
      _c=$(curl -sS -o /dev/null -w '%%{http_code}' -H "Authorization: Bearer $2" "$_b/v2/") || _c=000
      echo "$_c"
    }
    refused() { [ "$1" = 401 ] || [ "$1" = 403 ]; }

    KAPI=https://kubernetes.default.svc
    SADIR=/var/run/secrets/kubernetes.io/serviceaccount
    BRIDGE_NS='${var.bridge_namespace}'
    kapi() {
      if [ $# -ge 4 ]; then
        _c=$(curl -sS -o /tmp/kapi.json -w '%%{http_code}' -X "$1" --cacert "$SADIR/ca.crt" \
          -H "Authorization: Bearer $(cat "$SADIR/token")" -H "Content-Type: $3" --data "$4" "$KAPI$2") || _c=000
      else
        _c=$(curl -sS -o /tmp/kapi.json -w '%%{http_code}' -X "$1" --cacert "$SADIR/ca.crt" \
          -H "Authorization: Bearer $(cat "$SADIR/token")" "$KAPI$2") || _c=000
      fi
      echo "$_c"
    }
    secret_path() { echo "/api/v1/namespaces/$BRIDGE_NS/secrets/$1"; }
    ann() { jq -r --arg k "$1" '.metadata.annotations[$k] // ""' /tmp/kapi.json; }
    data() { jq -r --arg k "$1" '.data[$k] // "" | @base64d' /tmp/kapi.json; }

    AUD='${var.audience}'
    CREDS_URL='${var.credentials_url}'
    bridge_ca() {
      _h=$${CREDS_URL#https://}; _h=$${_h%%/*}
      openssl s_client -connect "$_h" -servername "$${_h%:*}" </dev/null 2>/dev/null \
        | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' > /tmp/bridge-ca.crt
      test -s /tmp/bridge-ca.crt
    }
    mint_pod_token() {
      _ns=$(cat "$SADIR/namespace")
      _req=$(jq -cn --arg aud "$AUD" --arg pod "$POD_NAME" --arg uid "$POD_UID" \
        '{apiVersion: "authentication.k8s.io/v1", kind: "TokenRequest",
          spec: {audiences: [$aud], expirationSeconds: 3600,
                 boundObjectRef: {apiVersion: "v1", kind: "Pod", name: $pod, uid: $uid}}}')
      _c=$(kapi POST "/api/v1/namespaces/$_ns/serviceaccounts/$SA_NAME/token" application/json "$_req")
      case "$_c" in
        200|201) jq -er .status.token /tmp/kapi.json; _s=$?; rm -f /tmp/kapi.json; return $_s ;;
        *) echo "TokenRequest: HTTP $_c" >&2; return 1 ;;
      esac
    }
    bridge_ask() {
      _c=$(curl -sS -o /tmp/bridge.json -w '%%{http_code}' --cacert /tmp/bridge-ca.crt \
        -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
        --data "$(jq -cn --arg i "$2" '{image: $i}')" "$CREDS_URL") || _c=000
      echo "$_c"
    }

    NX_API='${var.nexus_url}/service/rest'
    nx() {
      _c=$(curl -sS -o /tmp/nx.json -w '%%{http_code}' -u "$username:$password" -X "$1" "$NX_API$2") || _c=000
      echo "$_c"
    }
    CLUSTER='${var.cluster_name}'
    MARKER="managed-by=harbor-workload-identity-bridge cluster=$CLUSTER"
    nexus_state() {
      _c=$(nx GET "/v1/security/users?source=default&userId=bridge-$CLUSTER.")
      [ "$_c" = 200 ] || { echo "list users: HTTP $_c" >&2; return 1; }
      # Nexus matches the id prefix case-insensitively; ownership is not.
      jq --arg p "bridge-$CLUSTER." '[.[] | select(.userId | startswith($p))]' /tmp/nx.json > /tmp/users.json
      _c=$(nx GET "/v1/security/roles?source=default")
      [ "$_c" = 200 ] || { echo "list roles: HTTP $_c" >&2; return 1; }
      jq --arg p "bridge-$CLUSTER." '[.[] | select(.id | startswith($p))]' /tmp/nx.json > /tmp/roles.json
    }
    identity() {
      _id=$1; _owner=$2; shift 2
      _want=$(printf '%s\n' "$@" | jq -R . | jq -sc 'sort')
      jq -e --arg id "$_id" --arg d "$MARKER nexusaccess=$_owner" --argjson p "$_want" '
          [.[] | select(.id == $id)] as $r
          | ($r | length) == 1 and ($r[0].privileges | sort) == $p
            and ($r[0].roles | length) == 0 and $r[0].description == $d' /tmp/roles.json >/dev/null \
        || fail "role $_id: want privileges $_want and owner $_owner, got $(jq -c --arg id "$_id" '[.[] | select(.id == $id)]' /tmp/roles.json)"
      jq -e --arg id "$_id" --arg first "$MARKER" --arg last "nexusaccess=$_owner" '
          [.[] | select(.userId | startswith($id + "_"))] as $u
          | ($u | length) == 1
            and ($u[0].userId | ltrimstr($id + "_") | test("^[0-9a-f]{16}$"))
            and $u[0].roles == [$id] and $u[0].status == "active"
            and $u[0].firstName == $first and $u[0].lastName == $last
            and $u[0].emailAddress == "nexus-bridge@harbor-workload-identity-bridge.invalid"' /tmp/users.json >/dev/null \
        || fail "users of $_id: want one <id>_<16 hex> holding only role $_id, owned by $_owner; got $(jq -c --arg id "$_id" '[.[] | select(.userId | startswith($id + "_")) | {userId, status, roles, firstName, lastName}]' /tmp/users.json)"
    }
    gone() {
      jq -e --arg id "$1" '[.[] | select(.id == $id)] | length == 0' /tmp/roles.json >/dev/null \
        || fail "role $1 still exists"
      jq -e --arg id "$1" '[.[] | select(.userId | startswith($id + "_"))] | length == 0' /tmp/users.json >/dev/null \
        || fail "users of $1 still exist: $(jq -c --arg id "$1" '[.[] | select(.userId | startswith($id + "_")) | .userId]' /tmp/users.json)"
    }
    # --- end of nexus-check-lib ---
  SH
}
