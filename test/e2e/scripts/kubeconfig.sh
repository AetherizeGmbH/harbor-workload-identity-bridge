# Sourced (bash) by every local-exec script of the harness that runs
# kubectl: test-exec-pod, harbor-bridge-install's teardown guard and
# coredns-cm. One copy, so the three cannot drift apart.
#
# harness_kubeconfig DIR writes a kubeconfig for the cluster the K8S_*
# environment variables describe into DIR, which it makes private (0700)
# and the caller removes on exit:
#
#   K8S_HOST, K8S_CA     the apiserver URL and its CA (PEM)
#   K8S_TOKEN            a bearer token (GKE: the operator's access token),
#                        or empty, and then
#   K8S_CERT, K8S_KEY    a client certificate and key (PEM; kind)
#
# k then runs kubectl with that file and nothing else. Every credential
# reaches kubectl through a file in DIR and never through a command line:
# any local user can read a process's argv (ps, /proc/<pid>/cmdline), and
# the GKE token is the operator's cloud-platform credential. Only bash
# builtins (printf) handle the values, so they are not on any argv either.
# The explicit --kubeconfig keeps the operator's ~/.kube/config and
# $KUBECONFIG out of every call.

harness_kubeconfig() {
  local d=$1
  if [ ! -d "$d" ]; then
    echo "harness_kubeconfig: $d is not a directory" >&2
    return 1
  fi
  chmod 700 "$d" || return 1
  printf '%s' "$K8S_CA" >"$d/ca.crt" || return 1
  local user
  if [ -n "${K8S_TOKEN:-}" ]; then
    printf '%s' "$K8S_TOKEN" >"$d/token" || return 1
    user="    tokenFile: \"$d/token\""
  else
    printf '%s' "${K8S_CERT:-}" >"$d/tls.crt" || return 1
    printf '%s' "${K8S_KEY:-}" >"$d/tls.key" || return 1
    user="    client-certificate: \"$d/tls.crt\""$'\n'"    client-key: \"$d/tls.key\""
  fi
  printf '%s\n' \
    'apiVersion: v1' \
    'kind: Config' \
    'clusters:' \
    '- name: harness' \
    '  cluster:' \
    "    server: \"$K8S_HOST\"" \
    "    certificate-authority: \"$d/ca.crt\"" \
    'users:' \
    '- name: harness' \
    '  user:' \
    "$user" \
    'contexts:' \
    '- name: harness' \
    '  context:' \
    '    cluster: harness' \
    '    user: harness' \
    'current-context: harness' >"$d/kubeconfig" || return 1
  HARNESS_KUBECONFIG="$d/kubeconfig"
}

k() {
  kubectl --kubeconfig="${HARNESS_KUBECONFIG:?harness_kubeconfig was not called}" "$@"
}
