# 20. Harbor compatibility is tested, not asserted

## Status

Accepted

## Context

The bridge speaks to exactly one external surface: Harbor's robot-account
resource under `/api/v2.0`, through the pinned `goharbor/go-client`
(`bridge/controlplane/harbor/client.go`). The whole call set is six
operations:

| Op                | Endpoint                       |
|-------------------|--------------------------------|
| Create            | `POST   /robots`               |
| Delete            | `DELETE /robots/{id}`          |
| List (paginated)  | `GET    /robots`               |
| GetByID           | `GET    /robots/{id}`          |
| RefreshSecret     | `PATCH  /robots/{id}`          |
| UpdatePermissions | `PUT    /robots/{id}`          |

We make **no** registry/v2 token calls (ADR-0013 returns robot Basic Auth and
lets containerd run the docker-registry handshake), and touch no project, user,
or auth-config API.

The README needs to state which Harbor versions this works against. The wrong
way to do that is to read the Harbor changelog and assert a range. These
endpoints don't drift in *signature* — `/api/v2.0` is backward-compatible and
the SDK version freezes the wire shapes — so the only failure mode is **"this
capability didn't exist yet"** in an older server. Three concrete gates:

- **System-level robots** (`Level: "system"`, multi-project permissions, the
  `robot$` prefix). This is the robot-account-2.0 redesign; older Harbor only
  has project-scoped robots with a different create shape.
- **`RefreshSecret`** (`PATCH /robots/{id}`), which the persistent-robot
  password rotation (ADR-0003) depends on. This landed *after* system robots,
  so it — not the create path — is the likely floor.
- **The `robot$` read-back asymmetry** (ADR-0014): Harbor changed whether the
  listed name carries the prefix. `GetByName` already matches both forms, so a
  passing run across the range is what *proves* that handling holds rather than
  us asserting it.

A claim like "RefreshSecret exists since Harbor 2.x" is exactly the kind of
unverified assertion this project's working agreement forbids. We have a full
kelet-driven e2e harness (`make e2e`) that builds a fresh kind cluster, installs
Harbor via the official Helm chart, and exercises the entire reconcile → robot →
pull chain. The Harbor version is already a module variable
(`test/e2e/modules/harbor/main.tf`, `var.version_harbor`). So the cheap, honest
answer is: **run the real chain on each version and record the result.**

### Chart version is not Harbor version

The install knob is the **Helm chart** version, which does not equal the Harbor
app version. The mapping is clean and stable (`chart 1.N.x → Harbor 2.(N-4).x`,
e.g. chart `1.19.1` → Harbor `2.15.1`, chart `1.13.5` → `2.9.5`), but it must be
*resolved at runtime* (`helm show chart harbor/harbor --version <X>`) so the
published table can never drift from what actually ran.

## Decision

Make the e2e Harbor version a first-class parameter and drive a scheduled CI
matrix that fills the README compatibility table from real runs.

1. **Parameter.** `make e2e HARBOR_CHART_VERSION=<chart>` forwards
   `TF_VAR_version_harbor` into the harness; the file-scope variable is threaded
   into the `run "harbor"` block. The default stays the current pin, so an
   unparameterised `make e2e` is unchanged.

2. **Matrix scope: floor + two mid + ceiling, not every minor.** The contract is
   defined by its endpoints, and the middle of the range is low-risk precisely
   because the endpoints don't change there — so floor and ceiling carry almost
   all the signal and two mid-points guard against a surprise. The seed list is
   chart `1.13.5 / 1.15.2 / 1.17.5 / 1.19.1` (Harbor `2.9 / 2.11 / 2.13 / 2.15`).
   The list is a plain matrix array, edited as the range moves.

3. **Floor is discovered, not declared.** The floor is the lowest version whose
   run is green; the ceiling is the highest. A green floor run is a signal that
   the *true* floor may be lower — probe down by lowering the bottom matrix entry
   in a later run. A red floor run raises the floor. The README marks which row
   is the current floor/ceiling. Distinguish two kinds of red: an **API** failure
   (robot create/refresh rejected) is a genuine floor; a **harness** failure (old
   chart won't deploy on the current kind/k8s) is a harness limit, noted as such —
   we can't *claim* a version we can't stand up, but it isn't the reconciler's
   floor.

4. **Trigger: weekly schedule + `workflow_dispatch`, not per-PR.** A full e2e is
   ~5 min and stands up a real Harbor; running four on every PR is wasteful and
   multiplies flake. PR e2e (`e2e.yml`) stays single-version on the ceiling. The
   matrix (`harbor-compat.yml`) runs weekly and on demand, so a new upstream
   Harbor release is picked up within a week.

5. **Table is auto-committed via PR.** Each matrix leg writes a result record
   (`chart`, resolved `appVersion`, pass/fail, date); an aggregation job renders
   the table between markers in the README and opens a PR. The table is therefore
   always backed by a run, and a human reviews before merge.

## Consequences

**Positive:**

- The supported range is a property of green CI, not a maintainer's memory. The
  floor's binding constraint (RefreshSecret / system robots) is proven, and the
  ADR-0014 `robot$` handling is re-proven, on every covered version.
- Adding/dropping a version is a one-line matrix edit; the table follows.
- The chart→app resolution at runtime means the published Harbor versions can't
  silently diverge from what was tested.

**Negative / trade-offs:**

- Four real Harbor stand-ups per week is real CI minutes. Bounded by running
  weekly (not per-PR) and by four legs (not every minor).
- The matrix tests the *whole chain*, so a harness-side incompatibility on an old
  chart reads as a red even when the bridge's API usage is fine. Mitigated by the
  API-vs-harness distinction in §3, surfaced in the run log and the table note.
- The table reflects only versions in the matrix; the prose states that
  in-between minors are inferred from the endpoint contract, not individually run.

## Alternatives considered

- **Assert the range from the Harbor changelog / API docs.** Zero CI cost, but
  it's the unverified assertion the working agreement forbids, and it cannot
  catch behavioural drift (e.g. the `robot$` asymmetry) that only a real pull
  proves.
- **Every supported minor on every PR.** Maximal coverage, but multiplies the
  slowest, most flake-prone job by the full range on every change for signal the
  endpoint contract already gives us. Rejected for cost/flake.
- **Unit-test against a recorded Harbor API fixture per version.** Cheaper than
  e2e, but a fixture is just the assertion in another form — it proves we match a
  *recording*, not a running server, and never exercises the containerd pull.
- **Maintain the table by hand from manual runs.** No new machinery, but the
  table drifts the moment someone forgets, which is the exact failure this item
  exists to kill.

[adr-0003]: 0003-persistent-robots-per-harboraccess.md
[adr-0013]: 0013-return-robot-basic-auth-credentials.md
[adr-0014]: 0014-harbor-robot-dollar-prefix-handling.md
