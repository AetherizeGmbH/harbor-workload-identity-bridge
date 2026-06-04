#!/usr/bin/env bash
# Watches for the bridge-pull-test pod and captures diagnostics the
# moment it lands in a failed state. Designed to race with tofu test's
# destroy phase — we win because containerd ImagePullBackOff happens
# fast (≤30s) while tofu's pull_pod Job has a 300s timeout, giving us
# a 4+ minute window to grab everything.
set -uo pipefail

ROOT=${1:-./diag-last}
mkdir -p "$ROOT"

cluster=bridge-e2e

echo "[capture] waiting for cluster ${cluster} + pull-test pod"

# Wait up to 20 minutes for the pull-test pod to appear (we expect the
# preceding stages to take 8-12 minutes)
for _ in $(seq 1 1200); do
  if kubectl --request-timeout=5s -n test-pull get pod -l app=bridge-pull-test --no-headers 2>/dev/null | grep -q bridge-pull-test; then
    break
  fi
  sleep 1
done

if ! kubectl --request-timeout=5s -n test-pull get pod -l app=bridge-pull-test --no-headers 2>/dev/null | grep -q bridge-pull-test; then
  echo "[capture] pull-test pod never appeared; nothing to capture"
  exit 0
fi

echo "[capture] pull-test pod present; waiting for ImagePullBackOff/Error state"

# Wait for the pod to be in a failed-pulling state
for _ in $(seq 1 240); do
  status=$(kubectl --request-timeout=5s -n test-pull get pod -l app=bridge-pull-test -o jsonpath='{.items[0].status.containerStatuses[0].state}' 2>/dev/null)
  if echo "$status" | grep -qE 'ImagePullBackOff|ErrImagePull|CrashLoopBackOff'; then
    break
  fi
  if echo "$status" | grep -q '"running"'; then
    echo "[capture] pod is running — pull succeeded, no failure to capture"
    exit 0
  fi
  sleep 2
done

echo "[capture] capturing diagnostics to ${ROOT}"

for node in $(docker ps --filter "label=io.x-k8s.kind.cluster=${cluster}" --format "{{.Names}}"); do
  docker exec "$node" journalctl -u kubelet --no-pager -n 800 \
    > "${ROOT}/${node}.kubelet.log" 2>&1
  docker exec "$node" sh -c '
    echo "=== /etc/default/kubelet ==="; cat /etc/default/kubelet 2>/dev/null
    echo; echo "=== /etc/kubernetes/credential-provider ==="; ls -la /etc/kubernetes/credential-provider 2>/dev/null
    echo; echo "=== credential-provider-config.yaml ==="; cat /etc/kubernetes/credential-provider-config/credential-provider-config.yaml 2>/dev/null
    echo; echo "=== containerd hosts.toml ==="; find /etc/containerd/certs.d -type f 2>/dev/null | while read f; do echo "--- $f ---"; cat "$f"; done
  ' > "${ROOT}/${node}.node-state.log" 2>&1
done

kubectl get pods -A -o wide                                              > "${ROOT}/pods.txt"   2>&1
kubectl get nodes -o wide                                                > "${ROOT}/nodes.txt"  2>&1
kubectl describe pod -n test-pull -l app=bridge-pull-test                > "${ROOT}/pull-test.describe.txt" 2>&1
kubectl logs -n harbor-bridge-system deploy/harbor-bridge --tail=200     > "${ROOT}/bridge.log" 2>&1
kubectl logs -n harbor-bridge-system ds/harbor-bridge-plugin --all-containers --tail=100 > "${ROOT}/plugin.log" 2>&1
kubectl -n harbor-bridge-system get events --sort-by='.lastTimestamp'    > "${ROOT}/bridge-events.txt"   2>&1
kubectl -n test-pull get events --sort-by='.lastTimestamp'               > "${ROOT}/test-pull-events.txt" 2>&1

# Also pull bridge metrics — the load-bearing signal: ok/forbidden/unauthorized counters
kubectl -n harbor-bridge-system port-forward svc/harbor-bridge 18443:8443 >/dev/null 2>&1 &
PF=$!
sleep 3
curl -sk https://localhost:18443/metrics 2>/dev/null | grep -E "^bridge_credential_issuances_total|^bridge_oidc_validation_failures_total" \
  > "${ROOT}/bridge-metrics.txt" 2>&1
kill $PF 2>/dev/null
wait 2>/dev/null

echo "[capture] done — see ${ROOT}/"
