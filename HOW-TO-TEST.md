# How to test the bridge end-to-end

This is the playbook for validating the bridge against a real Kubernetes
cluster and a real Harbor, without the Helm chart (which doesn't exist
yet). It's also the closest thing we have to a contributor onboarding
guide.

> **Status as of 2026-05-31:** ADR-0013 is validated. All four phases
> below were run against kind + Harbor 2.x: `Ready=True` reached, the
> bridge returned credentials, `crane pull` produced a 4MB OCI tarball
> from Harbor with those credentials. The run also uncovered and fixed
> [a latent `robot$`-prefix bug](docs/adr/0014-harbor-robot-dollar-prefix-handling.md)
> in the reconciler; if you're testing against a Harbor instance where
> system-level robots were created by earlier bridge builds, the
> existing 409-recovery path will adopt them on the first reconcile
> with the current code.
>
> Re-run the full sequence whenever you change anything in the
> credential path (validator, handler, harbor client). The unit tests
> can't tell you whether containerd-equivalent clients still accept
> what the bridge returns.

## Topology

The bridge runs **on your laptop**, not in the cluster. In production
(Phase 5 onward) it will run as a Deployment inside the cluster. For now
it's a `go run` process pointed at kind via your kubeconfig.

This setup has one quirk that the diagram below clarifies: the bridge's
OIDC validator wants to fetch a JWKS from
`https://kubernetes.default.svc.cluster.local/openid/v1/jwks`, but
that hostname is cluster-internal — it only resolves from inside a Pod.
From your laptop, the name doesn't resolve at all. The fix is
`kubectl proxy`: a small auxiliary process on your laptop that forwards
HTTP requests to the kind apiserver using your kubeconfig credentials.
The bridge fetches JWKS via the proxy. The `iss` claim it expects on
incoming SA tokens is still the cluster-internal URL — only the *fetch
URL* changes. This is what the new `BRIDGE_OIDC_JWKS_URL` env var
controls.

![Local-dev topology](docs/img/local-dev-topology.svg)

Numbered flows in the diagram:

1. **bridge → kubectl proxy** — go-oidc's JWKS fetch over plain HTTP localhost.
2. **kubectl proxy → kube-apiserver** — proxy forwards the request, attaching
   your kubeconfig credentials.
3. **bridge → kube-apiserver** (direct) — controller-runtime's Manager
   watches `HarborAccess` CRs and reads/writes robot Secrets using the
   same kubeconfig. No proxy here; the k8s API client handles DNS and
   TLS itself.
4. **bridge → Harbor** — Harbor REST API for robot account CRUD,
   authenticated with the admin credentials from
   `BRIDGE_HARBOR_ADMIN_DIR`.
5. **your terminal → kube-apiserver** — `kubectl create token` mints
   a fresh, audience-scoped SA token.
6. **your terminal → bridge** — `curl` POSTs that token to
   `/v1/credentials`; bridge returns the robot's Basic Auth.
7. **`crane`/`skopeo` → Harbor** — the ADR-0013 acid test. A real
   registry client takes the credentials from step 6 and completes
   Harbor's bearer-token handshake itself.

## Prerequisites

- A running kind (or any local k8s) cluster with admin access.
- A running Harbor with admin credentials and at least one project you
  can push a small image to.
- `kubectl`, `jq`, `openssl`, Go 1.26+ on your laptop.
- `crane` (preferred) or `skopeo` for Phase 4. Install with
  `go install github.com/google/go-containerregistry/cmd/crane@latest`.

## Phase 0 — Gather info

Note these down before starting:

```bash
# OIDC issuer string the kind apiserver advertises (what tokens claim as iss).
kubectl get --raw /.well-known/openid-configuration | jq -r .issuer
# typical: https://kubernetes.default.svc.cluster.local

# Harbor base URL — must be reachable from your laptop.
echo "https://your-harbor.example.com"   # or e.g. https://harbor.dev.127.0.0.1.nip.io:8443

# A Harbor project name that exists and contains at least one image.
echo "your-project"
```

You'll also need the Harbor admin username and password.

## Phase 1 — Start the bridge

This is the only step that needs **two terminals**.

### Terminal 1: kubectl proxy

```bash
make proxy
# = kubectl proxy --port=8001
# leave this running for the duration of the test
```

Sanity check from a third terminal (optional):

```bash
curl -s http://127.0.0.1:8001/openid/v1/jwks | jq .keys[0].kid
# should print a kid (key id) without error
```

### Terminal 2: the bridge

