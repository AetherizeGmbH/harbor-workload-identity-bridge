// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

// Command toolchaincheck fails when the Go toolchain that runs it is older
// than the minimum that ./go.mod declares: the `toolchain` directive, or the
// `go` directive when there is no toolchain line.
//
// Dockerfile.bridge and Dockerfile.plugin run it before they build. The
// golang images set GOTOOLCHAIN=local, and under that setting the go command
// ignores the `toolchain` directive and builds with whatever Go the image
// carries. go.mod's toolchain line is a security floor (stdlib CVE fixes on
// the OIDC/x509 paths). CI and govulncheck honour it, so without this check a
// builder image older than that line would ship the vulnerable stdlib while
// every check stays green.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/version"
	"os"
	"runtime"
	"strings"
)

func main() {
	gomod, err := os.ReadFile("go.mod")
	if err == nil {
		var msg string
		msg, err = check(runtime.Version(), gomod)
		if err == nil {
			fmt.Println(msg)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "toolchaincheck: %v\n", err)
	os.Exit(1)
}

// check reports whether the running toolchain (as runtime.Version prints it)
// satisfies the minimum declared in gomod.
func check(running string, gomod []byte) (string, error) {
	want, err := minimum(gomod)
	if err != nil {
		return "", err
	}
	// runtime.Version appends " X:<experiments>" for GOEXPERIMENT builds.
	fields := strings.Fields(running)
	if len(fields) == 0 || !version.IsValid(fields[0]) {
		return "", fmt.Errorf("running toolchain %q is not a Go release; cannot compare it with go.mod's minimum %s", running, want)
	}
	have := fields[0]
	if version.Compare(have, want) < 0 {
		return "", fmt.Errorf("building with %s, but go.mod requires at least %s: "+
			"move the golang image in Dockerfile.bridge and Dockerfile.plugin to %s or newer", have, want, want)
	}
	return fmt.Sprintf("toolchain %s satisfies go.mod's minimum %s", have, want), nil
}

// minimum returns the oldest toolchain gomod accepts, in runtime.Version form
// ("go1.27.1"). The `toolchain` line wins over the `go` line; `go mod tidy`
// drops the toolchain line when it would not raise the minimum, so the `go`
// line is the fallback, never an empty string.
func minimum(gomod []byte) (string, error) {
	var goLine, toolchain string
	sc := bufio.NewScanner(bytes.NewReader(gomod))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "//")
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		switch f[0] {
		case "go":
			goLine = "go" + f[1]
		case "toolchain":
			toolchain = f[1]
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("reading go.mod: %w", err)
	}
	for _, v := range []string{toolchain, goLine} {
		if v == "" || v == "default" {
			continue
		}
		if !version.IsValid(v) {
			return "", fmt.Errorf("go.mod declares %q, which is not a valid Go version", v)
		}
		return v, nil
	}
	return "", errors.New("go.mod has neither a toolchain nor a go directive")
}
