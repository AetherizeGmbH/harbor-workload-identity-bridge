// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

const gomodWithToolchain = `module example.com/m

go 1.26.0

// Security floor, see the comment in the real go.mod.
toolchain go1.27.1

require (
	example.com/dep v1.0.0 // indirect
)
`

func TestCheck(t *testing.T) {
	cases := []struct {
		name    string
		running string
		gomod   string
		wantErr string // substring; empty means the check passes
	}{
		{name: "builder equals toolchain", running: "go1.27.1", gomod: gomodWithToolchain},
		{name: "builder newer patch", running: "go1.27.2", gomod: gomodWithToolchain},
		{name: "builder newer minor", running: "go1.28.0", gomod: gomodWithToolchain},
		{name: "GOEXPERIMENT suffix", running: "go1.27.1 X:boringcrypto", gomod: gomodWithToolchain},
		{
			// The case the check exists for: GOTOOLCHAIN=local would build
			// with the older Go and ignore the toolchain line.
			name: "builder older than toolchain", running: "go1.27.0", gomod: gomodWithToolchain,
			wantErr: "building with go1.27.0, but go.mod requires at least go1.27.1",
		},
		{
			name: "builder satisfies go line but not toolchain", running: "go1.26.9", gomod: gomodWithToolchain,
			wantErr: "requires at least go1.27.1",
		},
		{
			name: "release candidate is older than the release", running: "go1.27rc2", gomod: gomodWithToolchain,
			wantErr: "requires at least go1.27.1",
		},
		{
			name: "no toolchain line falls back to go line", running: "go1.26.0",
			gomod: "module example.com/m\n\ngo 1.26.0\n",
		},
		{
			name: "no toolchain line, builder older than go line", running: "go1.25.9",
			gomod:   "module example.com/m\n\ngo 1.26.0\n",
			wantErr: "requires at least go1.26.0",
		},
		{
			name: "toolchain default falls back to go line", running: "go1.25.9",
			gomod:   "module example.com/m\n\ngo 1.26.0\ntoolchain default\n",
			wantErr: "requires at least go1.26.0",
		},
		{
			name: "trailing comment on the toolchain line", running: "go1.27.0",
			gomod:   "module example.com/m\n\ngo 1.26.0\ntoolchain go1.27.1 // pinned\n",
			wantErr: "requires at least go1.27.1",
		},
		{
			name: "devel toolchain is refused", running: "devel go1.28-abcdef Tue Sep 1 00:00:00 2026 +0000",
			gomod: gomodWithToolchain, wantErr: "not a Go release",
		},
		{
			name: "no directives", running: "go1.27.1", gomod: "module example.com/m\n",
			wantErr: "neither a toolchain nor a go directive",
		},
		{
			name: "garbage toolchain", running: "go1.27.1", gomod: "module example.com/m\n\ngo 1.26.0\ntoolchain 1.27.1\n",
			wantErr: "not a valid Go version",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := check(tc.running, []byte(tc.gomod))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("check(%q) = %v, want success", tc.running, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("check(%q) succeeded, want an error containing %q", tc.running, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("check(%q) = %v, want an error containing %q", tc.running, err, tc.wantErr)
			}
		})
	}
}

// The repository's own go.mod must parse, and its minimum must be the
// toolchain line (the security floor), not the lower go line.
func TestMinimum_RepositoryGoMod(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	got, err := minimum(gomod)
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, line := range strings.Split(string(gomod), "\n") {
		if v, ok := strings.CutPrefix(line, "toolchain "); ok {
			want = strings.TrimSpace(v)
		}
	}
	if want == "" {
		t.Skip("go.mod has no toolchain line; the go-line fallback is covered by TestCheck")
	}
	if got != want {
		t.Fatalf("minimum(go.mod) = %q, want the toolchain line %q", got, want)
	}
}

var golangFrom = regexp.MustCompile(`(?m)^FROM\s+(?:--platform=\S+\s+)?golang:([0-9]+\.[0-9]+\.[0-9]+)-[a-z0-9.]+@sha256:[0-9a-f]{64}\s`)

// Both release Dockerfiles must run the check before they compile, and name
// the builder's exact Go release in the image tag, at or above go.mod's
// minimum. The Docker build enforces the floor on the digest; this test
// reports a lagging tag on every PR, not only on PRs that trigger e2e.
func TestDockerfiles_EnforceToolchainFloor(t *testing.T) {
	gomod, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	want, err := minimum(gomod)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Dockerfile.bridge", "Dockerfile.plugin"} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("../../" + name)
			if err != nil {
				t.Fatal(err)
			}
			df := string(raw)
			m := golangFrom.FindStringSubmatch(df)
			if m == nil {
				t.Fatalf("no digest-pinned golang:<x.y.z>-<variant> builder stage; the tag must name the exact Go release")
			}
			if _, err := check("go"+m[1], gomod); err != nil {
				t.Fatalf("builder image golang:%s is older than go.mod's minimum %s: %v", m[1], want, err)
			}
			run := strings.Index(df, "go run ./hack/toolchaincheck")
			build := strings.Index(df, "go build")
			if run < 0 || build < 0 || run > build {
				t.Fatalf("%s must run `go run ./hack/toolchaincheck` before its first `go build`", name)
			}
		})
	}
}
