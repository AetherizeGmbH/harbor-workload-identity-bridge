## [0.11.4](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.11.3...v0.11.4) (2026-09-28)

### Bug Fixes

* **installer:** roll back rejected kubelet restarts, patch the env file kubelet reads ([#141](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/141)) ([8c1e2c0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/8c1e2c018295c5cb4225f9bf45f218bd06fc1185))

## [0.11.3](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.11.2...v0.11.3) (2026-09-28)

### Bug Fixes

* **controlplane:** suspend refused robots, live rotation reads, finalizer ownership ([#137](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/137)) ([4192b0c](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/4192b0ca4b004d9fa2356b7c2a02dc7bb7565f88))

## [0.11.2](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.11.1...v0.11.2) (2026-09-28)

### Bug Fixes

* **bridge:** graceful shutdown, real readiness, rotating Harbor admin credentials ([#136](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/136)) ([d3d0625](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/d3d062562868d4970aa7f2c8d3f339da822b460c))

## [0.11.1](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.11.0...v0.11.1) (2026-09-28)

### Bug Fixes

* **dataplane:** resilient JWKS keys, audited refusals, no credentials for deleting objects ([#135](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/135)) ([ade34e9](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/ade34e92514fcd1e8512af277ecfd51bf6cff374))

## [0.11.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.10.2...v0.11.0) (2026-09-28)

### Features

* **chart:** configurable plugin provider name for several installs per cluster ([#130](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/130)) ([ac4591b](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/ac4591b6c0a3a19b89e5bc290b1a246d57d0d263))

## [0.10.2](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.10.1...v0.10.2) (2026-09-28)

### Bug Fixes

* **harbor:** no redirects, no URL credentials, fail closed on prefix and name mismatches ([#134](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/134)) ([0b7b7a6](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/0b7b7a63b7162964f457c8af45bdbf7bad3d8b2d))

## [0.10.1](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.10.0...v0.10.1) (2026-09-27)

### Bug Fixes

* **api:** admit only Go durations as tokenTTL and require spec ([#129](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/129)) ([19066ef](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/19066efd14c0a49e5f8bdbc5f2a756ce6fbaea13))
* **plugin:** pin the bridge wire keys, test the TLS server name, correct the docs ([#128](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/128)) ([7b00f8c](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/7b00f8cd523750b7fbc26e49226fc8f2caee8ca4))

## [0.10.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.9.0...v0.10.0) (2026-09-27)

### ⚠ BREAKING CHANGES

* **dataplane:** the bridge refuses ServiceAccount tokens that live
longer than bridge.tokenValidation.maxLifetime (1h) or are not bound to
a pod. Kubelet's tokens are unaffected; bind hand-minted tokens with
--bound-object-kind=Pod --bound-object-name=<pod> and --duration <= 1h.

* docs: close threat-model item O3 with ADR-0028

- Threat model: T1 and T2 name the lifetime cap and the pod binding as
  mitigations; new accepted risk A5 keeps the residual (a stolen
  kubelet token stays usable for at most its remaining hour after its
  pod is gone, since only TokenReview checks the bound object); the
  logged-events list names the new categories; O3 is marked closed.
  The model requires a new approval for changes, so its status is
  proposed until the maintainer approves this revision.
- README: the O3 item leaves "Open decisions (TODO)", whose intro now
  lists O2 and O5; phase 10 lists the token lifetime cap and ADR-0028.

* docs: correct where denials are logged and the Harbor CA hint

Found while documenting ADR-0028 in the same sections.

- SECURITY.md said failures log at V(1). Denials are credential denied
  lines on the fixed-info audit logger; a missing robot Secret (503) and
  API errors (500) go to the regular log.
- HOW-TO-TEST.md called BRIDGE_HARBOR_CA_FILE a backlog item; it shipped
  in 0.8.0 (chart: harbor.caSecret).

* refactor(bridge): build the validator config in one tested function

BRIDGE_REQUIRE_POD_BOUND_TOKEN and dataplane.Config's
AllowNonPodBoundTokens mean opposite things, and nothing tested the
negation between them: bridge/cmd had no tests, the validator and the
env parsing were tested only on their own, and the e2e sends only
kubelet's tokens, which are pod-bound either way. Dropping the "!" left
every test green while the default deployment accepted tokens bound to
no pod.

validatorConfig now maps the bridge config onto the validator's, and a
table test follows each setting from the env var the chart renders to
the validator field; the inverted mapping fails it. The startup warnings
read the same validator config, so they report what is enforced.

* docs: bound a redeemed token's reach by the password rotation

SECURITY.md, ADR-0028 and threat-model item A5 said a stolen token stops
working when it expires and that sustained abuse needs a new token every
hour. One accepted token returns the robot's password, and Harbor
accepts that password until the next rotation, which the bridge
schedules PasswordRotationInterval (24h) plus RotationSafetyMargin (1m)
after the previous one (bridge/controlplane/contract.go). The cap bounds
how long a token can be redeemed, not how long its credentials work, and
sustained abuse costs about one TokenRequest a day. The passages now say
so and point to deleting the password Secret for an immediate rotation.

* fix(chart): default bridge.tokenValidation for --reuse-values upgrades

### Bug Fixes

* **dataplane:** cap token lifetime and require pod-bound tokens ([#121](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/121)) ([d79a447](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/d79a44789156984c0fa00d1f01bcb46ebd8e3de6))

## [0.9.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.8.0...v0.9.0) (2026-09-24)

### Features

* **chart:** optional plugin namespace with the CA delivered by trust-manager ([#110](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/110)) ([9dd46cf](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/9dd46cf1f3b29af3ae7f45a67c7ef3388e5282a3))

### Bug Fixes

* **chart:** pin images by digest, and correct stale security guidance ([#113](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/113)) ([3bce496](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/3bce4964bbe808d06cbded6f48f2ff3dd1bd3f23))

## [0.8.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.7.0...v0.8.0) (2026-09-24)

### ⚠ BREAKING CHANGES

* **bridge:** harbor.url must use https, or set
harbor.allowInsecureHTTP: true.

### Bug Fixes

* **bridge:** require https to Harbor, serve only bridge-written Secrets, trim RBAC ([#112](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/112)) ([1d5f7fd](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/1d5f7fdc47fd2ec96f332fe64d8c0fe59a448545))
* **plugin:** never follow redirects and refuse unusable bridge responses ([#111](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/111)) ([73d63bf](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/73d63bf7eadb2a3640df52a13e45c5f8c91e7ae2))

## [0.7.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.6.0...v0.7.0) (2026-09-24)

### Features

* **dataplane:** attributed audit log and a per-source rate limit ([#107](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/107)) ([cdddf17](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/cdddf17fe66e71c1e261c2a0c753b4e14c77e10f))

### Bug Fixes

* **installer:** stricter kubelet discovery and node path validation ([#108](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/108)) ([ab9f12c](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/ab9f12cb4018e0c757c390774cf33c6fe776b676))

## [0.6.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.5.5...v0.6.0) (2026-09-24)

### ⚠ BREAKING CHANGES

* **bridge:** BRIDGE_AUDIENCE is required (the chart sets it from
plugin.audience). HarborAccess objects whose trustPolicy.audience differs
from plugin.audience stop receiving credentials and report
AudienceMismatch; set their audience to plugin.audience.

### Features

* **bridge:** serve one audience and a selected set of HarborAccess objects ([#106](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/106)) ([a5fb5f9](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/a5fb5f93b919a899a4920e7ce4384680d6e49d29))

## [0.5.5](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.5.4...v0.5.5) (2026-09-24)

### Bug Fixes

* **api:** accept only real Harbor project names in HarborAccess grants ([#102](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/102)) ([2021a2a](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/2021a2adcc3a2903720b4a36b85363fa9d289590))
* **installer:** leave an already-merged provider config byte-identical; add gosec and fuzzing ([#105](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/105)) ([39fb04a](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/39fb04ace2b762770247fe06a8fb5bacd5ac3da6))

## [0.5.4](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.5.3...v0.5.4) (2026-09-24)

### Bug Fixes

* **dataplane:** send the bridge token only to the apiserver and rate-limit key fetches ([#101](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/101)) ([fb4b6b2](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/fb4b6b2f3151ad6f0a8d1f7e2030c6c329155820))

## [0.5.3](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.5.2...v0.5.3) (2026-09-24)

### Bug Fixes

* **harbor:** never dump credentials and bound every Harbor call ([#100](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/100)) ([fbddc42](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/fbddc42d65dea5ac3edb0ab86ec12d4b10f7a413))
* **installer:** never follow symlinks when reading or replacing host files ([#99](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/99)) ([52d185c](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/52d185c800ed6514afe65b4c5d8c2ddcb52b2356))

## [0.5.2](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.5.1...v0.5.2) (2026-09-24)

### Bug Fixes

* **deps:** update kubernetes go modules ([#93](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/93)) ([c1b4403](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/c1b4403ecad5685eca9271de4c77fa7c0875c377))

## [0.5.1](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.5.0...v0.5.1) (2026-09-24)

### Bug Fixes

* **deps:** update go modules ([#68](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/68)) ([9573936](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/9573936ac5ae8f4fdc13a907688dcd68b552c825))

## [0.5.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.4.0...v0.5.0) (2026-09-24)

### ⚠ BREAKING CHANGES

* **installer:** plugin.patchKubelet is removed; setting it fails
templating. Use plugin.install.mode (auto replaces true, none replaces
false). tls.enabled=false now requires tls.existingSecret.

### Features

* **installer:** node installer modes and an optional plugin DaemonSet ([#89](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/89)) ([7fb4962](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/7fb4962f93afe1e5933c6a5a3fae3b4473d05aca)), closes [#46](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/46)

## [0.4.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.8...v0.4.0) (2026-09-24)

### ⚠ BREAKING CHANGES

* **bridge:** /metrics moved from the credential port (8443) to
plain HTTP on port 8080 (Service <release>-metrics). HarborAccess names
are limited to 63 characters; apply the CRD manually on upgrade.

### Bug Fixes

* **bridge:** level-triggered robot lifecycle and multi-replica data plane ([#88](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/88)) ([7c579b6](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/7c579b6d919069b30f00fc8806459cf65fb76ef4))

## [0.3.8](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.7...v0.3.8) (2026-09-24)

### Bug Fixes

* **deps:** update kubernetes go modules to v0.36.4 ([#70](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/70)) ([e5b0c5f](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/e5b0c5fc3b2aadd8bb075f69ca8af8f27787c6c5))
* **deps:** update kubernetes go modules to v0.37.0 ([#74](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/74)) ([690bbf3](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/690bbf320065298429cc2e407734d170ad506efe))
* **deps:** update module sigs.k8s.io/controller-runtime to v0.25.0 ([#78](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/78)) ([5eb847f](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/5eb847f203819638f72fcc790d40ffb72bd505fb))
* **deps:** update module sigs.k8s.io/controller-runtime to v0.25.1 ([#81](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/81)) ([7c865fa](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/7c865fad95d0b4e6897cd55e4b846eb2292a7ca2))

## [0.3.7](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.6...v0.3.7) (2026-07-27)

### Bug Fixes

* **deps:** update module github.com/go-openapi/runtime to v0.33.0 ([#55](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/55)) ([eeecb61](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/eeecb61c30d9f4322bb2c96d128b438cba549ae7))

## [0.3.6](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.5...v0.3.6) (2026-07-24)

### Bug Fixes

* **deps:** update module github.com/prometheus/client_golang to v1.24.1 ([#54](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/54)) ([06ffa9a](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/06ffa9a1c170411f38446f0d98ac6d3fd9de7243))

## [0.3.5](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.4...v0.3.5) (2026-07-23)

### Bug Fixes

* **deps:** update kubernetes go modules to v0.36.3 ([#53](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/53)) ([fcf486f](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/fcf486fa5f40070c3b1eb5b2dc71191ffa05f72b))

## [0.3.4](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.3...v0.3.4) (2026-07-22)

### Bug Fixes

* **deps:** update go modules ([#50](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/50)) ([8102da0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/8102da002a74486c849552ae9d888af78d4077c7))

## [0.3.3](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.2...v0.3.3) (2026-07-17)

### Bug Fixes

* **deps:** update module github.com/go-openapi/runtime to v0.32.5 ([#49](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/49)) ([b3d6a5b](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/b3d6a5b53626316bcf315ebacc15f57851f40eca))

## [0.3.2](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.1...v0.3.2) (2026-07-09)

### Bug Fixes

* **deps:** update go modules ([#39](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/39)) ([0b3e93c](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/0b3e93cbcdc383de50c8707bffa9e6682466dbfd))

## [0.3.1](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.0...v0.3.1) (2026-06-21)

### Bug Fixes

* **deps:** update go modules ([#21](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/21)) ([602ccdb](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/602ccdbbedec776c0b87bb8986ad564c0412c89f))
* **deps:** update kubernetes go modules to v0.36.2 ([#19](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/19)) ([9c784c0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/9c784c043e01aaf45c81573c17c8806dbd29bab4))

## [0.3.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.2.8...v0.3.0) (2026-06-07)

### ⚠ BREAKING CHANGES

* **release:** in 0.x is a MINOR bump, not a 1.0.0 release. Map breaking to
minor so the naming work releases as 0.3.0.

Also drops the erroneous 'chore(release): 1.0.0' commit (no v1.0.0 tag was
ever published; the tag push was rejected, leaving main half-released).
* **bridge:** Harbor robot names and robot-password Secret names change
from dash-delimited to dot-delimited. Safe now (nothing deployed); post
-release this would require a robot/Secret rename migration.

go build/vet/test and -race all pass.

* chore(docs): update readme version on release

Signed-off-by: Karsten Siemer <karsten.siemer@aetherize.com>

* test(naming): pin cross-package Secret-name contract; refresh stale F2 comments

- Add TestRobotSecretName_ContractPinned in the data plane: nothing pinned
  dataplane.robotSecretName against controlplane.secretNameFor, and this pass
  changed both. Drift would make the plugin read a Secret the reconciler never
  wrote (every pull 503s); now caught at test time.
- Update F2/defense-in-depth test comments and a labels_test comment that still
  described the old dash-joined / hyphen-prefix scheme as current.

* fix(naming): hash-truncate overflowing Secret names; unify overflow separator

Two follow-ups from the self-review of the dot-delimiter change:

- Secret names had no length handling: "robot-<haNs>.<haName>" can exceed the
  253-char Kubernetes object-name limit (ns<=63 + a name up to 253), which made
  the reconciler loop forever on a "name too long" API error and the CR never
  go Ready. Add deterministic hash-truncation mirroring harbor.RobotName's
  overflow path, in a shared controlplane helper (robotSecretNameFor) and its
  byte-identical data-plane mirror (dataplane.robotSecretName, ADR-0015).
- Robot-name overflow path joined the disambiguating hash with '-' while the
  rest of the scheme uses '.'. Switch to '.' for consistency (budget already
  reserved one separator char; still a valid Harbor name).

Tests: TestRobotSecretNameFor (controlplane) and the extended
TestRobotSecretName_ContractPinned (dataplane) cover overflow (<=253,
deterministic, distinct-inputs-distinct-outputs, prefix preserved). PHASES.md
TODO note removed. go build/vet/test and -race all pass.

* fix(dataplane): deterministic HarborAccess match, reject empty audience/issuer

findHarborAccess returned whichever CR k8s List yielded first. List order is not
stable across the two HA replicas or restarts, so a duplicate (sub,iss,aud) could
flip a workload between a more- and a less-privileged robot. Collect all matches,
select the namespace/name-sorted first, and log the ambiguity.

Also reject any CR with an empty trustPolicy.audience/issuer and ignore empty aud
token entries: defense-in-depth so the auth decision no longer rests solely on the
CRD MinLength=1 markers. Both changes have negative-control-verified tests.

Bump the go toolchain to 1.26.4 for the GO-2026-5037/5038/5039 stdlib fixes
(govulncheck: 3 vulnerabilities -> 0).

Fold the audit's residual operator decisions into SECURITY.md: HarborAccess
authorship is cluster-privileged (the bridge applies no authz to CR contents),
pin bridge/plugin images by digest, /metrics is unauthenticated on the NodePort
port, and the tls.enabled footgun. Correct stale ADR-0018 ownership-prefix docs in
NOTES.txt and values.yaml and the now-hardened bridge pod-security row.

Remove the working AUDIT.md; its actionable items now live in SECURITY.md.

### Bug Fixes

* **bridge:** guard HarborAccess naming collisions and bound credentia… ([#9](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/9)) ([2e98560](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/2e98560bb7f81ea5a5f7d28186b81ef586b106fa))

### CI

* **release:** bump breaking changes to minor, not major (pre-1.0) ([cc82f1c](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/cc82f1c8bad3a47d79cc59eda7176335c74afed1)), closes [#9](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/9)

## [0.2.8](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.2.7...v0.2.8) (2026-06-06)

### Bug Fixes

* **ci:** bump kind from v0.24.0 to v0.32.0 for containerd 2.x ([6183780](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/6183780e6c26537c65bdd75bce66337f99315b33))

## [0.2.7](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.2.6...v0.2.7) (2026-06-06)

### Bug Fixes

* **e2e:** bridge_install pulls from build_images, no silent defaults ([0051dd3](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/0051dd358cf66db0b48fddb0e98736cdac68fa1b))

## [0.2.6](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.2.5...v0.2.6) (2026-06-06)

### Bug Fixes

* **deps:** update go modules ([fc8374d](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/fc8374d125c2f35078b2c053ce30af10a42a0304))

## [0.2.5](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.2.4...v0.2.5) (2026-06-06)

### Bug Fixes

* **deps:** update kubernetes go modules ([9678954](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/9678954fd71f130a13e9d2c8e50ed33085cc54b3))

## [0.2.4](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.2.3...v0.2.4) (2026-06-06)

### Bug Fixes

* **e2e:** kind_load must wait for containerd_hosts restart ([273c6e6](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/273c6e627d8b0dc8dd17d43eb6195fe58a629de4))

## [0.2.3](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.2.2...v0.2.3) (2026-06-06)

### Bug Fixes

* **ci:** Renovate uses GitHub App installation token ([d289be0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/d289be02c6633a2d59c4fdd638c7af89382a5a12))

## [0.2.2](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.2.1...v0.2.2) (2026-06-06)

### Bug Fixes

* **ci:** grant renovate token statuses/checks/actions read ([31294c2](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/31294c2759e7cf4e9a787ab7bd22c4d5f88a7fc3))
* **renovate:** exclude golangci/golangci-lint binary from digest pinning ([8a8293a](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/8a8293a7a8c325ed10546d7efc0883b8050f3754))

## [0.2.1](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.2.0...v0.2.1) (2026-06-05)

### Bug Fixes

* **bridge:** restore Watch event decoding and immediate finalizer requeue ([9c8316b](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/9c8316b757e6498e47f7328bd1d77a5d6928d3bb))
* **lint:** migrate golangci-lint to v2 schema and clear all findings ([d0d24a3](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/d0d24a3ad932034d93eee778b8357f84abb9c073))

## [0.2.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.1.1...v0.2.0) (2026-06-05)

### Features

* **e2e:** hostname-based Harbor + cert distribution + helper modules ([2b87199](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/2b8719927d4c74d00d7298fc8dd36c94987a87df))

### Bug Fixes

* **bridge:** emit cacheKeyType=Registry; chart RBAC for kubelet audience ([a43dd79](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/a43dd79f43d17562ab5e556dcfb4e1084cd6dafe)), closes [#1](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/issues/1)

## [0.1.1](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.1.0...v0.1.1) (2026-06-04)

### Bug Fixes

* **release:** clean image names + correct org slug ([b1c92b5](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/b1c92b52e2e558824ae23f3f0f476f0129fd370b))

## [0.1.0](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.0.7...v0.1.0) (2026-06-04)

### Features

* **release:** publish chart as OCI artifact to ghcr.io ([0ec0b22](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/0ec0b22752be8503b2b13944514b74fe73fb99c2))

## [0.0.7](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.0.6...v0.0.7) (2026-06-04)

### Bug Fixes

* **release:** grant actions: write so dispatch can fire downstream jobs ([bc268ea](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/bc268eae1f4620b5dad4173e1d259cd2c85cf69a))

## [0.0.6](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.0.5...v0.0.6) (2026-06-04)

### Bug Fixes

* **release:** unpin semantic-release plugin majors ([ac52d19](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/ac52d19186ad1fadcb8369352b64273ecb1576b3))
* **release:** use cycjimmy/semantic-release-action to expose outputs ([ebf06f1](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/ebf06f1b6e1a077f54c8bea5169fc07fb00de23f))

## [0.0.5](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.0.4...v0.0.5) (2026-06-04)

### Bug Fixes

* **release:** make release-images concurrency group per-tag ([8fb51a4](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/8fb51a433977b25e168113e9a407be6303289fb4))

## [0.0.4](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.0.3...v0.0.4) (2026-06-04)

### Bug Fixes

* **release:** dispatch images on semantic-release + cross-compile Dockerfiles ([05a64bd](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/05a64bd731bd5ff2aac515186b3af804d677da74))

## [0.0.3](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.0.2...v0.0.3) (2026-06-04)

### Bug Fixes

* **ci:** regen goldens for 0.0.2 ([3613a69](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/3613a6900f54144264a7cac36dc6a5a70d08105f))
* **ci:** regen goldens on release + golangci-lint-action v7 ([8bccbf3](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/8bccbf3df7ab0e4fd6423f677abc89e123ad381b))

## [0.0.2](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.0.1...v0.0.2) (2026-06-04)

### Bug Fixes

* **ci:** bump golangci-lint v2.12.2, helm v4.2.0, drop yamllint --strict ([7677473](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/7677473b7d7a7918eea91d9867f8038816b508b0))

## [0.0.1](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.0.0...v0.0.1) (2026-06-04)

### Bug Fixes

* **bridge:** data race in Server between Start() and Addr() ([0dbe857](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/0dbe857f115812b668f4ae29caf0b6e42169c344))
* **ci:** pin kube-version, bump golangci-lint, tofu fmt, dedupe values.yaml plugin key ([9799898](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/commit/9799898c21a6fc33c3649a0d6880531ff5c3c415))
