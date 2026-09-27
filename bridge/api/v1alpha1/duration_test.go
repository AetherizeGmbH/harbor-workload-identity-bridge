// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

func TestDuration_DecodesGoDurations(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		`"1h"`:       time.Hour,
		`"90m"`:      90 * time.Minute,
		`"1h0m0s"`:   time.Hour,
		`"1.5h"`:     90 * time.Minute,
		`"300000ms"`: 5 * time.Minute,
	} {
		var d Duration
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if d.Duration != want || d.Err() != nil {
			t.Errorf("%s: got %s, Err %v; want %s, nil", raw, d.Duration, d.Err(), want)
		}
		out, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if canon := `"` + want.String() + `"`; string(out) != canon {
			t.Errorf("%s: marshalled to %s, want %s", raw, out, canon)
		}
	}
}

func TestDuration_NullIsAbsentNotInvalid(t *testing.T) {
	d := Duration{Duration: time.Hour}
	if err := json.Unmarshal([]byte(`null`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Duration != 0 || d.Err() != nil {
		t.Errorf("null: got %s, Err %v; want 0, nil", d.Duration, d.Err())
	}
}

// Values the apiserver admitted under `format: duration` (strfmt), and any
// other JSON, must decode without an error: one such value would otherwise
// fail the decode of every HarborAccess list. They must also survive a
// round trip unchanged, so no write of the object rewrites the user's value.
func TestDuration_ToleratesValuesGoCannotParse(t *testing.T) {
	for _, raw := range []string{
		`"1d"`, `"3 hours"`, `"PT1H"`, `"1x5m"`, `"1h "`, `""`,
		`"18446744374s"`, // overflows time.Duration; strfmt wrapped it to ~5m
		`3600`, `true`, `{}`, `[]`,
	} {
		d := Duration{Duration: time.Hour}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			t.Errorf("%s: decode failed: %v", raw, err)
			continue
		}
		if d.Duration != 0 {
			t.Errorf("%s: Duration = %s, want 0", raw, d.Duration)
		}
		if err := d.Err(); err == nil {
			t.Errorf("%s: Err() = nil, want an error", raw)
		}
		out, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != raw {
			t.Errorf("%s: marshalled to %s, want it unchanged", raw, out)
		}
	}
}

func TestDuration_ErrNamesAndBoundsTheValue(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"1d"`), &d); err != nil {
		t.Fatal(err)
	}
	if msg := d.Err().Error(); !strings.Contains(msg, `"1d"`) || !strings.Contains(msg, "days are not supported") {
		t.Errorf("Err() = %q, want it to name the value and the unit rule", msg)
	}

	long := `"` + strings.Repeat("9", 100_000) + `d"`
	if err := json.Unmarshal([]byte(long), &d); err != nil {
		t.Fatal(err)
	}
	// The message is written into a status condition (message maxLength
	// 32768); a legacy value may be much longer.
	if n := len(d.Err().Error()); n > 512 {
		t.Errorf("Err() for a %d-byte value is %d bytes long, want it bounded", len(long), n)
	}
}

// The controller-runtime cache hands out deep copies; the invalid marker
// must survive them or the reconciler would never see it.
func TestDuration_DeepCopyKeepsInvalidValue(t *testing.T) {
	ha := &HarborAccess{}
	if err := json.Unmarshal([]byte(`{"spec":{"tokenTTL":"1d"}}`), ha); err != nil {
		t.Fatal(err)
	}
	cp, ok := ha.DeepCopyObject().(*HarborAccess)
	if !ok {
		t.Fatal("DeepCopyObject returned another type")
	}
	if cp.Spec.TokenTTL.Err() == nil {
		t.Error("deep copy lost the invalid tokenTTL")
	}
}

// The regression the type exists for: the JSON decoder client-go uses
// aborts a whole list on the first item it cannot decode. With one
// HarborAccess whose tokenTTL time.ParseDuration rejects, the list must
// still decode, with every other item intact.
func TestHarborAccessList_DecodesDespiteOneUnparseableTokenTTL(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	list := `{
	  "apiVersion": "harbor.aetherize.io/v1alpha1", "kind": "HarborAccessList", "metadata": {},
	  "items": [
	    {"apiVersion": "harbor.aetherize.io/v1alpha1", "kind": "HarborAccess",
	     "metadata": {"name": "typo", "namespace": "tenant-b"}, "spec": {"tokenTTL": "1d"}},
	    {"apiVersion": "harbor.aetherize.io/v1alpha1", "kind": "HarborAccess",
	     "metadata": {"name": "ok", "namespace": "tenant-a"}, "spec": {"tokenTTL": "15m"}}
	  ]}`
	obj, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode([]byte(list), nil, nil)
	if err != nil {
		t.Fatalf("decode HarborAccessList: %v", err)
	}
	got, ok := obj.(*HarborAccessList)
	if !ok || len(got.Items) != 2 {
		t.Fatalf("decoded %T %+v, want a HarborAccessList with 2 items", obj, obj)
	}
	if got.Items[0].Spec.TokenTTL.Err() == nil {
		t.Error(`item "typo": Err() = nil, want the unparseable value reported`)
	}
	if ttl := got.Items[1].Spec.TokenTTL; ttl.Err() != nil || ttl.Duration != 15*time.Minute {
		t.Errorf(`item "ok": tokenTTL = %s (Err %v), want 15m`, ttl.Duration, ttl.Err())
	}
}

// runtime.DefaultUnstructuredConverter (used for patches and unstructured
// clients) goes through MarshalJSON/UnmarshalJSON and must keep the raw
// value as well.
func TestDuration_UnstructuredRoundTripKeepsInvalidValue(t *testing.T) {
	ha := &HarborAccess{}
	if err := json.Unmarshal([]byte(`{"spec":{"tokenTTL":"3 hours"}}`), ha); err != nil {
		t.Fatal(err)
	}
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ha)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := u["spec"].(map[string]any)
	if spec["tokenTTL"] != "3 hours" {
		t.Errorf("unstructured tokenTTL = %#v, want the raw value", spec["tokenTTL"])
	}
	back := &HarborAccess{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u, back); err != nil {
		t.Fatalf("FromUnstructured: %v", err)
	}
	if back.Spec.TokenTTL.Err() == nil {
		t.Error("FromUnstructured lost the invalid tokenTTL")
	}
}
