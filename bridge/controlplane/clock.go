// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import "time"

// Clock is the interface the reconciler and janitor use to read the current
// time. Tests inject a fixed-time fake so password-rotation and orphan-sweep
// decisions are deterministic.
type Clock interface {
	Now() time.Time
}

// RealClock returns time.Now(). The default for production code.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }
