// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

// NexusFinalizerName is the finalizer the Nexus reconciler sets on every
// NexusAccess it provisions, so that it can delete the Nexus users and the
// role before the object goes. With a selector the bridge sets
// NexusFinalizerName + "-<instance>" instead (Config.NexusFinalizer,
// ADR-0026, ADR-0032).
const NexusFinalizerName = "nexus.aetherize.io/user"
