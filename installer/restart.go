// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// kubeletControl is how the installer drives the kubelet systemd unit.
// A config field (not a package variable) so tests inject a fake and can
// run in parallel.
type kubeletControl interface {
	// restart reloads systemd and restarts unit.
	restart(unit string) error
	// state returns the unit's ActiveState ("active", "activating",
	// "failed", …).
	state(unit string) (string, error)
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

func (c nsenterControl) state(unit string) (string, error) {
	if err := validUnitName(unit); err != nil {
		return "", err
	}
	// `systemctl is-active` exits non-zero for every state but "active",
	// so the exit code alone is not an error here — the printed state is
	// the answer.
	out, err := c.systemctl("is-active", unit)
	st := strings.TrimSpace(string(out))
	if st == "" && err != nil {
		return "", fmt.Errorf("systemctl is-active %s: %w", unit, err)
	}
	return st, nil
}

// verifyTiming bounds the post-restart verification. The kubelet unit on
// common distributions restarts a crashed kubelet after ~10s (RestartSec),
// so "active" must hold across a settle period longer than that before the
// install counts as good.
type verifyTiming struct {
	timeout, interval, settle time.Duration
}

var defaultVerifyTiming = verifyTiming{timeout: 90 * time.Second, interval: 2 * time.Second, settle: 15 * time.Second}

// waitKubeletHealthy fails unless unit reaches "active" and is still
// active after the settle period, and — when check is non-nil — the
// running kubelet satisfies check. It turns "systemctl restart returned
// 0" (which it does even when kubelet crash-loops on a config it rejects,
// or never picks up the flags) into an actual verification (audit M6).
func waitKubeletHealthy(ctl kubeletControl, unit string, vt verifyTiming, check func() error) error {
	deadline := time.Now().Add(vt.timeout)
	var last string
	var lastCheck error
	for time.Now().Before(deadline) {
		st, err := ctl.state(unit)
		if err != nil {
			return err
		}
		last = st
		if st == "active" {
			lastCheck = nil
			if check != nil {
				lastCheck = check()
			}
			if lastCheck == nil {
				time.Sleep(vt.settle)
				if st2, err := ctl.state(unit); err == nil && st2 == "active" {
					return nil
				} else if err == nil {
					last = st2
				}
			}
		}
		time.Sleep(vt.interval)
	}
	if lastCheck != nil {
		return fmt.Errorf("kubelet unit %q is active but not wired as intended after %s: %w", unit, vt.timeout, lastCheck)
	}
	return fmt.Errorf("kubelet unit %q did not become stably active within %s after the restart (last state %q) — check `journalctl -u %s` on the node", unit, vt.timeout, last, unit)
}
