// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is the Go type of spec.tokenTTL: a time.Duration written as a
// Go duration string ("90m", "1h", "1h30m"), like metav1.Duration.
//
// Unlike metav1.Duration, decoding never fails. The bridge reads every
// HarborAccess through one LIST, and the JSON decoder aborts that list on
// the first item it cannot decode: one object with a tokenTTL that
// time.ParseDuration rejects would stop the controller's cache from ever
// syncing (the leader exits) and stall every credential request, for all
// workloads, not only for that object's. Up to 0.10.0 the CRD declared
// tokenTTL as `format: duration`, whose grammar also admits values such as
// "1d", "3 hours" or "PT1H".
//
// The CRD therefore no longer sets a format: its CEL rule's duration()
// parses with time.ParseDuration, so the apiserver admits only what this
// type decodes. The rule exempts an unchanged value: the apiserver does not
// ratchet CEL evaluation errors, and without the exemption an object that
// still holds such a legacy value could not be written at all, not its
// status and not its finalizer.
//
// A value that is not a Go duration decodes to zero; Err reports it, the
// reconciler surfaces it as InvalidSpec, the data plane refuses to serve
// the object, and MarshalJSON writes the original JSON back unchanged.
//
// +kubebuilder:validation:Type=string
type Duration struct {
	time.Duration `json:"-"`

	// invalid is the raw JSON of a value time.ParseDuration rejects; empty
	// for a valid or absent value.
	invalid string `json:"-"`
}

// UnmarshalJSON implements json.Unmarshaler. It never returns an error;
// see the type comment.
func (d *Duration) UnmarshalJSON(b []byte) error {
	*d = Duration{}
	if string(b) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if pd, err := time.ParseDuration(s); err == nil {
			d.Duration = pd
			return nil
		}
	}
	d.invalid = string(b)
	return nil
}

// MarshalJSON implements json.Marshaler. A valid value is written in
// time.Duration's canonical form, an invalid one exactly as it was read.
func (d Duration) MarshalJSON() ([]byte, error) {
	if d.invalid != "" {
		return []byte(d.invalid), nil
	}
	return json.Marshal(d.String())
}

// Err returns nil when the value was absent or a valid Go duration, and
// otherwise why it is not one.
func (d Duration) Err() error {
	if d.invalid == "" {
		return nil
	}
	var s string
	if err := json.Unmarshal([]byte(d.invalid), &s); err != nil {
		return fmt.Errorf("%s is not a string", clip(d.invalid))
	}
	return fmt.Errorf("%q is not a Go duration such as 30m, 1h or 1h30m (units h, m, s, ms, us, ns; days are not supported)", clip(s))
}

// maxShownInvalid bounds how much of an invalid value Err repeats. Values
// stored before the CRD set a maxLength can be arbitrarily long, and the
// message ends up in a status condition, whose message is limited.
const maxShownInvalid = 64

func clip(s string) string {
	if len(s) <= maxShownInvalid {
		return s
	}
	return s[:maxShownInvalid] + "..."
}
