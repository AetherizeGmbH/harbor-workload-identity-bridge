// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"strings"
)

// extraArgsKey is the variable of the kubelet environment file whose
// value the kubelet unit expands, unquoted, into kubelet's command line.
const extraArgsKey = "KUBELET_EXTRA_ARGS"

// mergeExtraArgs rewrites the KUBELET_EXTRA_ARGS assignment of an
// /etc/default/kubelet file: existing args are preserved, stale
// --image-credential-provider-* occurrences are stripped, and ours are
// appended. Every other line round-trips verbatim. The pre-ADR-0021
// script overwrote the whole file, clobbering operator-set args.
//
// The file is read as systemd reads an EnvironmentFile (parseEnvFile), so
// that the line it rewrites is the assignment kubelet gets: systemd
// accepts whitespace around the "=" and before the value, and when a
// variable is assigned twice the last assignment wins. An assignment the
// installer did not recognise used to get a second one appended after it,
// which silently dropped the operator's args. Anything it cannot rewrite
// faithfully is refused.
func mergeExtraArgs(existing []byte, binDir, configFile string) ([]byte, error) {
	ours := []string{
		flagBinDir + "=" + binDir,
		flagConfigFile + "=" + configFile,
	}

	assignments, err := parseEnvFile(string(existing))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(existing), "\n")
	// Drop a single trailing empty segment so we can re-join with a
	// guaranteed final newline.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	found := false
	for _, a := range assignments {
		if a.key != extraArgsKey {
			continue
		}
		if found {
			return nil, fmt.Errorf("multiple KUBELET_EXTRA_ARGS lines in /etc/default/kubelet — refusing to guess which one kubelet uses")
		}
		found = true
		if a.first != a.last {
			return nil, fmt.Errorf("KUBELET_EXTRA_ARGS in /etc/default/kubelet continues over several lines (lines %d-%d), which the installer cannot rewrite safely; put it on one line, or add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", a.first+1, a.last+1)
		}
		if a.afterContinuedComment {
			return nil, fmt.Errorf("KUBELET_EXTRA_ARGS in /etc/default/kubelet (line %d) follows a comment line that ends in a backslash, which systemd before v254 reads as part of the comment; remove the backslash", a.first+1)
		}
		_, raw, _ := strings.Cut(lines[a.first], "=")
		val, err := unquoteExtraArgs(strings.Trim(raw, envWhitespace))
		if err != nil {
			return nil, err
		}
		args := stripCredentialProviderFlags(splitArgs(val))
		args = append(args, ours...)
		lines[a.first] = extraArgsKey + `="` + strings.Join(args, " ") + `"`
	}
	// Appending a line, or the final newline the file lacks, must not
	// change what the file's last line means.
	if (!found || !bytes.HasSuffix(existing, []byte("\n"))) && openAtEnd(string(existing)) {
		return nil, fmt.Errorf("the last line of /etc/default/kubelet ends inside a quoted value, after an escape, or in a comment continued with a backslash, so the installer cannot add to the file safely; fix that line")
	}
	if !found {
		lines = append(lines, extraArgsKey+`="`+strings.Join(ours, " ")+`"`)
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// unquoteExtraArgs strips exactly one matching pair of outer quotes. The
// value is an EnvironmentFile assignment that the kubelet unit expands as
// unquoted $KUBELET_EXTRA_ARGS (whitespace-split into argv). The installer
// re-renders it as a double-quoted, space-joined list, which is only
// faithful when no argument carries quotes, escapes, or embedded spaces of
// its own. Anything else is refused rather than rewritten: a mangled line
// can keep kubelet from starting after the restart (audit M5). The old
// strings.Trim stripped ANY number of quote characters from both ends,
// unbalancing a value like --register-with-taints="x:NoSchedule".
func unquoteExtraArgs(val string) (string, error) {
	if n := len(val); n >= 2 && (val[0] == '"' || val[0] == '\'') && val[n-1] == val[0] {
		val = val[1 : n-1]
	}
	if strings.ContainsAny(val, "\"'\\$`") {
		return "", fmt.Errorf("KUBELET_EXTRA_ARGS in /etc/default/kubelet contains quoting, escapes, or expansions (%q) the installer cannot rewrite safely; add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", val)
	}
	return val, nil
}

// splitArgs splits an unquoted $KUBELET_EXTRA_ARGS the way systemd splits
// it into kubelet's argv: at spaces, tabs and line breaks only, not at any
// other Unicode space.
func splitArgs(val string) []string {
	return strings.FieldsFunc(val, func(r rune) bool { return strings.ContainsRune(envWhitespace, r) })
}

// stripCredentialProviderFlags removes both the "--flag=value" and
// "--flag value" forms of the two credential-provider flags.
func stripCredentialProviderFlags(args []string) []string {
	out := make([]string, 0, len(args))
	skipNext := false
	for _, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == flagBinDir || arg == flagConfigFile {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, flagBinDir+"=") || strings.HasPrefix(arg, flagConfigFile+"=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

// envAssignment is one assignment of an EnvironmentFile, as systemd reads
// it (parseEnvFile).
type envAssignment struct {
	key, value string
	// first and last are the indices of the lines (the file split at "\n")
	// the assignment starts and ends on.
	first, last int
	// afterContinuedComment is set when the line before the assignment is
	// a comment that ends in a backslash: systemd before v254 continued
	// such a comment onto the next line and did not read this assignment.
	afterContinuedComment bool
}

// Character classes of systemd's env-file parser (src/basic/string-util.h).
const (
	envWhitespace      = " \t\n\r"
	envNewline         = "\n\r"
	envComments        = "#;"
	envShellNeedEscape = "\"\\`$"
)

// parseEnvFile returns the assignments of an EnvironmentFile in the order
// systemd reads them, a port of parse_env_file_internal (systemd
// src/basic/env-file.c, v254 and later). Leading whitespace is skipped; a
// line whose first other character is '#' or ';' is a comment; the key is
// the text before the first '=' without trailing whitespace; the value
// starts after the whitespace that follows the '=', loses its trailing
// whitespace unless quoted, and continues over several lines inside quotes
// or after a backslash. A line without '=' is ignored. A key that is not a
// valid variable name is returned as it is (systemd ignores it). A
// carriage return that does not end a line is refused: systemd ends a line
// there, and line-based rewriting would not.
func parseEnvFile(content string) ([]envAssignment, error) {
	for i := 0; i < len(content); i++ {
		if content[i] == '\r' && (i+1 == len(content) || content[i+1] != '\n') {
			return nil, fmt.Errorf("/etc/default/kubelet has a carriage return inside a line (byte %d), which systemd reads as a line break; the installer cannot rewrite the file safely", i)
		}
	}
	const (
		preKey = iota
		inKey
		preValue
		inValue
		valueEscape
		singleQuote
		doubleQuote
		doubleQuoteEscape
		comment
		commentEscape
	)
	var (
		out              []envAssignment
		key, value       []byte
		lastKeyWS        = -1
		lastValueWS      = -1
		line, first      int
		continuedComment bool // the line before this one is a comment ending in '\'
		startsContinued  bool
		state            = preKey
	)
	push := func() {
		if lastKeyWS >= 0 {
			key = key[:lastKeyWS]
		}
		out = append(out, envAssignment{key: string(key), value: string(value), first: first, last: line, afterContinuedComment: startsContinued})
		key, value = nil, nil
	}
	for i := 0; i < len(content); i++ {
		c := content[i]
		isNewline := strings.IndexByte(envNewline, c) >= 0
		isWS := strings.IndexByte(envWhitespace, c) >= 0
		switch state {
		case preKey:
			switch {
			case strings.IndexByte(envComments, c) >= 0:
				state = comment
			case !isWS:
				state = inKey
				lastKeyWS = -1
				first, startsContinued = line, continuedComment
				key = append(key[:0], c)
			}
		case inKey:
			switch {
			case isNewline:
				state = preKey
				key = key[:0]
			case c == '=':
				state = preValue
				lastValueWS = -1
			default:
				if !isWS {
					lastKeyWS = -1
				} else if lastKeyWS < 0 {
					lastKeyWS = len(key)
				}
				key = append(key, c)
			}
		case preValue:
			switch {
			case isNewline:
				state = preKey
				push()
			case c == '\'':
				state = singleQuote
			case c == '"':
				state = doubleQuote
			case c == '\\':
				state = valueEscape
			case !isWS:
				state = inValue
				value = append(value, c)
			}
		case inValue:
			switch {
			case isNewline:
				state = preKey
				if lastValueWS >= 0 {
					value = value[:lastValueWS]
				}
				push()
			case c == '\\':
				state = valueEscape
				lastValueWS = -1
			default:
				if !isWS {
					lastValueWS = -1
				} else if lastValueWS < 0 {
					lastValueWS = len(value)
				}
				value = append(value, c)
			}
		case valueEscape:
			state = inValue
			if !isNewline {
				value = append(value, c)
			}
		case singleQuote:
			if c == '\'' {
				state = preValue
			} else {
				value = append(value, c)
			}
		case doubleQuote:
			switch c {
			case '"':
				state = preValue
			case '\\':
				state = doubleQuoteEscape
			default:
				value = append(value, c)
			}
		case doubleQuoteEscape:
			state = doubleQuote
			switch {
			case strings.IndexByte(envShellNeedEscape, c) >= 0:
				value = append(value, c)
			case c != '\n':
				value = append(value, '\\', c)
			}
		case comment:
			switch {
			case c == '\\':
				state = commentEscape
			case isNewline:
				state = preKey
			}
		case commentEscape:
			if isNewline {
				state = preKey
			} else {
				state = comment
			}
		}
		if c == '\n' {
			continuedComment = state == preKey && commentEndsInBackslash(content, i)
			line++
		}
	}
	switch state {
	case preValue, inValue, valueEscape, singleQuote, doubleQuote, doubleQuoteEscape:
		if state == inValue && lastValueWS >= 0 {
			value = value[:lastValueWS]
		}
		push()
	}
	return out, nil
}

// commentEndsInBackslash reports whether the line that ends at the newline
// at index end of content is a comment (its first non-whitespace character
// is '#' or ';') whose last character before the line break is a
// backslash.
func commentEndsInBackslash(content string, end int) bool {
	start := strings.LastIndexByte(content[:end], '\n') + 1
	body := strings.TrimSuffix(content[start:end], "\r")
	trimmed := strings.TrimLeft(body, envWhitespace)
	return trimmed != "" && strings.IndexByte(envComments, trimmed[0]) >= 0 && strings.HasSuffix(body, "\\")
}

// openAtEnd reports whether a line appended after content would not be an
// assignment of its own: content ends inside quotes or right after a
// backslash (a value escape, or a comment that systemd before v254
// continued onto the next line).
func openAtEnd(content string) bool {
	trimmed := strings.TrimRight(content, "\r\n")
	if strings.HasSuffix(trimmed, "\\") {
		return true
	}
	probe := content
	if probe != "" && !strings.HasSuffix(probe, "\n") {
		probe += "\n"
	}
	const sentinel = "HARBOR_BRIDGE_INSTALLER_PROBE"
	assignments, err := parseEnvFile(probe + sentinel + "=x\n")
	if err != nil || len(assignments) == 0 {
		return true
	}
	last := assignments[len(assignments)-1]
	return last.key != sentinel || last.value != "x"
}
