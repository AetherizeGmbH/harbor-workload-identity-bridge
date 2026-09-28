// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// kubeletControl is how the installer drives the kubelet systemd unit.
// A config field (not a package variable) so tests inject a fake and can
// run in parallel.
type kubeletControl interface {
	// restart reloads systemd and restarts unit.
	restart(unit string) error
	// status returns what systemd reports about unit (unitStatus).
	status(unit string) (unitStatus, error)
}

// unitStatus is what systemd reports about the kubelet unit.
type unitStatus struct {
	// ActiveState is the unit's ActiveState ("active", "activating",
	// "failed", …).
	ActiveState string
	// MainPID is the unit's main process; 0 while none runs, for example
	// between a crash and the automatic restart (RestartSec).
	MainPID int
	// NRestarts counts the automatic restarts (Restart=) of the unit; -1
	// when systemd does not report it (before systemd 235).
	NRestarts int
}

func (s unitStatus) String() string {
	return fmt.Sprintf("ActiveState=%s MainPID=%d NRestarts=%d", s.ActiveState, s.MainPID, s.NRestarts)
}

// unitNameRegex is the systemd unit-name character set. The unit name is a
// chart value that ends up as an argument to systemctl in the HOST's
// namespaces; restricting it keeps a value like "kubelet; rm -rf /" or an
// option-looking "--help" from ever reaching systemctl.
var unitNameRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]*$`)

func validUnitName(unit string) error {
	if !unitNameRegex.MatchString(unit) {
		return fmt.Errorf("invalid systemd unit name %q", unit)
	}
	return nil
}

// nsenterControl runs systemctl inside the host's namespaces via nsenter
// into PID 1. Requires hostPID + privileged (the DaemonSet sets both for
// every mode except none). Restarting kubelet does not kill running
// containers — containerd owns them (ADR-0021). No shell is involved: the
// unit name is passed as a single argv element.
type nsenterControl struct{}

func (nsenterControl) systemctl(args ...string) ([]byte, error) {
	argv := append([]string{"-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "systemctl"}, args...)
	return exec.Command("nsenter", argv...).CombinedOutput()
}

func (c nsenterControl) restart(unit string) error {
	if err := validUnitName(unit); err != nil {
		return err
	}
	// daemon-reload: not strictly required for an EnvironmentFile change,
	// but cheap, and it covers operators who template unit drop-ins.
	if out, err := c.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w (output: %s)", err, bytes.TrimSpace(out))
	}
	if out, err := c.systemctl("restart", unit); err != nil {
		return fmt.Errorf("systemctl restart %s: %w (output: %s)", unit, err, bytes.TrimSpace(out))
	}
	return nil
}

func (c nsenterControl) status(unit string) (unitStatus, error) {
	if err := validUnitName(unit); err != nil {
		return unitStatus{}, err
	}
	out, err := c.systemctl("show", "--property=ActiveState", "--property=MainPID", "--property=NRestarts", unit)
	if err != nil {
		return unitStatus{}, fmt.Errorf("systemctl show %s: %w (output: %s)", unit, err, bytes.TrimSpace(out))
	}
	return parseUnitStatus(out)
}

// parseUnitStatus parses the Key=Value lines of `systemctl show`. It needs
// ActiveState and MainPID; NRestarts is optional (systemd 235+).
func parseUnitStatus(out []byte) (unitStatus, error) {
	st := unitStatus{MainPID: -1, NRestarts: -1}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			st.ActiveState = value
		case "MainPID":
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return unitStatus{}, fmt.Errorf("systemctl show: MainPID=%q is not a PID", value)
			}
			st.MainPID = n
		case "NRestarts":
			if n, err := strconv.Atoi(value); err == nil && n >= 0 {
				st.NRestarts = n
			}
		}
	}
	if st.ActiveState == "" || st.MainPID < 0 {
		return unitStatus{}, fmt.Errorf("systemctl show did not report ActiveState and MainPID (output: %s)", bytes.TrimSpace(out))
	}
	return st, nil
}

// verifyTiming bounds the post-restart verification. settle is how long
// one kubelet process must stay up before the install counts as good:
// kubelet exits at startup on a credential-provider config or flags it
// rejects, and its unit (Restart=always) starts it again after RestartSec
// (10s in kubeadm's unit, less in others). Two samples of "active" alone do not
// tell a stable kubelet from one that crashed and was restarted in
// between, so both samples must also name the same main process
// (waitKubeletHealthy). A kubelet that exits later than settle after its
// start is not caught; no finite window can catch every crash.
type verifyTiming struct {
	timeout, interval, settle time.Duration
}

var defaultVerifyTiming = verifyTiming{timeout: 90 * time.Second, interval: 2 * time.Second, settle: 15 * time.Second}

// waitKubeletHealthy fails unless unit is active with a main process that
// is still the same process, still active, after the settle period
// (sameProcess), and — when check is non-nil — the running kubelet
// satisfies check. It turns "systemctl restart returned 0" (which it does
// even when kubelet crash-loops on a config it rejects, or never picks up
// the flags) into an actual verification (audit M6). Every attempt takes a
// fresh first sample, so a process that was replaced during one attempt
// has to stay up for a whole settle period of its own.
func waitKubeletHealthy(ctl kubeletControl, unit string, vt verifyTiming, check func() error) error {
	deadline := time.Now().Add(vt.timeout)
	var last unitStatus
	var lastCheck error
	for time.Now().Before(deadline) {
		first, err := ctl.status(unit)
		if err != nil {
			return err
		}
		last = first
		if first.ActiveState == "active" && first.MainPID > 0 {
			lastCheck = nil
			if check != nil {
				lastCheck = check()
			}
			if lastCheck == nil {
				time.Sleep(vt.settle)
				second, err := ctl.status(unit)
				if err != nil {
					return err
				}
				last = second
				if sameProcess(first, second) {
					return nil
				}
			}
		}
		time.Sleep(vt.interval)
	}
	if lastCheck != nil {
		return fmt.Errorf("kubelet unit %q is active but not wired as intended after %s: %w", unit, vt.timeout, lastCheck)
	}
	return fmt.Errorf("kubelet unit %q did not become stably active within %s after the restart: no kubelet process stayed up for %s (last %s) — check `journalctl -u %s` on the node", unit, vt.timeout, vt.settle, last, unit)
}

// sameProcess reports whether second, taken a settle period after first,
// shows the unit still active with the same main process: no crash and no
// automatic restart in between. NRestarts is compared when systemd reports
// it, which also covers a reused PID.
func sameProcess(first, second unitStatus) bool {
	return second.ActiveState == "active" &&
		second.MainPID > 0 && second.MainPID == first.MainPID &&
		(first.NRestarts < 0 || second.NRestarts < 0 || first.NRestarts == second.NRestarts)
}
