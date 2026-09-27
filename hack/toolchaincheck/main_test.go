// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dockerfiles are the release Dockerfiles that run the check.
var dockerfiles = []string{"Dockerfile.bridge", "Dockerfile.plugin"}

// checkCommand is how both Dockerfiles invoke the check.
const checkCommand = "go run ./hack/toolchaincheck"

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
	for _, name := range dockerfiles {
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
			run := strings.Index(df, checkCommand)
			build := strings.Index(df, "go build")
			if run < 0 || build < 0 || run > build {
				t.Fatalf("%s must run `%s` before its first `go build`", name, checkCommand)
			}
		})
	}
}

// The builder stage runs the check before it copies any source: it holds
// only what the COPY instructions above the check put there (today go.mod,
// go.sum and this directory). Rebuild exactly that view in a temp dir, from
// each Dockerfile's own COPY lines, and run the check there the way the
// Dockerfile does. A main() that reads a file the stage lacks, or an import
// of a package the stage does not copy, fails here instead of first failing
// in release-images, after the release tag exists.
func TestToolchaincheck_RunsInTheDockerStage(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("the go command is needed to run the check as the Dockerfiles do: %v", err)
	}
	for _, name := range dockerfiles {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile("../../" + name)
			if err != nil {
				t.Fatal(err)
			}
			copies, err := copiesBeforeCheck(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			stage := t.TempDir()
			for _, c := range copies {
				if err := stageCopy("../..", stage, c); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(goBin, "run", "./hack/toolchaincheck")
			cmd.Dir = stage
			cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s in the %s builder stage view (%v) failed: %v\n%s", checkCommand, name, copies, err, out)
			}
			if !strings.Contains(string(out), "satisfies go.mod's minimum") {
				t.Fatalf("%s printed %q, want the success line", checkCommand, out)
			}
		})
	}
}

// copyInstr is one COPY instruction: sources relative to the build context,
// destination relative to the builder stage's WORKDIR.
type copyInstr struct {
	srcs []string
	dest string
}

// copiesBeforeCheck returns the COPY instructions of the golang builder
// stage that precede the RUN line invoking the check.
func copiesBeforeCheck(df string) ([]copyInstr, error) {
	var copies []copyInstr
	inBuilder := false
	for _, line := range strings.Split(df, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "FROM":
			inBuilder = golangFrom.MatchString(line + "\n")
			copies = nil
		case "COPY":
			if !inBuilder {
				continue
			}
			args := f[1:]
			for len(args) > 0 && strings.HasPrefix(args[0], "--") {
				if strings.HasPrefix(args[0], "--from") {
					return nil, fmt.Errorf("COPY %s before the check: this test only models copies from the build context", args[0])
				}
				args = args[1:]
			}
			if len(args) < 2 {
				return nil, fmt.Errorf("cannot parse %q", line)
			}
			copies = append(copies, copyInstr{srcs: args[:len(args)-1], dest: args[len(args)-1]})
		case "RUN":
			if inBuilder && strings.Contains(line, checkCommand) {
				if len(copies) == 0 {
					return nil, fmt.Errorf("no COPY before %q", line)
				}
				return copies, nil
			}
		}
	}
	return nil, fmt.Errorf("no RUN line with %q in a golang builder stage", checkCommand)
}

// stageCopy applies c to stage with Docker's COPY semantics for the forms
// the Dockerfiles use: a destination ending in "/" receives each source
// under its base name, otherwise the single source becomes the destination.
// A directory source copies its contents.
func stageCopy(context, stage string, c copyInstr) error {
	dest := path.Clean(c.dest)
	if path.IsAbs(dest) {
		return fmt.Errorf("COPY %v %s: this test models destinations relative to WORKDIR only", c.srcs, c.dest)
	}
	if strings.HasSuffix(c.dest, "/") || dest == "." {
		for _, src := range c.srcs {
			if err := copyTree(filepath.Join(context, src), filepath.Join(stage, dest, path.Base(src))); err != nil {
				return err
			}
		}
		return nil
	}
	if len(c.srcs) != 1 {
		return fmt.Errorf("COPY %v %s: several sources need a destination ending in /", c.srcs, c.dest)
	}
	return copyTree(filepath.Join(context, c.srcs[0]), filepath.Join(stage, dest))
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
}
