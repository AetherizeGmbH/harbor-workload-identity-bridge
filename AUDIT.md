# Security Audit — Harbor Workload Identity Bridge

Audit date: 2026-06-06. Scope: the whole repo (Go bridge + plugin, Helm chart,
Dockerfiles, CI, repo hygiene). The reusable prompt that produced this is in
[AUDIT_PROMPT.md](AUDIT_PROMPT.md).

The system's most exposed surface is the data-plane credential endpoint
`POST /v1/credentials`: it is reachable on **every node's NodePort**
(`0.0.0.0:31443` by default) and its only request authenticator is the SA token
in the `Authorization` header (mTLS is optional and off by default). Most of the
weight below is there and in the CR→robot→secret identity mapping.

## Summary

| ID | Severity | Title | Status |
|----|----------|-------|--------|
| F1 | High | Unauthenticated request-body memory-exhaustion DoS on credential endpoint | **Fixed** |
| F2 | High | Robot-name & Secret-name delimiter collision → cross-identity credential/permission confusion | **Partially fixed** (guards + 403 read-path backstop) / Needs-decision (collision-free naming) |
| F3 | Medium | Credential endpoint exposed to whole node network; mTLS off by default | Recommended |
| F4 | Medium | `/metrics` served unauthenticated on the externally-exposed TLS port | Recommended |
| F5 | Low | Data plane did not re-validate per-CR issuer (defense-in-depth) | **Fixed** |
| F6 | Medium | Privileged kubelet-patching init container + images pinned by mutable tag (supply chain) | Recommended |
| F7 | Low | Non-deterministic CR selection when multiple HarborAccess match | Recommended |
| F8 | Low | OIDC validation error string logged at debug | Recommended |
| F9 | Info | `tls.enabled=false` and other "fail-open-ish" chart knobs | Recommended |

---

## F1 — Unauthenticated request-body memory-exhaustion DoS (High) — FIXED

**Location:** `bridge/dataplane/handler.go`, `ServeHTTP` (the request-body decode
block, formerly `json.NewDecoder(r.Body).Decode(&req)`).

**Why it's an issue.** The endpoint binds on every node's NodePort. In
`ServeHTTP` the order was: (1) require the `Authorization` header to merely
*start with* `Bearer ` (any non-empty string passes — `Bearer x`), then (2)
`json.NewDecoder(r.Body).Decode(&req)` with **no size limit**, and only then (3)
validate the token. So an attacker who can reach a node IP and sends
`Authorization: Bearer x` plus a multi-gigabyte JSON body forces the bridge to
buffer it into memory **before any real authentication**. With the chart's
256Mi memory limit this OOM-kills the bridge → crash-loop → cluster-wide image
pulls fail. `ReadTimeout` (15s) does not prevent it on a fast link, and a deeply
nested JSON value amplifies it further. This is an unauthenticated availability
attack on a component every node depends on.

**Fix applied.** Added a 64 KiB cap (the body only carries a short image
reference for audit logging) via `http.MaxBytesReader` before the decode, and a
named constant documenting why:

```go
const maxRequestBodyBytes = 64 << 10 // 64 KiB
...
r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
    http.Error(w, "invalid JSON body", http.StatusBadRequest)
    ...
}
```

An oversized body now returns 400 and is never buffered past the cap.
Regression test: `TestHandler_RejectsOversizedBody` in `handler_test.go`.

**Hardening still recommended (not applied — behavior change):** move token
validation *before* body decode so unauthenticated callers can't even reach the
JSON parser. The size cap removes the DoS; reordering removes the parse surface
entirely. Low effort; left as a decision because it reorders the audit-log
"image" capture relative to auth.

---

## F2 — Robot-name & Secret-name delimiter collision → cross-identity confusion (High) — PARTIALLY FIXED (guards) / NEEDS DECISION (naming scheme)

**Location:**
- `bridge/controlplane/harbor/naming.go:76` — `RobotName` →
  `"bridge-" + cluster + "-" + saNamespace + "-" + saName`.
- `bridge/controlplane/reconciler.go:382` — `secretNameFor` →
  `"robot-" + ha.Namespace + "-" + ha.Name`.
