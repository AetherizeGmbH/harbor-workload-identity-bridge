# Upstream Kubernetes issue — KEP-4412 credential provider silently aborts between match and exec

This is a ready-to-file draft for kubernetes/kubernetes. Copy the body
into `gh issue create --repo kubernetes/kubernetes --title "..."` or
the web UI once you've confirmed it's worth filing.

---

## Title

```
kubelet credential provider matches image but silently aborts before exec'ing plugin binary (v1.35.0)
```

## What happened?

I have a credential provider plugin wired up per KEP-4412 with both
`KubeletServiceAccountTokenForCredentialProviders` and
`ServiceAccountNodeAudienceRestriction` feature gates enabled. The
plugin binary exists, is executable, lives in the configured
`--image-credential-provider-bin-dir`, and matches the provider name
in the `CredentialProviderConfig`.

When a pod is scheduled with an image matching `matchImages`, kubelet
logs at V=4:

```
plugins.go:55 "Registered credential provider" provider="harbor-bridge-plugin"   # at startup
plugins.go:75 "Generating per pod credential provider"
              provider="harbor-bridge-plugin"
              podName="pull-test"
              podNamespace="test-pull"
              podUID="..."
              serviceAccountName="image-puller"
kuberuntime_image.go:39 "Pulling image without credentials" image="..."
```

The gap between the two log lines is ~115 microseconds. The plugin
binary is **never invoked** — I confirmed this by replacing the
binary with a wrapper script that logs every invocation to `/tmp`. The
log file is never created.

The pod fails with `ImagePullBackOff: no basic auth credentials`.

## What I expected to happen

After the `Generating per pod credential provider` log, kubelet should
fork+exec the plugin binary, pass the
`CredentialProviderRequest` JSON on stdin, read the response from
stdout, and use those credentials when calling
`runtime.PullImage(image, auth, ...)`.

## How to reproduce

```bash
# Standard kind cluster on 1.35
cat <<'EOF' > /tmp/kind-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  KubeletServiceAccountTokenForCredentialProviders: true
nodes:
  - role: control-plane
  - role: worker
EOF
kind create cluster --image kindest/node:v1.35.0 --config /tmp/kind-config.yaml --name kep4412-repro

# Place a minimal "always-credentials" plugin
docker exec kep4412-repro-worker sh -c 'mkdir -p /etc/kubernetes/credential-provider /etc/kubernetes/credential-provider-config'

cat > /tmp/printer <<'BIN'
#!/bin/sh
date >> /tmp/provider.log
echo "[INVOKED]" >> /tmp/provider.log
cat | tee -a /tmp/provider.log >/dev/null
printf '{"apiVersion":"credentialprovider.kubelet.k8s.io/v1","kind":"CredentialProviderResponse","cacheKeyType":"Image","cacheDuration":"1m","auth":{"*":{"username":"x","password":"x"}}}'
BIN
docker cp /tmp/printer kep4412-repro-worker:/etc/kubernetes/credential-provider/printer
docker exec kep4412-repro-worker chmod +x /etc/kubernetes/credential-provider/printer

docker exec kep4412-repro-worker sh -c 'cat > /etc/kubernetes/credential-provider-config/cfg.yaml' <<'EOF'
apiVersion: kubelet.config.k8s.io/v1
kind: CredentialProviderConfig
providers:
  - name: printer
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    matchImages: ["fake.example.com/*"]
    defaultCacheDuration: "1m"
EOF

docker exec kep4412-repro-worker sh -c '
  echo "KUBELET_EXTRA_ARGS=--v=4 --image-credential-provider-bin-dir=/etc/kubernetes/credential-provider --image-credential-provider-config=/etc/kubernetes/credential-provider-config/cfg.yaml" > /etc/default/kubelet
  systemctl daemon-reload
  systemctl restart kubelet
'

# Schedule a pod that triggers credential lookup
kubectl run repro --image=fake.example.com/foo:bar --restart=Never --command -- sleep 60

sleep 10

# Expected: at least one "[INVOKED]" line in /tmp/provider.log
# Observed: file does not exist
docker exec kep4412-repro-worker cat /tmp/provider.log 2>&1
# returns: cat: /tmp/provider.log: No such file or directory

# Observed kubelet log timing:
docker exec kep4412-repro-worker journalctl -u kubelet --since "1 minute ago" --no-pager | \
  grep -E "plugins.go:75|kuberuntime_image.go:39"
# Typically <500 microseconds between the two log lines.
```

