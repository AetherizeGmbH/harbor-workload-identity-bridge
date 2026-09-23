# 24. The chart can leave the plugin to the platform; supported node platforms

## Status

Accepted. Extends ADR-0021 (node installer modes).

## Context

Issue #46 (Talos Linux): on Talos the kubelet credential-provider plugin must be
delivered as a system extension, and kubelet is configured through the machine
configuration. The chart always rendered the plugin DaemonSet, its ConfigMap and
its ServiceAccount; the only lever was `install.mode=none`, which still runs a
DaemonSet that copies files to paths Talos never uses.

The same need exists wherever nodes are provisioned out of band: baked node
images, Bottlerocket settings, fleet configuration management, and managed
offerings that forbid privileged DaemonSets (GKE Autopilot).

Separately, the installer (ADR-0021) targets systemd-managed kubelets only, and
its documentation claimed k3s support it does not have: k3s embeds kubelet in the
`k3s` binary (no `kubelet` process to discover, no `kubelet` unit to restart, and
`/etc/default/kubelet` is never read). RKE2 and Talos supervise kubelet
themselves in comparable ways.

## Decision

1. `plugin.enabled` (default `true`). When `false` the chart renders **no**
   DaemonSet, plugin ConfigMap, plugin ServiceAccount or plugin mTLS client
   Certificate. It still renders the bridge, the Service, and the kubelet
   audience RBAC (ADR-0017) — kubelet needs that RBAC no matter who installed the
   plugin. `plugin.matchImages` and `plugin.install.*` are not required then;
   `plugin.audience` still is. NOTES print the exact provider entry (endpoint,
   CA path, audience, cache type) to install, and
   `docs/install-external-plugin.md` describes Talos, baked images and GKE.
2. The plugin binary for out-of-band installation is taken from the published
   plugin image (`/plugin/harbor-bridge-plugin`, multi-arch, signed, SBOM
   attested). No separate artifact is published; the image is the supply-chain
   anchor (ADR-0019).
3. Platform support matrix (documented in `docs/platforms.md`):

   | Platform | Install path | Verified |
   |---|---|---|
   | kind / kubeadm (systemd kubelet sourcing `/etc/default/kubelet`) | `auto` → patch | yes, e2e in CI |
   | GKE Standard (COS / Ubuntu) | `auto` → merge into `cri_auth_config.yaml` | harness ready (ADR-0022), not run |
   | EKS (AL2023) | `auto` → merge into the ECR provider config (JSON) | unit-tested merge, not run |
   | AKS (Ubuntu / Azure Linux) | `auto` → merge into the ACR provider config | unit-tested merge, not run |
   | Talos | `plugin.enabled=false` + system extension + machine config | documented, not run |
   | k3s / RKE2 | `install.mode=none` or `plugin.enabled=false`, flags via the distribution's kubelet args | documented, not run |
   | GKE Autopilot, Bottlerocket | not supported (no privileged DaemonSets / no custom provider binaries) | — |

4. The installer's error for "no kubelet process" names these distributions and
   points to `none` / `plugin.enabled=false`, instead of failing opaquely.

## Consequences

- Talos and other out-of-band platforms install the chart without any node
  agent, and the chart no longer creates objects they cannot use.
- The support claims match what the code can do. Unverified platforms are
  marked as such; GKE is the next one to verify (ADR-0022), EKS/AKS harnesses
  would reuse the same shared e2e modules (`test/e2e/modules/*`: token-auth
  kubeconfig, LoadBalancer Harbor, diagnostics) with a cloud-specific cluster
  module.
