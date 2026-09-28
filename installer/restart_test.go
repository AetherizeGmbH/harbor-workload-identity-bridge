// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseUnitStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		want unitStatus
	}{
		"running":         {"ActiveState=active\nMainPID=4242\nNRestarts=0\n", unitStatus{"active", 4242, 0}},
		"any order":       {"NRestarts=3\nMainPID=4242\nActiveState=active\n", unitStatus{"active", 4242, 3}},
		"auto-restart":    {"MainPID=0\nNRestarts=2\nActiveState=activating\n", unitStatus{"activating", 0, 2}},
		"old systemd":     {"MainPID=17\nActiveState=active\n", unitStatus{"active", 17, -1}},
		"empty NRestarts": {"MainPID=17\nActiveState=active\nNRestarts=\n", unitStatus{"active", 17, -1}},
	} {
		got, err := parseUnitStatus([]byte(tc.out))
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
	for name, out := range map[string]string{
		"no MainPID":      "ActiveState=active\n",
		"no ActiveState":  "MainPID=17\n",
		"garbage MainPID": "ActiveState=active\nMainPID=x\n",
		"empty":           "",
	} {
		if got, err := parseUnitStatus([]byte(out)); err == nil {
			t.Errorf("%s: accepted as %+v", name, got)
		}
	}
}

// crashLoopUnit is a kubelet unit with Restart=always whose kubelet exits
// up after every start; systemd starts the next one down later
// (RestartSec). Between two kubelets the unit is "activating" (SubState
// auto-restart) with no main process, and a unit with StartLimitInterval=0
// never reaches "failed". An up of 0 is a kubelet that never exits.
type crashLoopUnit struct {
	start    time.Time
	up, down time.Duration
}

func (u *crashLoopUnit) restart(string) error { return nil }

func (u *crashLoopUnit) status(string) (unitStatus, error) {
	elapsed := time.Since(u.start)
	if u.up == 0 {
		return unitStatus{ActiveState: "active", MainPID: 1000, NRestarts: 0}, nil
	}
	cycle := u.up + u.down
	n := int(elapsed / cycle)
	if elapsed%cycle < u.up {
		return unitStatus{ActiveState: "active", MainPID: 1000 + n, NRestarts: n}, nil
	}
	return unitStatus{ActiveState: "activating", MainPID: 0, NRestarts: n}, nil
}

// withoutNRestarts is a unit on systemd before 235, which reports no
// NRestarts: only the main PID tells two kubelets apart.
type withoutNRestarts struct{ kubeletControl }

func (u withoutNRestarts) status(unit string) (unitStatus, error) {
	st, err := u.kubeletControl.status(unit)
	st.NRestarts = -1
	return st, err
}

// TestWaitKubeletHealthy_CatchesAutoRestartBetweenSamples: a kubelet that
// exits a few seconds after every start (a config or flag it rejects) and
// is started again by systemd is "active" at both samples whenever the
// second one lands in the next kubelet's lifetime. Before the check
// compared the main process, such a crash loop passed verification for
// uptimes of roughly 2.5-7s with kubeadm's RestartSec=10, and for most
// uptimes with a short RestartSec. The timings are defaultVerifyTiming's,
// one second scaled to 10ms.
func TestWaitKubeletHealthy_CatchesAutoRestartBetweenSamples(t *testing.T) {
	const second = 10 * time.Millisecond
	vt := verifyTiming{timeout: 90 * second, interval: 2 * second, settle: 15 * second}
	type loop struct{ up, down float64 }
	var cases []loop
	for _, up := range []float64{3, 4, 4.5, 5.5, 6} {
		cases = append(cases, loop{up, 10}) // kubeadm: RestartSec=10
	}
	for _, up := range []float64{2, 3, 4, 5} {
		cases = append(cases, loop{up, 1}) // a unit with RestartSec=1
	}
	var wg sync.WaitGroup
	errs := make([]error, len(cases))
	legacy := make([]error, len(cases))
	for i, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unit := &crashLoopUnit{start: time.Now(), up: time.Duration(c.up * float64(second)), down: time.Duration(c.down * float64(second))}
			errs[i] = waitKubeletHealthy(unit, "kubelet", vt, nil)
			old := &crashLoopUnit{start: time.Now(), up: unit.up, down: unit.down}
			legacy[i] = waitKubeletHealthy(withoutNRestarts{old}, "kubelet", vt, nil)
		}()
	}
	wg.Wait()
	for i, c := range cases {
		for what, err := range map[string]error{"": errs[i], " (no NRestarts)": legacy[i]} {
			if err == nil || !strings.Contains(err.Error(), "did not become stably active") {
				t.Errorf("kubelet up %gs, restarted after %gs%s: got %v, want a verification failure", c.up, c.down, what, err)
			}
		}
	}
}

func TestWaitKubeletHealthy_StableKubeletPasses(t *testing.T) {
	vt := verifyTiming{timeout: time.Second, interval: time.Millisecond, settle: 5 * time.Millisecond}
	if err := waitKubeletHealthy(&crashLoopUnit{start: time.Now()}, "kubelet", vt, nil); err != nil {
		t.Fatalf("stable kubelet failed verification: %v", err)
	}
	// Up longer than the settle period after the restart: verified. (A
	// crash after the settle period is not caught; verifyTiming.)
	unit := &crashLoopUnit{start: time.Now(), up: time.Hour, down: time.Millisecond}
	if err := waitKubeletHealthy(unit, "kubelet", vt, nil); err != nil {
		t.Fatalf("kubelet that stays up failed verification: %v", err)
	}
}

// TestWaitKubeletHealthy_ActiveWithoutMainProcessIsNotVerified: "active"
// without a main process cannot be told apart from a restarted kubelet.
func TestWaitKubeletHealthy_ActiveWithoutMainProcessIsNotVerified(t *testing.T) {
	vt := verifyTiming{timeout: 20 * time.Millisecond, interval: time.Millisecond, settle: time.Millisecond}
	unit := &scriptedUnit{st: unitStatus{ActiveState: "active", MainPID: 0, NRestarts: -1}}
	err := waitKubeletHealthy(unit, "kubelet", vt, nil)
	if err == nil || !strings.Contains(err.Error(), "MainPID=0") {
		t.Fatalf("got %v, want a failure that names the missing main process", err)
	}
}

type scriptedUnit struct{ st unitStatus }

func (u *scriptedUnit) restart(string) error              { return nil }
func (u *scriptedUnit) status(string) (unitStatus, error) { return u.st, nil }

func TestSameProcess(t *testing.T) {
	a := unitStatus{ActiveState: "active", MainPID: 10, NRestarts: 0}
	for name, tc := range map[string]struct {
		second unitStatus
		want   bool
	}{
		"same":             {a, true},
		"new PID":          {unitStatus{"active", 11, 1}, false},
		"reused PID":       {unitStatus{"active", 10, 1}, false},
		"auto-restart":     {unitStatus{"activating", 0, 1}, false},
		"failed":           {unitStatus{"failed", 0, 0}, false},
		"no NRestarts":     {unitStatus{"active", 10, -1}, true},
		"no NRestarts new": {unitStatus{"active", 11, -1}, false},
	} {
		if got := sameProcess(a, tc.second); got != tc.want {
			t.Errorf("%s: sameProcess(%v, %v) = %v, want %v", name, a, tc.second, got, tc.want)
		}
	}
}