## What rules this out

- **Token projection failure.** `kubectl create token <sa>
  --audience=<value> --bound-object-kind=Pod --bound-object-name=<pod>`
  succeeds for the same SA + audience the credential provider would
  request. The apiserver is not rejecting the projected token.
- **tokenAttributes parsing issue.** Removing `tokenAttributes` from
  the provider config does not change the behaviour — the binary is
  still never exec'd, the same `Generating per pod credential
  provider` → `Pulling image without credentials` sequence happens.
  So it is not specific to `KubeletServiceAccountTokenForCredentialProviders`.
- **Binary not executable / wrong permissions.** Confirmed `chmod
  0755`, owned by root. Directly running the binary as root inside
  the kind node container produces the expected response.
- **Cache hit returning empty.** Kubelet was restarted multiple
  times across these tests; in-memory cache should be clear.
  Additionally, the reproduction uses a unique pod UID for each
  attempt.
- **Cilium kube-proxy replacement with `bpf-lb-sock: false`.** The
  silent abort is observed on kind nodes regardless of the CNI; my
  reproduction does not depend on Cilium.

## Environment

- kubelet: `kubectl version --short` → `Server Version:
  v1.35.0`
- Cluster bootstrapped with kind, image `kindest/node:v1.35.0`
- Feature gates enabled in kubelet:
  - `KubeletServiceAccountTokenForCredentialProviders=true`
  - `ServiceAccountNodeAudienceRestriction=true`
- Container runtime: containerd v2.2.0 (from kind image)
- CRI socket: `unix:///run/containerd/containerd.sock`

```
$ docker exec kep4412-repro-worker /usr/bin/kubelet --version
Kubernetes v1.35.0
```

## Anything else we need to know?

Looking at `pkg/credentialprovider/plugin/plugins.go` for the
`Provide()` codepath, between
`klog.V(5).InfoS("Generating per pod credential provider", ...)` and
the call to `p.runPlugin(...)` there is a `getCachedCredentials(image)`
check. If that returns `found=true` with an empty value, the plugin
is never invoked. I can't reproduce that path locally — kubelet was
restarted multiple times during testing — so my best guess is some
other shortcut is firing. A breakpoint between line 75 and
`runPlugin` would reveal which.

I'm happy to test patches on this repro setup; it's deterministic.

## Linked context (downstream)

Discovered while building
https://github.com/aetherize/harbor-workload-identity-bridge — the
single-cluster e2e test fails on the load-bearing image-pull step
purely because of this. The bridge + plugin binary verifications all
pass when driven manually:

- `curl /v1/credentials` with a real SA token returns the right Basic
  Auth pair
- The plugin binary, invoked manually on the node with the kubelet
  stdin/stdout protocol, returns a valid `CredentialProviderResponse`
- `crane` accepts the credentials and successfully pulls the test
  image from Harbor

So the chain works end-to-end at every layer **except** kubelet's own
exec of the binary. That made it pretty clear this is an upstream
kubelet issue rather than a plugin or provider-config issue.

---

**File this if:** the `tofu test` in
[test/e2e/tests/01-bridge.tftest.hcl](../test/e2e/tests/01-bridge.tftest.hcl)
shows the same symptom on a fresh build. If the test passes (i.e.
something in the way we configured the cluster fixes the silent
abort), don't file — instead document what changed in
[docs/PHASES.md](PHASES.md) and recover the v0.1.0 path.