- `bridge/dataplane/handler.go`, `robotSecretName` — same dash-join,
  independently.

**Why it's an issue.** Both names are built by dash-joining DNS-label fields
that may themselves contain dashes (the CRD pattern
`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` allows them). The join is therefore ambiguous —
two *distinct* identities map to the *same* name:

```
RobotName(c, "team-a", "svc")   == RobotName(c, "team", "a-svc")   == "bridge-c-team-a-svc"
secretNameFor(ns="team-a", n="svc") == secretNameFor(ns="team", n="a-svc") == "robot-team-a-svc"
```

Consequences (multi-tenancy boundary break, plausible in a shared cluster):
- **Robot collision.** Two HarborAccess CRs for two different ServiceAccounts
  resolve to the same Harbor robot. The second reconcile finds the first's
  robot, passes the adoption check (it only compares the `cluster=` tag, not the
  `harboraccess=` tag — `reconciler.go:152`), then `UpdatePermissions` +
  `RefreshSecret` on it. Net effect: a single robot whose permissions are the
  last writer's, shared by two unrelated workload identities → **privilege
  escalation** (SA-A can pull/push SA-B's projects) plus the older Secret's
  password is invalidated → **DoS** for the first workload.
- **Secret collision.** Two CRs whose `<namespace>-<name>` collide write/read
  the same `robot-...` Secret in the bridge namespace. The data plane serves
  whatever credentials last landed there to both → wrong-robot credential
  issuance.

`naming.go` already has hash-suffix machinery (`hashOf`, `hashSuffixLen`) but it
is only triggered on *length overflow*, not on this delimiter ambiguity.

**Guards applied in this pass (safe, non-breaking — turn a silent cross-wire
into a clean denial).** The naming scheme itself is unchanged (that change is
breaking — see below), but three guards now make the collision fail safe rather
than disclose:

1. **Reconciler — Secret-name guard.** New `secretOwnedByOtherHA`
   (`reconciler.go`) + an early check in `reconcileNormal` (step 3b): refuse
   (`Ready=False` / `RobotConflict`) to write a robot Secret whose name is
   already held by a bridge-managed Secret *labelled* (`harbor.aetherize.io/
   harboraccess-namespace`/`-name`) for a different HarborAccess. First owner
   keeps its credentials; the colliding CR goes NotReady instead of
   overwriting. Secrets without the labels (pre-labels / hand-made) stay
   adoptable so upgrades don't break.
2. **Reconciler — Robot-name guard.** In `reconcileNormal` (step 5b),
   `recoverExistingRobot`, and `reconcileDelete`: parse the existing robot's
   `harboraccess=<ns>/<name>` description token and refuse to
   adopt/rotate/**delete** a robot that names a *different* HarborAccess. This
   closes the adoption gap noted previously (the adoption check used to compare
   only the `cluster=` tag).
3. **Data plane — read-path backstop.** `readRobotSecret` (`handler.go`) now
   returns `errSecretOwnerMismatch` → **HTTP 403** when the Secret found at the
   expected name is labelled for a different HarborAccess than the one matched
   for the token. This is the hard guarantee against disclosure: even if a
   reconciler guard regressed or a write race occurred, a token matched to CR A
   never receives a Secret stamped for CR B.

Regression tests: `TestReconcile_SecretNameCollision_RefusesToOverwrite`,
`TestReconcile_RobotNameCollision_RefusesForeignHarborAccess` (controlplane),
`TestHandler_SecretOwnerMismatch_Forbidden` (dataplane).

**Residual (Needs-decision — the real cure is collision-resistant naming).**
The guards prevent disclosure/hijack, but two genuinely-distinct workloads
whose names collide still cannot *both* be served (one is denied). Eliminating
that requires unambiguous names:

**How to fix (correct fix is collision-resistant naming).** Make both names a
function of the *structured* tuple, not a lossy dash-join. Recommended: keep a
human-readable prefix for grep-ability but always append a short hash of the
canonical, unambiguously-delimited tuple:

```go
// naming.go — robot name
func RobotName(cluster, saNs, saName string) (string, error) {
    h := hashOf(cluster + "\x00" + saNs + "\x00" + saName) // \x00 can't appear in DNS labels
    readable := fmt.Sprintf("%s%s-%s-%s", robotNamePrefix, cluster, saNs, saName)
    // ... truncate `readable` to leave room for "-"+h, then return readable+"-"+h
}
```

Do the same for `secretNameFor`/`robotSecretName` using
`ha.Namespace + "\x00" + ha.Name`. Both helpers must stay in lockstep
(`handler.go` duplicates the secret-name logic — ADR-0015 — so update both and
keep the cross-package test that pins the contract).

Alternative if you want zero collision risk and don't care about readability:
name purely by hash.

**Why this is left as a decision:** it is a **breaking change** — every existing
robot and Secret gets a new name, so on upgrade the reconciler re-creates robots
and Secrets (old ones become orphans the janitor cleans up). For an alpha,
single-cluster install that's tolerable, but it's an operator-visible migration,
so the maintainer should choose the scheme and the rollout. Until fixed, the
interim mitigation is operational: ADR-0009-style, document that SA
namespace/name pairs (and HarborAccess namespace/name pairs) must not be
dash-rearrangements of each other — but that's a footgun, not a fix.

---

## F3 — Credential endpoint exposed to the whole node network; mTLS off by default (Medium) — RECOMMENDED

**Location:** `charts/harbor-bridge/values.yaml` (`service.type: NodePort`,
`bridge.mTLS.enabled: false`), `charts/harbor-bridge/templates/bridge-service.yaml`,
`bridge/dataplane/server.go` (mTLS only enabled when `ClientCAFile` is set).

**Why it's an issue.** ADR-0008 frames the transport as "plugin →
`127.0.0.1:<nodePort>`", but a NodePort Service binds the port on `0.0.0.0` of
**every** node, not loopback. So the credential endpoint is reachable by anything
that can route to any node IP on `:31443`. The sole authenticator is then the SA
token. Combine with the standard ways a token leaks (a compromised pod, logs, an
SSRF that can read a projected token, a backup) and an attacker with a valid
audience-scoped token for an SA that has a HarborAccess CR can fetch that robot's
credentials **from off-node**, from anywhere with network reach — the localhost
assumption is not enforced anywhere.

**How to fix (pick per environment, document the default):**
- Default `bridge.mTLS.enabled: true` so the SA token is not the only factor, or
  at minimum make the chart loudly warn when it's off with a NodePort Service.
- Constrain reachability: ship a `NetworkPolicy` (or document a host firewall)
  limiting `:31443` to node-local / known sources; or use
  `spec.internalTrafficPolicy: Local` with a ClusterIP + host-routing approach
  instead of a wide-open NodePort where the topology allows.
- Document in `SECURITY.md` that NodePort = node-network-wide exposure and that
  SA-token theft is sufficient for credential theft when mTLS is off.

---

## F4 — `/metrics` unauthenticated on the externally-exposed TLS port (Medium) — RECOMMENDED

**Location:** `bridge/cmd/main.go` (the mux: `mux.Handle("/metrics", ...)` on the
same server as `CredentialsPath`), `bridge/dataplane/server.go` (mTLS optional).

**Why it's an issue.** `/metrics` is served on the same `:8443` that is exposed
via NodePort, and TLS client-auth is off by default (F3). Anyone who can reach
the node port can scrape Prometheus metrics with no credentials. The series
(`bridge_credential_issuances_total{result}`,
`bridge_oidc_validation_failures_total{reason}`, issuance latency, secret-missing
counts) leak issuance volume, failure patterns, and operational state to an
unauthenticated network-adjacent party. Lower impact than F1/F2 but it's free
reconnaissance and an information-disclosure boundary the docs imply doesn't
exist.

**How to fix:** require mTLS for `/metrics` (e.g. only register it when
`ClientCAFile` is set, or serve it on a separate listener bound to the pod
network / scraped via the ServiceMonitor over mTLS), or move it to the
controller-runtime metrics server on a non-NodePort port restricted by
NetworkPolicy. Don't co-locate an unauthenticated read endpoint with the
credential endpoint on a `0.0.0.0` NodePort.

---

## F5 — Data plane did not re-validate per-CR issuer (Low, defense-in-depth) — FIXED

**Location:** `bridge/dataplane/handler.go`, `findHarborAccess`.

**Why it's an issue.** Matching used only `sub` and `aud`. Issuer correctness
relied entirely on two other invariants: the Validator pins `iss` to the bridge's
single configured issuer (`oidc.go`), and the reconciler refuses CRs whose
`trustPolicy.issuer` disagrees with the cluster issuer (`reconciler.go:111`).
That's correct today, but the CR carries `trustPolicy.issuer` as a first-class
trust field and the handler ignored it — if either upstream invariant ever
regressed (e.g. a future multi-issuer validator), a token from one issuer could
match a CR that declared a different issuer.

**Fix applied.** `findHarborAccess` now also requires
`ha.Spec.TrustPolicy.Issuer == claims.Issuer` before matching. Consistent with
the existing invariants (no valid setup breaks) and closes the gap. Regression
test: `TestHandler_IssuerMismatch_NoMatch`.

---

## F6 — Privileged kubelet-patching init container + mutable image tags (Medium, supply chain) — RECOMMENDED

**Location:** `charts/harbor-bridge/templates/plugin-daemonset.yaml`
(`hostPID: true`, init container `securityContext.privileged: true`,
`nsenter -t 1 -m -u -i -n -p` writing `/etc/default/kubelet` and
`systemctl restart kubelet`), `values.yaml` (`plugin.patchKubelet: true` by
default), `plugin.image` / `bridge.image` referenced by **tag**, not digest.

**Why it's an issue.** With `patchKubelet: true` (the default) the plugin's init
container runs fully privileged in the host PID namespace and rewrites kubelet's
config and restarts it — that is node-root on every node. The init container runs
from `plugin.image` referenced by a mutable tag (the Dockerfiles pin their *base*
images by digest, but the chart pins the plugin/bridge images only by tag). So
anyone who can push that tag, or MITM/poison the pull, gets privileged code
execution on every node. The plugin image is also `alpine` (full shell)
specifically to run this copy/patch step, enlarging the on-node attack surface.
Much of this is inherent to "self-managed cluster, no upstream pattern," but the
defaults and integrity controls can be tightened.

**How to fix:**
- Pin `plugin.image` and `bridge.image` by **digest** in the chart (and document
  cosign/sigstore verification, ideally enforced by an admission policy).
- Consider defaulting `plugin.patchKubelet: false` (opt-in to the privileged
  path) so the privileged/hostPID/nsenter mode is a conscious choice; managed
  node images (EKS/GKE/AKS) don't need it.
- Scope the privileges: the init container only needs to write a couple of host
  files and restart one unit — narrow the mounts/capabilities rather than
  `privileged: true` where the platform allows, and gate `hostPID` strictly
  behind `patchKubelet`.

---

## F7 — Non-deterministic CR selection on overlapping matches (Low) — RECOMMENDED

**Location:** `bridge/dataplane/handler.go`, `findHarborAccess` (returns the
first list item that matches).

**Why it's an issue.** If two HarborAccess CRs share the same
`serviceAccountRef` + `audience` + issuer (misconfiguration, or two teams
granting the same SA), the handler returns whichever the cache lists first —
non-deterministic across restarts. A workload could silently receive the
narrower or broader of two permission sets depending on list order, and an
operator tightening permissions in one CR may see no effect because the other
CR wins.

**How to fix:** detect multiple matches and fail closed (403 + a logged
warning naming the colliding CRs) rather than silently picking one; or define
and document a deterministic tie-break (e.g. most-specific / oldest) and enforce
it. At minimum, emit a metric/log when >1 CR matches.

---

## F8 — OIDC validation error string logged at debug (Low) — RECOMMENDED

**Location:** `bridge/dataplane/handler.go`
(`logger.V(1).Info("token validation failed", "err", err.Error(), ...)`),
error wrapping in `bridge/dataplane/oidc.go`.

**Why it's an issue.** The wrapped go-oidc error is logged. go-oidc's verify
errors generally don't echo the full token, but the "malformed jwt"/parse paths
can include token fragments, and this is on the request hot path. It's only at
`V(1)` (debug), so low risk, but a debug-enabled bridge could write attacker-
controlled token material to logs.

**How to fix:** log the classified reason (`classifyOIDCError(err)`) rather than
the raw error string on the request path, and keep raw errors out of anything
that could contain token bytes. Confirm by reading what go-oidc emits for the
malformed/parse branches.

---

## F9 — `tls.enabled=false` and other fail-open-ish chart knobs (Info) — RECOMMENDED

**Location:** `charts/harbor-bridge/values.yaml` (`tls.enabled`), the
required-but-unset values (`plugin.matchImages`, `plugin.audience`,
`harbor.adminCredsSecret.name`).

**Why it's an issue.** `tls.enabled: false` is offered "only if terminating TLS
upstream," but the bridge (`server.go`) requires a cert/key and the plugin
(`plugin/main.go`) requires an `https://` endpoint, so the combination is
internally inconsistent and could leave operators in a confusing partial state.
Separately, an empty `plugin.matchImages` means kubelet never calls the plugin
and pulls **silently fall back to anonymous** (documented, but it's a
fail-open-shaped default worth a template-time guard). These aren't direct
vulnerabilities but they're foot-guns around the security-critical transport.

**How to fix:** fail-fast at `helm template` time on inconsistent TLS settings;
keep the existing required-value guards and make the anonymous-fallback
consequence of empty `matchImages` a hard error rather than a comment.

---

## Things checked and found OK (so the next auditor doesn't re-tread)

- **OIDC core checks** are enforced: signature + issuer pinned by go-oidc;
  `SkipExpiryCheck` is left false; `SkipClientIDCheck` is intentional and
  audience is enforced in the handler against the matched CR. `aud` string-vs-
  array parsing (`jsonAudience`) is safe.
- **No `InsecureSkipVerify`** in production code (only in `server_test.go`).
  Harbor client and OIDC HTTP client use proper CA trust.
- **Harbor ownership invariants** are checked at every write site (reconciler
  create/update/delete, janitor sweep): prefix (`OwnsRobot`) + description tag
  (`RobotBelongsToCluster`). The former adoption gap — the `harboraccess=` tag
  not being checked on adopt/delete — was the mechanism behind F2 and is now
  closed by F2's robot-name guard (reconciler + delete path).
- **Secrets are not logged**: `Config.Sanitized()` excludes admin creds;
  `auditIssuance` logs the robot username but never the password; robot Secret
  lives in the bridge namespace with tight RBAC and `0400` mounts.
- **Bridge pod hardening** is good: distroless nonroot, `runAsNonRoot`,
  `readOnlyRootFilesystem`, `drop: [ALL]`, seccomp RuntimeDefault, Secret file
  modes `0400`.
- **Repo hygiene**: `test/e2e/errored_test.tfstate` (contains a generated admin
  password) is **not** tracked by git and **is** matched by `.gitignore`
  (`test/e2e/**/errored_test.tfstate` covers the top-level file too —
  `git check-ignore` confirms). No secret is committed.
- **Server timeouts** (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`,
  `IdleTimeout`) are set — slowloris is bounded; the gap was body *size* (F1),
  not header/read time.

## Verification

`go build ./...`, `go vet ./...`, and `go test ./...` all pass with the F1, F2
(guards), and F5 fixes and five regression tests:

- `TestHandler_RejectsOversizedBody` (F1)
- `TestHandler_IssuerMismatch_NoMatch` (F5)
- `TestReconcile_SecretNameCollision_RefusesToOverwrite` (F2)
- `TestReconcile_RobotNameCollision_RefusesForeignHarborAccess` (F2)
- `TestHandler_SecretOwnerMismatch_Forbidden` (F2)

Code fixes applied this pass: F1 (body cap), F2 (collision guards + read-path
403 backstop), F5 (per-CR issuer re-check). F2's collision-free naming scheme,
F3/F4/F6/F7/F8/F9 remain Recommended/Needs-decision per their sections.
