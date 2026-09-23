# 23. Level-triggered robot lifecycle, rotation-safe credential caching, and complete revocation

## Status

Accepted. Amends ADR-0003 (when passwords rotate), ADR-0007/0013 (how long
kubelet may cache), ADR-0012 (which robots the janitor considers), ADR-0014
(where the robot prefix is handled).

## Context

A review of the robot lifecycle (audit 2026-07-06 findings C1, H1, H2, M1–M4,
plus the "robots not removed with the finalizer" and "e2e fails around the
rename" reports) found that several behaviours were wrong in ways the unit
tests could not see:

1. **Spec changes never reached Harbor.** `PUT /robots/{id}` omitted the robot
   name. Harbor's `updateV2Robot` rejects any body whose `name` differs from the
   stored on-wire name (`robot$…`) with 400 "cannot update the level or name of
   robot" (verified in goharbor/harbor `src/server/v2.0/handler/robot.go`). On
   top of that, the reconciler advanced `status.observedGeneration` on the
   *error* path, so the retry saw "generation unchanged", skipped the update,
   and reported `Ready=True` while Harbor still held the old grants. A
   revocation by CR edit was silently not a revocation.
2. **The same PUT also overwrote `disable`.** Harbor copies `params.Robot.Disable`
   into the stored robot, so an update without the field re-enables a robot an
   administrator disabled.
3. **Every spec edit rotated the password**, and the scheduled daily rotation
   rotated it at an arbitrary moment. kubelet caches the credentials the plugin
   returns for `cacheDuration` (the CR's `tokenTTL`, default 1h) and does not
   drop them on an authentication failure. Every rotation therefore failed the
   pulls of every node that had cached the old password, for up to one
   `tokenTTL`.
4. **Changing `serviceAccountRef` leaked the old robot** with a valid password
   (the janitor only deletes robots whose HarborAccess is gone), and deleting a
   HarborAccess revoked only the robot its *current* spec maps to. Robots
   created by 0.2.x under the dash-delimited names (pre-ADR-0018) were never
   matched by the dot-terminated ownership prefix at all, so they leaked
   forever.
5. **Nothing watched the robot Secret.** `Owns(&corev1.Secret{})` maps events
   through owner references, which cannot cross namespaces (HarborAccess in a
   tenant namespace, Secret in the bridge namespace), so a deleted Secret stayed
   broken until the next informer resync.
6. **The janitor judged robots against the informer cache.** A cache lagging a
   `serviceAccountRef` edit could make it delete the robot the reconciler had
   just created for the new identity.

## Decision

### Level-triggered reconcile

Every pass reads the robot back from Harbor — including its permissions,
`disable` flag, and expiry — and converges it to the spec:

- `harbor.Client.Update(current, description, perms)` sends `current.WireName`
  verbatim and echoes `current.Disabled`. An administrator's disable is
  respected and surfaced as `Ready=False, reason=RobotDisabled` (re-checked every
  `ResyncInterval`), never undone.
- `harbor.PermissionsMatch` compares the normalized grant sets; a robot carrying
  any grant the bridge never writes (another resource, kind, or a deny) does not
  match and is rewritten. Drift made in the Harbor UI is reverted.
- `status.observedGeneration` advances **only** in `markReady`. It now means
  "this generation is fully applied". Error paths set only the condition's
  `observedGeneration`.
- A 409 on create re-fetches and adopts (same ADR-0009 checks as before); the
  adopted robot then goes through the same convergence, so it can no longer be
  marked Ready with foreign grants (audit M4).

### Rotation is scheduled and promised

- A spec edit **never** rotates. Harbor applies the new grants to the existing
  robot and its tokens; the password stays valid.
- The control plane stamps every robot Secret with
  `harbor.aetherize.io/rotation-not-before` (RFC 3339): the instant before which
  it will not rotate that password. After a rotation it is one
  `PasswordRotationInterval` (24h) out.
- The reconciler rotates only once `now ≥ rotation-not-before +
  RotationSafetyMargin` (1 minute) and requeues itself for exactly that instant
  (capped at `ResyncInterval`, 1h, with a per-object offset derived from the UID
  so many objects do not resync in lockstep). The rotation no longer depends on
  controller-runtime's implicit 10h resync (audit M2).
- The data plane caps the cache duration it returns at
  `min(tokenTTL, rotation-not-before − now)`, rounded down to whole seconds;
  after the promise it returns `0` (do not cache). No kubelet cache can outlive
  a scheduled rotation.
- Rotation is still forced when the stored password cannot be valid: the Secret
  is missing or incomplete, its username is another robot's, or the robot was
  re-created under the same name (`harbor.aetherize.io/robot-id` differs from
  Harbor's ID). A forced rotation can still break caches; it only happens when
  the credentials were already broken.
- Secrets written by older bridges carry no annotations. The reconciler
  backfills them from `status.robot.lastRotated` without rotating.

### Revocation is complete

A robot is owned by a HarborAccess when all three hold (`robotOwnedBy`):

1. its name is in the cluster's ownership prefix — `bridge-<cluster>.` or the
   legacy `bridge-<cluster>-` without a dot (`harbor.OwnsLegacyRobot`);
2. its description's cluster tag equals the cluster exactly — the only layer
   that separates cluster `prod`'s legacy robots from cluster `prod-eu`'s;
3. its description names exactly that HarborAccess.

- **Deletion** (finalizer) lists all robots and deletes every one the
  HarborAccess owns, then its Secret (unless stamped for another HarborAccess),
  then releases the finalizer via an optimistic-lock merge patch. While Harbor
  is unreachable the finalizer holds, and the condition
  `Ready=False, reason=DeletionBlocked` tells the user why and how to force it.
- **Identity change**: after the new robot and Secret are in place, a pass that
  saw a spec change, a new robot, or a replaced robot deletes the HarborAccess's
  other robots (it pages through all robots, so it does not run on every pass).
- **Janitor** (leader only, every 5 minutes): deletes owned robots whose
  HarborAccess is gone, legacy robots of live HarborAccess objects, and robots
  whose owner's current `serviceAccountRef` maps to a different name. It reads
  owners with the **uncached** API reader after listing the robots, so it never
  acts on a spec older than the robot. It also deletes robot Secrets of this
  cluster whose HarborAccess is gone.

### One definition of the Secret contract

`bridge/internal/robotsecret` defines the Secret name, labels, annotations and
data keys. The control plane and the data plane both import it. ADR-0002
forbids the control plane from importing the data plane; a shared leaf package
imported by both does not violate that, and it removes ~40 lines of duplicated
security-critical naming (the plugin's deliberate duplication, ADR-0015, is
unaffected). The reconciler watches Secrets and maps them to their HarborAccess
through these labels (audit M1).

### Robot prefix handled by the client

The Harbor client strips the configured robot name prefix
(`BRIDGE_HARBOR_ROBOT_PREFIX`, chart `harbor.robotNamePrefix`, default `robot$`)
on every read path: `Robot.Name` is the internal name, `Robot.WireName` the
on-wire name used for Basic Auth and for updates. `GetByName` asks Harbor for
`q=name=<internal name>` (Harbor stores names without the prefix, so the filter
is prefix-agnostic) and falls back to a full scan. This supersedes ADR-0014's
"normalize in `OwnsRobot`/`GetByName`" and supports Harbor instances whose
administrator changed the prefix.

### HarborAccess names are DNS labels

The CRD rejects `metadata.name` longer than 63 characters (CEL on the root) and
the reconciler reports older objects with longer names as `InvalidSpec`. The
name is a label value on the Secret; a longer one could never be reconciled
(audit M3).

## Consequences

- Permission grants and revocations reach Harbor, are verified against what
  Harbor reports, and are re-applied if someone changes them out of band.
- Scheduled rotations are invisible to image pulls. The price: in the last
  `tokenTTL` before a rotation, nodes cache for progressively shorter times and
  call the bridge more often (bounded by one call per pull).
- A HarborAccess deletion, an identity change, or an upgrade from 0.2.x leaves no
  robot with a valid password behind. Deleting a HarborAccess now costs one full
  robot listing; so does the first pass after a spec change.
- The e2e harness verifies all of it against a real Harbor: a grant change
  without breaking cached credentials, a `serviceAccountRef` change with the old
  identity refused and its robot gone, and deletion of every HarborAccess with no
  robot of the cluster left.
- Upgrading needs no migration: missing annotations are backfilled without a
  rotation; the CRD's new name rule only affects names that could never work.