```bash
# Install the CRD into the cluster.
kubectl apply -f config/crd/bases/harbor.aetherize.io_harboraccesses.yaml

# Create the namespace where robot Secrets will live.
kubectl create namespace harbor-bridge-system

# Drop the Harbor admin credentials into a directory the bridge can read.
# Two files named exactly "username" and "password", contents being
# the literal values.
mkdir -p /tmp/harbor-admin
printf '%s' 'admin' > /tmp/harbor-admin/username
printf '%s' 'YOUR_HARBOR_ADMIN_PASSWORD' > /tmp/harbor-admin/password
chmod 600 /tmp/harbor-admin/*

# Run the bridge.
BRIDGE_CLUSTER_NAME=dev \
BRIDGE_NAMESPACE=harbor-bridge-system \
BRIDGE_OIDC_ISSUER="$(kubectl get --raw /.well-known/openid-configuration | jq -r .issuer)" \
BRIDGE_OIDC_JWKS_URL=http://127.0.0.1:8001/openid/v1/jwks \
BRIDGE_HARBOR_URL=https://your-harbor.example.com \
BRIDGE_HARBOR_ADMIN_DIR=/tmp/harbor-admin \
BRIDGE_LOG_LEVEL=debug \
make run-local
```

What `make run-local` does for you:

- Generates a 1-day self-signed TLS cert in `/tmp/bridge-tls` if absent.
- Sets `BRIDGE_TLS_CERT_FILE`, `BRIDGE_TLS_KEY_FILE`, `BRIDGE_LISTEN_ADDR=:8443`,
  `BRIDGE_HEALTH_ADDR=:8081`.
- Refuses to start if `BRIDGE_OIDC_ISSUER` looks cluster-internal but
  `BRIDGE_OIDC_JWKS_URL` is unset.

**Validation checkpoint 1.** Successful start looks like (json-formatted):

```
"msg":"data-plane server listening","addr":":8443","mtls":false
"msg":"starting orphan-robot sweep"
"msg":"starting bridge","leader_election":false
```

If you see neither, jump to Troubleshooting.

## Phase 2 — Apply a HarborAccess CR

```bash
# A namespace and SA the test workload will eventually run as.
kubectl create namespace test-pull
kubectl create serviceaccount image-puller -n test-pull

# Edit project name + audience to match your setup. The audience is a
# free-form string; you'll use the same value with `kubectl create token`
# below.
cat <<'YAML' | kubectl apply -f -
apiVersion: harbor.aetherize.io/v1alpha1
kind: HarborAccess
metadata:
  name: test-access
  namespace: harbor-bridge-system
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
  tokenTTL: 1h
YAML

# Watch the reconciler do its thing.
kubectl get harboraccess -n harbor-bridge-system test-access -o yaml -w
```

**Validation checkpoint 2.** Within a few seconds you should see:

- `status.conditions[type=Ready].status=True`
- `status.robot.name = robot$bridge-dev-test-pull-image-puller`
- A Secret in the bridge namespace:
  ```bash
  kubectl get secret -n harbor-bridge-system \
    robot-harbor-bridge-system-test-access -o yaml
  ```
  with `data.username` and `data.password`.
- In the Harbor UI, **Administration → Robot Accounts**, a new robot
  named `bridge-dev-test-pull-image-puller` with description
  `managed-by=harbor-workload-identity-bridge cluster=dev
  harboraccess=harbor-bridge-system/test-access`.

If you got here, the **control plane is validated end-to-end against
real Harbor and a real Kubernetes API**. This is independently useful
news.

## Phase 3 — Hit the data plane with curl

```bash
# Mint a fresh SA token with the audience the HarborAccess expects.
# Use the same audience string you put in trustPolicy.audience above.
TOKEN=$(kubectl create token image-puller -n test-pull \
  --audience=harbor-bridge --duration=1h)

# POST to the bridge. The body is audit-only — the bridge logs the image
# string but does not gate on it.
curl -sv -k \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"image":"your-harbor/your-project/whatever:tag"}' \
  https://localhost:8443/v1/credentials | jq .
```

**Validation checkpoint 3.** Expected response:

```json
{
  "username": "robot$bridge-dev-test-pull-image-puller",
  "password": "<long opaque string>",
  "expires_in": 3600,
  "cache_key_type": "ServiceAccount"
}
```

Save the username and password somewhere — Phase 4 needs them.

If you got here, the **SA-token → robot-credentials data path is
validated**: OIDC verification, CR matching, Secret lookup, audit
logging.

## Phase 4 — The ADR-0013 acid test

This is the only step that proves the bridge is **architecturally
correct**, not just internally consistent. We hand the credentials
from Phase 3 to a real registry client and try to pull from Harbor.

### Option A — crane (single command, no daemon)

