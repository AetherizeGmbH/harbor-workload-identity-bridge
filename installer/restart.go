// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os/exec"
)

// restartKubelet restarts the kubelet systemd unit from inside the
// host's namespaces. Requires hostPID + privileged (the DaemonSet sets
// both for every mode except none). Restarting kubelet does not kill
// running containers — containerd owns them (ADR-0021).
//
// daemon-reload is included for parity with the pre-ADR-0021 script:
// it is not strictly required for an EnvironmentFile change, but it is
// cheap and covers operators who template unit drop-ins around us.
var restartKubelet = func(unit string) error {
	script := fmt.Sprintf("systemctl daemon-reload && systemctl restart %s", unit)
	cmd := exec.Command("nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--", "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restart %s: %w (output: %s)", unit, err, out)
	}
	return nil
}
