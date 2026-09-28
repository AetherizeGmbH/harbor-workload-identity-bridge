// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
)

// kubeletEnvFiles are the files patch mode may edit to put its flags into
// KUBELET_EXTRA_ARGS (ADR-0034): the operator file of kubeadm's Debian
// packages and that of its RPM packages (kubernetes/release,
// cmd/krel/templates/latest/kubeadm/10-kubeadm.conf and kubeadm.spec).
// Never another file a unit reads: kubeadm regenerates
// /var/lib/kubelet/kubeadm-flags.env on join and upgrade.
var kubeletEnvFiles = []string{defaultKubeletPath, "/etc/sysconfig/kubelet"}

// setsExtraArgs matches an Environment= property that assigns
// KUBELET_EXTRA_ARGS.
var setsExtraArgs = regexp.MustCompile(`(^|[\s"'])` + extraArgsKey + `=`)

// kubeletEnvFile returns the node path of the environment file patch mode
// puts its flags into (ADR-0034): the one KUBELET_EXTRA_ARGS of the kubelet
// unit comes from. systemd reads the files of the unit's EnvironmentFile=
// settings in order, a wildcard expression expanded to its matches
// (expandEnvFile), and a later assignment wins, so that is the last file
// that assigns it, read as systemd reads it (parseEnvFile); when none does,
// the last listed of kubeletEnvFiles. A file systemd skips (a directory, or
// a file it does not load, behind a "-" setting) assigns nothing. It
// refuses, before the pass writes anything, a unit that lists neither of
// kubeletEnvFiles, an assignment that comes from another file, a
// KUBELET_EXTRA_ARGS set only with Environment=, which a line in the file
// would override, and a file it cannot read as systemd does.
func (c *config) kubeletEnvFile() (string, error) {
	env, err := c.kubelet.environment(c.KubeletUnit)
	if err != nil {
		return "", fmt.Errorf("find the environment files of kubelet unit %q: %w", c.KubeletUnit, err)
	}
	var assigning, lastOperator string
	for _, ref := range env.Files {
		if !filepath.IsAbs(ref.Path) {
			return "", fmt.Errorf("kubelet unit %q names the environment file %q, which is not an absolute path", c.KubeletUnit, ref.Path)
		}
		files, err := c.expandEnvFile(ref.Path)
		if err != nil {
			return "", fmt.Errorf("the environment files %q of kubelet unit %q: %w; add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", ref.Path, c.KubeletUnit, err)
		}
		for _, f := range files {
			operator := slices.Contains(kubeletEnvFiles, f)
			if operator {
				lastOperator = f
			}
			assignments, err := c.readEnvFile(f, ref.Optional && !operator)
			if err != nil {
				return "", err
			}
			for _, a := range assignments {
				if a.key == extraArgsKey {
					assigning = f
				}
			}
		}
	}
	switch {
	case assigning != "" && !slices.Contains(kubeletEnvFiles, assigning):
		return "", fmt.Errorf("kubelet unit %q takes KUBELET_EXTRA_ARGS from %s, which the installer does not edit (it edits only %s); add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", c.KubeletUnit, assigning, strings.Join(kubeletEnvFiles, " or "))
	case assigning != "":
		return assigning, nil
	case lastOperator == "":
		return "", fmt.Errorf("kubelet unit %q reads neither %s (its environment files: %s), so patch mode has nowhere to put the kubelet flags; add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", c.KubeletUnit, strings.Join(kubeletEnvFiles, " nor "), describeFiles(env.Files))
	case setsExtraArgs.MatchString(env.Assignments):
		return "", fmt.Errorf("kubelet unit %q sets KUBELET_EXTRA_ARGS with Environment=, which a KUBELET_EXTRA_ARGS line in %s would override and drop; move it into %s, or add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", c.KubeletUnit, lastOperator, lastOperator)
	}
	return lastOperator, nil
}