```bash
USER='robot$bridge-dev-test-pull-image-puller'
PASS='THE_PASSWORD_FROM_PHASE_3'

crane auth login your-harbor.example.com -u "$USER" -p "$PASS"
crane pull your-harbor.example.com/your-project/whatever:tag /tmp/pulled.tar
ls -lh /tmp/pulled.tar
```

### Option B — skopeo

```bash
skopeo inspect \
  --creds "$USER:$PASS" \
  docker://your-harbor.example.com/your-project/whatever:tag
```

**Validation checkpoint 4 — the load-bearing one.**

- **Success** (crane writes a tarball, skopeo prints a manifest): a
  containerd-equivalent client took the credentials we returned and
  completed Harbor's `/service/token` handshake itself. ADR-0013
  holds. Phase 4 of the project (the kubelet plugin binary) is safe
  to start.
- **Failure** with `401 unauthorized` from Harbor's `/service/token`:
  Harbor rejected the Basic Auth. Re-read [ADR-0013](docs/adr/0013-return-robot-basic-auth-credentials.md);
  the password from Phase 3 should match the value in the bridge's
  robot Secret (compare `kubectl get secret … -o jsonpath='{.data.password}' | base64 -d`).
- **Failure** with `403 denied` after auth: the robot's project
  permissions don't cover the image. Check
  `spec.permissions[].project` in the HarborAccess matches the image's
  project segment exactly (Harbor scopes are project-scoped strings, not regex).

If any of these happen, **stop**. The architecture has to be
re-examined before we sink more time into the plugin and chart.

### Reference: what the first successful run looked like

Run on 2026-05-31 against kind + Harbor 2.x:

```
$ crane pull --insecure harbor.dev.…/your-project/alpine:test3 /tmp/pulled.tar
$ tar tf /tmp/pulled.tar
sha256:2ffb2ff4aab36d06b7f3266bbb10e8232769cd2360613131d37abd19430cf6f1
d17f077ada118cc762df373ff803592abf2dfa3ddafaa7381e364dd27a88fca7.tar.gz
manifest.json
$ ls -lh /tmp/pulled.tar
-rw-r--r-- 1 user wheel 4.0M /tmp/pulled.tar
```

Bridge audit log:

```
"msg":"credential issued"
"subject":"system:serviceaccount:test-pull:image-puller"
"audience":"harbor-bridge"
"harboraccess":"harbor-bridge-system/test-access"
"robot":"robot$bridge-dev-test-pull-image-puller"
"ttl_seconds":3600
```

Bridge metrics:

```
bridge_credential_issuances_total{result="ok"} 1
bridge_credential_issuance_duration_seconds_count 1
```

## Phase 5 — Drive the kubelet plugin binary

The plugin is what kubelet actually fork+exec's per image pull
([ADR-0015](docs/adr/0015-plugin-duplicates-wire-types.md)). Until the
Helm chart (Phase 5 of the project) installs it onto a node, we can't
test it under real kubelet — but we can drive it by hand. The plugin is
a pure stdin→stdout transform, so a hand-crafted
`CredentialProviderRequest` reaches the bridge identically to one
emitted by kubelet.

### Binary hygiene

```bash
make build-plugin

file bin/harbor-bridge-plugin                    # static Mach-O / ELF
nm bin/harbor-bridge-plugin | grep -ic cgo       # 0
go list -deps ./plugin/... | grep -cE '^(k8s\.io|sigs\.k8s\.io)'  # 0 — ADR-0015
go version -m bin/harbor-bridge-plugin | grep -E 'CGO_ENABLED|vcs\.revision'
```

ADR-0015 is the standing check: the plugin's dep closure must contain
**zero** `k8s.io` / `sigs.k8s.io` packages. The bridge binary, by
comparison, carries 400+. Adding any such import to the plugin is a
breaking change to that ADR.

### Misconfig paths

Every error exits 1 with a clear stderr message. Spot-check a few:

```bash
echo '{}' | bin/harbor-bridge-plugin                                # HARBOR_BRIDGE_ENDPOINT is required
echo '{}' | HARBOR_BRIDGE_ENDPOINT=http://x.example bin/harbor-bridge-plugin   # must use https
echo '' | HARBOR_BRIDGE_ENDPOINT=https://localhost:8443 bin/harbor-bridge-plugin  # EOF on stdin
```

### Happy path — byte-equal Basic Auth

