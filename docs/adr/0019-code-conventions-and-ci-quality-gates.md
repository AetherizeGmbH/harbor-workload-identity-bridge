# 19. Code conventions and CI quality gates

## Status

Accepted

## Context

The project's coding conventions were, until now, implicit — encoded in
scattered config files (`.golangci.yml`, `.commitlintrc.json`,
`.yamllint.yml`) and in the habits visible across the tree, but never written
down in one place. New contributors (and new agent sessions) had to
reverse-engineer them from the code or rediscover them by tripping a CI job.

At the same time we added a layer of security and supply-chain tooling to CI
(Trivy, govulncheck, CodeQL, OpenSSF Scorecard, hadolint, actionlint, plus
cosign image signing and SBOM attestation). These are now *gates*, not advice:
a PR that violates them goes red. A decision that turns "advice" into "blocks
merge" is exactly the kind of commitment ADR-0001 §15 says to record.

This ADR does not invent new style — it ratifies what the existing configs and
the established code already do, so there is a single normative reference.
ADR-0001 already governs *when* to write an ADR; this one governs *how the code
is written and checked*.

## Decision

### Go source

- **Formatting is `gofmt -s`.** Enforced by the `gofmt` job
  (`.github/workflows/lint.yml`) and `make fmt`. Simplification (`-s`) is part
  of the contract, not optional.
- **golangci-lint v2 is the linter, run with the checked-in config**
  (`.golangci.yml`). The enabled set is deliberately conservative
  (`default: none` + an explicit allow-list: `errcheck`, `govet`,
  `staticcheck`, `revive`, `errorlint`, `bodyclose`, `nilerr`, `unparam`, …).
  `errcheck.check-type-assertions: true` — unchecked `x.(T)` is an error.
  Adding or removing a linter is a config change reviewed on its own merits, not
  an ad-hoc per-PR `//nolint`.
- **Every Go file carries the SPDX header** from `hack/boilerplate.go.txt`:
  `// Copyright 2026 The Aetherize Authors.` / `// SPDX-License-Identifier:
  Apache-2.0`. Generated code (deepcopy) gets it via the `controller-gen`
  `headerFile` flag (`Makefile` `generate`).
- **The Go toolchain floor is pinned in `go.mod` for security reasons, not
  taste.** The `go 1.26.0` line and its comment pin the minimum to a release
  carrying specific CVE fixes reachable from the bridge's OIDC/x509 paths.
  Lowering it is a security regression; bumps come through Renovate.
- **Package-isolation invariants are mechanically enforced and must stay
  green:**
  - `bridge/controlplane` must not import `bridge/dataplane`
    (ADR-0002) — `make verify-package-isolation`.
  - `plugin/` must not import `k8s.io` or `sigs.k8s.io`
    (ADR-0015) — `make verify-plugin-isolation`.
  Both run in `.github/workflows/test.yml`. Code that needs to cross these lines
  needs a superseding ADR first, not an import.
- **Tests run with `-race` in CI** (`go test -race -count=1 ./...`). New code
  that races fails the build; don't paper over it with `-count=1` locally.

### Commits

- **Conventional Commits, enforced by commitlint** (`.commitlintrc.json`,
  `commitlint.yml`). The `type` must be one of
  `feat|fix|perf|refactor|docs|style|test|ci|build|chore|revert`; the header is
  capped at 100 chars. semantic-release consumes these to compute the version
  and changelog (`.releaserc.json`), so the type is load-bearing — `feat`/`fix`
  cut releases, the rest don't.
- **Dependency PRs use the `deps` scope** (`fix(deps):`, `chore(deps):`) per the
  Renovate config (`:semanticCommitScope(deps)` in `renovate.json`).

### Non-Go assets