// readEnvFile returns the assignments systemd takes from the environment
// file at the node path f of the kubelet unit. A missing file has none. So
// has, when skippable is set, a file systemd skips: a directory, or a file
// it does not load at all (errUnloadableEnvFile). skippable is set for a
// file behind a "-" setting that patch mode would not edit: a file it
// edits must be one systemd loads, or the flags never reach kubelet.
func (c *config) readEnvFile(f string, skippable bool) ([]envAssignment, error) {
	data, err := readHostFile(c.hostPath(f))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil && skippable && isHostDir(c.hostPath(f)):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %s, an environment file of kubelet unit %q: %w", f, c.KubeletUnit, err)
	}
	assignments, err := parseEnvFile(string(data))
	switch {
	case errors.Is(err, errUnloadableEnvFile) && skippable:
		logf("%s, an environment file of kubelet unit %q, sets nothing: %v", f, c.KubeletUnit, err)
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%s: %w", f, err)
	}
	return assignments, nil
}

// isHostDir reports whether path is a directory, without following a
// symlink in its last component.
func isHostDir(path string) bool {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return false
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(filepath.Base(path))
	return err == nil && fi.IsDir()
}

func describeFiles(files []envFileRef) string {
	if len(files) == 0 {
		return "none"
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	return strings.Join(paths, ", ")
}

// expandEnvFile returns the node paths of the files systemd reads for the
// EnvironmentFile= setting pattern (an absolute file name or wildcard
// expression, systemd.exec(5)), in its order. systemd passes every setting
// to glob(3) without flags (safe_glob in exec_context_load_environment), in
// the C locale: a path component with a wildcard (globMagic) matches the
// entries of its directory (globMatch), a component without one is taken
// as it is, backslash escapes removed, and the matches are sorted
// bytewise. A pattern without a wildcard is the one file it names, which
// may be missing. A matched directory on the way to the last component
// must be a directory, never a symlink: systemd follows symlinks, which the
// installer cannot resolve on the node from its mount, so it refuses one.
// A matched last component can be anything; the caller reads it
// (readEnvFile).
func (c *config) expandEnvFile(pattern string) ([]string, error) {
	components := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	if !slices.ContainsFunc(components, globMagic) {
		for i, comp := range components {
			components[i] = globUnescape(comp)
		}
		return []string{"/" + strings.Join(components, "/")}, nil
	}
	paths := []string{"/"}
	for i, comp := range components {
		if !globMagic(comp) {
			for j := range paths {
				paths[j] = filepath.Join(paths[j], globUnescape(comp))
			}
			continue
		}
		var next []string
		for _, dir := range paths {
			matches, err := c.globDir(dir, comp, i == len(components)-1)
			if err != nil {
				return nil, err
			}
			next = append(next, matches...)
		}
		paths = next
	}
	sort.Strings(paths)
	return paths, nil
}

// globDir returns the node paths of the entries of the node directory dir
// that match the wildcard component pattern (globMatch). Unless last is
// set, only directories count, as glob(3) descends only into those. A
// missing directory, or a file in its place, has no entries.
func (c *config) globDir(dir, pattern string, last bool) ([]string, error) {
	root, err := os.OpenRoot(c.hostPath(dir))
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return nil, nil
	case err != nil:
		return nil, err
	}
	defer func() { _ = root.Close() }()
	d, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		ok, err := globMatch(pattern, e.Name())
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		path := filepath.Join(dir, e.Name())
		switch {
		case last:
		case e.Type()&fs.ModeSymlink != 0:
			return nil, fmt.Errorf("%s is a symlink, which systemd follows and the installer does not", path)
		case !e.IsDir():
			continue
		}
		out = append(out, path)
	}
	return out, nil
}

// globMagic reports whether the path component comp holds a wildcard, as
// glibc's __glob_pattern_type decides it: an unescaped '*' or '?', or an
// unescaped '[' followed by an unescaped ']'. A '[' without a ']' is a
// literal character.
func globMagic(comp string) bool {
	bracket := false
	for i := 0; i < len(comp); i++ {
		switch comp[i] {
		case '*', '?':
			return true
		case '\\':
			i++
		case '[':
			bracket = true
		case ']':
			if bracket {
				return true
			}
		}
	}
	return false
}