```bash
TOKEN=$(kubectl create token image-puller -n test-pull \
  --audience=harbor-bridge --duration=10m)

# Baseline: curl the bridge directly.
CURL_RESP=$(curl -sk -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"image":"your-harbor/your-project/whatever:tag"}' \
  https://localhost:8443/v1/credentials)

# Same token, same image, through the plugin.
PLUGIN_RESP=$(jq -n --arg tok "$TOKEN" --arg img "your-harbor/your-project/whatever:tag" \
  '{apiVersion:"credentialprovider.kubelet.k8s.io/v1",
    kind:"CredentialProviderRequest",
    image:$img,
    serviceAccountToken:$tok}' \
  | HARBOR_BRIDGE_ENDPOINT=https://localhost:8443 \
    HARBOR_BRIDGE_CA_BUNDLE=/tmp/bridge-tls/tls.crt \
    bin/harbor-bridge-plugin)

# Auth pairs must be byte-equal.
diff <(jq '{u:.username,p:.password}' <<<"$CURL_RESP") \
     <(jq '.auth | to_entries[0].value | {u:.username,p:.password}' <<<"$PLUGIN_RESP")
```

**Validation checkpoint 5 — the load-bearing one for the plugin.** The
diff is empty, meaning the plugin's `auth.<host>.{username,password}`
pair is identical to the one curl received from the bridge directly. By
transitivity with Phase 4 (crane proved containerd accepts those exact
credentials), the kubelet → plugin → bridge → containerd → Harbor chain
is now end-to-end proven — short of running a real kubelet.

The plugin's response shape should look like:

```json
{
  "apiVersion": "credentialprovider.kubelet.k8s.io/v1",
  "kind": "CredentialProviderResponse",
  "cacheKeyType": "ServiceAccount",
  "cacheDuration": "1h0m0s",
  "auth": {
    "harbor.dev.aetherize.com": {
      "username": "robot$bridge-dev-test-pull-image-puller",
      "password": "<long opaque string>"
    }
  }
}
```

Notice:
- `cacheDuration` is the Go duration form — matches `metav1.Duration`'s
  `MarshalJSON` output, so kubelet's strict decoder accepts it.
- The `auth` map's key is the image's host portion (preserving any
  `:port`) — kubelet uses that to match this credential set to the
  image being pulled.

### Refusal paths — empty auth, exit 0, refusal logged to stderr

```bash
# 401: garbage token.
jq -n '{apiVersion:"credentialprovider.kubelet.k8s.io/v1",
        kind:"CredentialProviderRequest",
        image:"your-harbor/your-project/whatever:tag",
        serviceAccountToken:"not-a-real-token"}' \
| HARBOR_BRIDGE_ENDPOINT=https://localhost:8443 \
  HARBOR_BRIDGE_CA_BUNDLE=/tmp/bridge-tls/tls.crt \
  bin/harbor-bridge-plugin

# 403: valid token but audience the HarborAccess doesn't accept.
WRONG=$(kubectl create token image-puller -n test-pull \
  --audience=wrong-audience --duration=10m)
jq -n --arg tok "$WRONG" \
  '{apiVersion:"credentialprovider.kubelet.k8s.io/v1",
    kind:"CredentialProviderRequest",
    image:"your-harbor/your-project/whatever:tag",
    serviceAccountToken:$tok}' \
| HARBOR_BRIDGE_ENDPOINT=https://localhost:8443 \
  HARBOR_BRIDGE_CA_BUNDLE=/tmp/bridge-tls/tls.crt \
  bin/harbor-bridge-plugin
```

Both should emit:

```json
{
  "apiVersion": "credentialprovider.kubelet.k8s.io/v1",
  "kind": "CredentialProviderResponse",
  "cacheKeyType": "Image",
  "cacheDuration": "0s",
  "auth": {}
}
```

…with `exit 0` and a `bridge refused credentials for …; returning empty
auth (no cache)` line on stderr. The empty auth map plus
`cacheKeyType=Image` + `cacheDuration=0s` tells kubelet *"I have no
credentials for this image and don't cache that fact"* — so the next
pull re-invokes the plugin, giving an operator a fast feedback loop
while fixing a HarborAccess CR. The bridge's metrics should show
`issuances{result=unauthorized}` for the 401 case and
`issuances{result=forbidden}` for the 403 case.

### Reference: validation run on 2026-05-31