- **Workflows**: `actionlint` (shellcheck on `run:` blocks, expression typing,
  action-input validation) + `yamllint` (`.yamllint.yml`, no `--strict` —
  warnings don't fail, but `key-duplicates` and other `error` rules do).
- **Dockerfiles**: `hadolint`, matrixed over `Dockerfile.bridge` and
  `Dockerfile.plugin`.
- **OpenTofu** (`test/e2e`): `tofu fmt -check -recursive`, version-pinned to
  match `e2e.yml` so laptop and CI agree.
- **Helm chart**: `helm lint` against every test values file plus a **golden
  render** diff (`make chart-test`); intentional template changes require
  `make chart-golden-update` and a committed golden file.

### Security & supply-chain gates

- **Source scanning** runs on every PR and gates merge:
  - **Trivy** filesystem scan (`vuln,secret,misconfig`) — fails on
    `CRITICAL,HIGH` (with `ignore-unfixed: true`), uploads SARIF to the Security
    tab (`.github/workflows/trivy.yml`). Accepted misconfigurations (e.g. the
    privileged node-installer DaemonSet) are risk-accepted in
    `.trivyignore.yaml`, each entry **path-scoped and justified** — that file is
    the single audited surface for "we know, and here's why it's fine", not a
    mute button. Trivy does not auto-load the `.yaml` ignore form, so the
    workflow passes it via `trivyignores:` explicitly.
  - **govulncheck** — Go's reachability-aware vuln scanner; only fails on vulns
    actually reachable in the call graph (`.github/workflows/test.yml`).
  - **CodeQL** — dataflow SAST for Go (`.github/workflows/codeql.yml`).
  - **OpenSSF Scorecard** — supply-chain posture on the default branch
    (`.github/workflows/scorecard.yml`).
- **Released images are signed and have an attested SBOM**
  (`.github/workflows/release-images.yml`): keyless **cosign** signature
  (Sigstore Fulcio + Rekor, GitHub OIDC — no long-lived keys), a **Trivy**
  CycloneDX SBOM attested with `cosign attest`, and BuildKit SLSA provenance.
  Signature and attestation bind to the immutable image **digest**.
- **All third-party GitHub Actions are pinned to a commit digest** with a
  trailing `# vX.Y.Z` comment, and updated by Renovate's `github-actions`
  manager (`renovate.json` `pinDigests`). The one documented exception is
  `golangci/golangci-lint`'s binary-version input, which is a release-tag string
  the action reads, not a Git ref. Tool *binaries* that we intentionally track
  at latest (Trivy, govulncheck) use `@latest` and are not Renovate-pinned.

### Architecture decisions

Unchanged from ADR-0001: significant decisions get an ADR before the code lands;
ADRs are immutable once Accepted and are reversed by a superseding ADR, not
edited. The bar in ADR-0001 §15 still applies — don't ADR trivial naming or
file-layout choices.

## Consequences

**Positive:**

- One normative reference for "how do we write code here", discoverable by
  humans and agents without grepping every dotfile.
- The gates are mechanical and uniform: a green CI run means formatting,
  linting, isolation invariants, vuln/SAST scans, and supply-chain checks all
  passed. Reviewers spend their attention on design, not style nits.
- The supply-chain posture (digest-pinned actions, signed images, attested
  SBOMs) is now a *contract* a downstream consumer can verify, not a one-off.

**Negative / trade-offs:**

- More can turn a PR red, including findings in code the PR didn't touch (a
  newly disclosed CVE flips Trivy/govulncheck red on an untouched dependency).
  That is intended — it surfaces the issue at the next PR rather than never —
  but it means an unrelated change can be blocked by a dependency bump.
- Keeping the tool digests current depends on Renovate actually running; a
  stalled Renovate means slowly drifting pins.
- `@latest` Trivy/govulncheck binaries can change behavior between runs (a new
  release adds a check). Accepted deliberately: for security scanners, "newest
  rules" beats "reproducible miss". Pin them here if that ever bites.

## Alternatives considered

- **Leave conventions implicit in the dotfiles.** Status quo. Rejected: it works
  for the original author and fails everyone else; the configs don't explain
  *why* (e.g. the go.mod floor, the isolation invariants) and don't mention the
  cross-cutting CI gates at all.
- **A `CONTRIBUTING.md` instead of an ADR.** A contributing guide is a fine
  *surface* for some of this, but the decision to make these checks blocking is
  an architectural commitment with trade-offs (the "negatives" above) — that
  belongs in the immutable ADR log. A future `CONTRIBUTING.md` can link here.
- **Looser gates (warn, don't block).** Rejected for the security scanners: a
  warning in CI output is a warning nobody reads. The whole point of ADR-0018's
  and the F2 audit's lineage is that security boundaries fail *closed*.
