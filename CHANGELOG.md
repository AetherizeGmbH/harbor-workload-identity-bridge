## [0.3.3](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.2...v0.3.3) (2026-07-17)

## [0.3.2](https://github.com/AetherizeGmbH/harbor-workload-identity-bridge/compare/v0.3.1...v0.3.2) (2026-07-09)

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