Plugin happy path emitted exactly the response shape above; the auth
pair was byte-equal to the curl baseline. Both refusal paths produced
the empty-auth response with exit 0 and stderr message. Bridge metrics
recorded the corresponding counter increments
(`issuances{result=ok}`, `unauthorized`, `forbidden`) and OIDC failure
classification (`oidc_validation_failures{reason=malformed}` for the
garbage token).

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| `dial tcp: lookup kubernetes.default.svc.cluster.local: no such host` at bridge startup | `BRIDGE_OIDC_JWKS_URL` not set | Start `make proxy` in a second terminal and set `BRIDGE_OIDC_JWKS_URL=http://127.0.0.1:8001/openid/v1/jwks`. |
| Reconciler logs `create robot: NOT_FOUND: project "X" not found` and the CR is `Ready=False` | The Harbor project referenced in `spec.permissions[].project` does not exist | Create the project in Harbor. The next reconcile (~immediately, triggered by the existing watch) will recover. |
| Reconciler logs `create robot: … (status 409)` once then converges to `Ready=True` | A previous reconcile created the robot in Harbor but didn't observe success in time; the next reconcile saw 409 and recovered (re-fetched + rotated password + wrote Secret) | None. This is the designed 409 recovery path. The 409 message will remain in `kubectl logs` history. |
| `Ready=False` with `RobotProvisioned=True` (inconsistent status) | Pre-fix bridge (before the 409 recovery landed) created a robot then looped on 409 forever | Upgrade to a bridge build that includes the 409 recovery (any commit after the "decode Harbor errors + 409 recovery" patch). Then `kubectl delete harboraccess` and re-apply, or bump the CR's generation. |
| Bridge starts but `/v1/credentials` returns `401 invalid token` | SA token's `iss` claim does not match `BRIDGE_OIDC_ISSUER` | Re-print the issuer with `kubectl get --raw /.well-known/openid-configuration` and ensure both the env var and the HarborAccess `trustPolicy.issuer` match it byte-for-byte. |
| `/v1/credentials` returns `403 no matching HarborAccess` | Either the SA subject doesn't match `serviceAccountRef` or the token's `aud` doesn't match `trustPolicy.audience` | Compare `kubectl get sa image-puller -n test-pull` (subject = `system:serviceaccount:test-pull:image-puller`) to the CR's `serviceAccountRef.{namespace,name}`. Compare `kubectl create token … --audience=X` X to `trustPolicy.audience`. |
| `/v1/credentials` returns `503 credentials not yet available` | The robot Secret hasn't materialised in the bridge namespace yet | Watch `kubectl get harboraccess -n harbor-bridge-system test-access -o yaml` until `Ready=True`. If it stays `Ready=False`, the reason field will say what's wrong (Harbor unreachable, admin creds bad, etc.). |
| Reconciler logs `tls: failed to verify certificate` against Harbor | Harbor is using a cert signed by a CA your system trust store doesn't have | Currently no fix in code — `BRIDGE_HARBOR_CA_FILE` is on the backlog. Workaround: trust Harbor's CA at the OS level (macOS Keychain, Linux ca-certificates), or use a Harbor with a publicly-trusted cert. |
| `crane pull` fails with `401` from Harbor | Wrong credentials passed (most common), or robot's password rotated between Phase 3 and Phase 4 | Re-run Phase 3 and use the freshly-returned password. |
| `kubectl proxy` exits with `error: error upgrading connection` | Background job got SIGHUP or the kubeconfig context changed | Restart `make proxy` and re-fetch JWKS via `curl 127.0.0.1:8001/openid/v1/jwks` to confirm it's healthy. |

## What this doc explicitly doesn't cover

- **The Helm chart** (Phase 5 of the project). Not built yet; this test
  replaces it with `make run-local` plus manual `kubectl apply`s and a
  hand-driven plugin invocation.
- **Real kubelet fork+exec of the plugin**. Phase 5 covers driving the
  plugin binary by hand; full kubelet integration requires the chart to
  install the credential-provider-config and binary onto a node, which
  is a Phase 6 deliverable.
- **Real containerd via the credential-provider hook**. The closest
  equivalents here are Phase 4 (crane/skopeo, same Harbor handshake)
  and Phase 5 (plugin byte-equal Basic Auth). Full containerd
  validation is part of Phase 6's e2e suite against two kind clusters.
- **Multi-cluster collision tests**. Single cluster only. ADR-0009's
  multi-cluster claims will be validated in Phase 6.
- **Harbor TLS with a private CA**. Currently the bridge's Harbor
  client uses the OS trust store only. If you hit
  `tls: failed to verify certificate`, the troubleshooting table
  has the workaround.

## When you've run this successfully

If Phases 1–4 all pass on a Harbor version or kind topology that's not
yet documented here, add a line to the *Reference: what the first
successful run looked like* section above (Harbor version, k8s version,
crane vs skopeo) so future contributors know the matrix the bridge has
been seen working on.

If a step fails on a setup that worked before, treat it as a
regression — open an issue or ping the maintainer rather than working
around it locally. The whole point of this doc is to surface
architectural breakage early; silently patching a local install
defeats it.