// globUnescape removes the backslash escapes from a component without a
// wildcard, as glob(3) does before it looks the name up.
func globUnescape(comp string) string {
	if !strings.Contains(comp, `\`) {
		return comp
	}
	var b strings.Builder
	for i := 0; i < len(comp); i++ {
		if comp[i] == '\\' && i+1 < len(comp) {
			i++
		}
		b.WriteByte(comp[i])
	}
	return b.String()
}

// globMatch reports whether the file name name matches the wildcard
// component pattern as glibc's fnmatch with FNM_PERIOD matches it in the C
// locale, byte by byte: '*' matches any bytes, '?' one byte, a bracket
// expression one byte of its set ("[!...]" or "[^...]" negates it, a ']'
// first in it is a member, "a-z" is a byte range), a backslash escapes the
// next byte, and a leading '.' of the name matches only a literal '.'. A
// '[' without a closing ']' is a literal character, and a trailing
// backslash matches nothing. Character classes, equivalence classes and
// collating symbols ("[:", "[=", "[." in a bracket expression) are not
// evaluated: they are an error.
func globMatch(pattern, name string) (bool, error) {
	if strings.HasPrefix(name, ".") && !strings.HasPrefix(pattern, ".") && !strings.HasPrefix(pattern, `\.`) {
		return false, nil
	}
	p, n := 0, 0
	// The pattern position after the last '*' and the name position it
	// matched up to: on a mismatch that '*' takes one more byte.
	starP, starN := -1, 0
	for n < len(name) || p < len(pattern) {
		if p < len(pattern) {
			switch c := pattern[p]; c {
			case '*':
				starP, starN = p+1, n
				p++
				continue
			case '?':
				if n < len(name) {
					p++
					n++
					continue
				}
			case '[':
				end, match, err := globBracket(pattern, p, name, n)
				if err != nil {
					return false, err
				}
				switch {
				case end < 0 && n < len(name) && name[n] == '[':
					// No closing ']': a literal '['.
					p++
					n++
					continue
				case end >= 0 && match:
					p = end
					n++
					continue
				}
			case '\\':
				if p+1 < len(pattern) && n < len(name) && name[n] == pattern[p+1] {
					p += 2
					n++
					continue
				}
			default:
				if n < len(name) && name[n] == c {
					p++
					n++
					continue
				}
			}
		}
		if starP >= 0 && starN < len(name) {
			starN++
			p, n = starP, starN
			continue
		}
		return false, nil
	}
	return true, nil
}

// globBracket evaluates the bracket expression that starts at pattern[p]
// ('[') against the byte name[n]. It returns the pattern position after the
// expression and whether the byte is in its set; end is -1 when the
// expression has no closing ']' (a literal '['). A missing byte (n at the
// end of name) is in no set.
func globBracket(pattern string, p int, name string, n int) (end int, match bool, err error) {
	i := p + 1
	negate := false
	if i < len(pattern) && (pattern[i] == '!' || pattern[i] == '^') {
		negate = true
		i++
	}
	in := false
	for first := true; ; first = false {
		if i >= len(pattern) {
			return -1, false, nil
		}
		c := pattern[i]
		switch {
		case c == ']' && !first:
			return i + 1, n < len(name) && in != negate, nil
		case c == '[' && i+1 < len(pattern) && strings.IndexByte(":=.", pattern[i+1]) >= 0:
			return 0, false, fmt.Errorf("the wildcard expression %q has a character class, equivalence class or collating symbol, which the installer does not evaluate", pattern)
		case c == '\\':
			if i+1 >= len(pattern) {
				return -1, false, nil
			}
			i++
			c = pattern[i]
		}
		i++
		lo, hi := c, c
		if i+1 < len(pattern) && pattern[i] == '-' && pattern[i+1] != ']' {
			i++
			hi = pattern[i]
			if hi == '\\' {
				if i+1 >= len(pattern) {
					return -1, false, nil
				}
				i++
				hi = pattern[i]
			}
			i++
		}
		if n < len(name) && lo <= name[n] && name[n] <= hi {
			in = true
		}
	}
}
