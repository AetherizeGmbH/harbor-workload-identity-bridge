#!/usr/bin/env bash
# Run between `tofu test` failures and `tofu destroy` to capture the
# state that would otherwise be lost. Polls until the cluster appears
# (in case tofu is still applying), then dumps everything to ./diag/.
set -uo pipefail

ROOT=${1:-./diag}
mkdir -p "$ROOT"
cluster=bridge-e2e

# Wait for cluster nodes to be present
for _ in $(seq 1 30); do
  nodes=$(docker ps --filter "label=io.x-k8s.kind.cluster=${cluster}" --format "{{.Names}}" 2>/dev/null)
  [ -n "$nodes" ] && break
  sleep 2
done

if [ -z "$nodes" ]; then
  echo "No nodes for cluster ${cluster}" >&2
  exit 1
fi

echo "Collecting diagnostics from nodes: $nodes"

for node in $nodes; do
  docker exec "$node" journalctl -u kubelet --no-pager -n 500 > "${ROOT}/${node}.kubelet.log" 2>&1
  docker exec "$node" sh -c '
    echo "=== /etc/default/kubelet ==="; cat /etc/default/kubelet 2>/dev/null
    echo; echo "=== /etc/kubernetes/credential-provider ==="; ls -la /etc/kubernetes/credential-provider 2>/dev/null
    echo; echo "=== /etc/kubernetes/credential-provider-config ==="; ls -la /etc/kubernetes/credential-provider-config 2>/dev/null
    echo; echo "=== credential-provider-config.yaml ==="; cat /etc/kubernetes/credential-provider-config/credential-provider-config.yaml 2>/dev/null
    echo; echo "=== containerd hosts.toml ==="; find /etc/containerd/certs.d -type f -exec sh -c "echo ===\$1===; cat \$1" _ {} \; 2>/dev/null
  ' > "${ROOT}/${node}.cred-state.log" 2>&1
done

kubectl get pods -A -o wide                                                  > "${ROOT}/pods.txt"
kubectl get nodes -o wide                                                    > "${ROOT}/nodes.txt"
kubectl describe pod -n test-pull -l app=bridge-pull-test                    > "${ROOT}/pull-test.describe.txt" 2>&1
kubectl logs -n harbor-bridge-system deploy/harbor-bridge --tail=200         > "${ROOT}/bridge.log" 2>&1
kubectl logs -n harbor-bridge-system ds/harbor-bridge-plugin -c install --tail=100 > "${ROOT}/plugin-install.log" 2>&1
kubectl -n harbor-bridge-system get events --sort-by='.lastTimestamp'        > "${ROOT}/bridge-events.txt" 2>&1
kubectl -n test-pull get events --sort-by='.lastTimestamp'                   > "${ROOT}/test-pull-events.txt" 2>&1
kubectl -n test-pull port-forward $(kubectl -n harbor-bridge-system get svc -l app.kubernetes.io/component=bridge -o name | head -1) >/dev/null 2>&1 &
# don't actually port-forward, just summarize state
echo "Diagnostics written to ${ROOT}"
