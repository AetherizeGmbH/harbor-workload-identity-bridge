// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// extraArgsKey is the variable of the kubelet environment file whose
// value the kubelet unit expands, unquoted, into kubelet's command line.
const extraArgsKey = "KUBELET_EXTRA_ARGS"

// mergeExtraArgs rewrites the KUBELET_EXTRA_ARGS assignment of a kubelet
// environment file such as /etc/default/kubelet (kubeletEnvFile): existing
// args are preserved, stale --image-credential-provider-* occurrences are
// stripped, and ours are appended. Every other line round-trips verbatim. The pre-ADR-0021
// script overwrote the whole file, clobbering operator-set args.
//
// The file is read as systemd reads an EnvironmentFile (parseEnvFile), so
// that the line it rewrites is the assignment kubelet gets: systemd
// accepts whitespace around the "=" and before the value, and when a
// variable is assigned twice the last assignment wins. An assignment the
// installer did not recognise used to get a second one appended after it,
// which silently dropped the operator's args; that appended line is merged
// back into the operator's (appendedByEarlierInstaller). Anything it cannot
// rewrite faithfully is refused, and so is a file systemd does not load
// (errUnloadableEnvFile).
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

	var extra []envAssignment
	for _, a := range assignments {
		if a.key == extraArgsKey {
			extra = append(extra, a)
		}
	}
	appended := -1
	if len(extra) == 2 && appendedByEarlierInstaller(extra, lines) {
		// Kubelet has used the appended line, the second, and not the
		// operator's args. Merge the flags into the operator's line and
		// drop the appended one: kubelet gets the operator's args back.
		appended = extra[1].first
		logf("line %d of the kubelet environment file assigns KUBELET_EXTRA_ARGS only the two --image-credential-provider-* flags: an installer before this version appended it because it did not recognise the assignment on line %d, and kubelet has run without the args of line %d since. Merging the flags into line %d and removing line %d; kubelet gets those args back", appended+1, extra[0].first+1, extra[0].first+1, extra[0].first+1, appended+1)
		extra = extra[:1]
	}
	if len(extra) > 1 {
		return nil, fmt.Errorf("multiple KUBELET_EXTRA_ARGS lines (lines %d and %d) — refusing to guess which one kubelet uses", extra[0].first+1, extra[1].first+1)
	}
	found := len(extra) == 1
	for _, a := range extra {
		if a.first != a.last {
			return nil, fmt.Errorf("KUBELET_EXTRA_ARGS continues over several lines (lines %d-%d), which the installer cannot rewrite safely; put it on one line, or add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", a.first+1, a.last+1)
		}
		if a.afterContinuedComment {
			return nil, fmt.Errorf("KUBELET_EXTRA_ARGS (line %d) follows a comment line that ends in a backslash, which systemd before v254 reads as part of the comment; remove the backslash", a.first+1)
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
		return nil, fmt.Errorf("the last line of the file ends inside a quoted value, after an escape, or in a comment continued with a backslash, so the installer cannot add to the file safely; fix that line")
	}
	if appended >= 0 {
		lines = append(lines[:appended], lines[appended+1:]...)
	}
	if !found {
		lines = append(lines, extraArgsKey+`="`+strings.Join(ours, " ")+`"`)
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// appendedByEarlierInstaller reports whether extra, the two
// KUBELET_EXTRA_ARGS assignments of a kubelet environment file, has the
// shape installers before this version left (audit #94): they recognised
// only a line that starts with "KUBELET_EXTRA_ARGS=" after its leading and
// trailing whitespace, missed an operator's assignment with whitespace
// before the "=", and appended their own line, which systemd prefers
// because it comes later. Every later pass of theirs rewrote only that
// line, to exactly their two flags. So the first assignment is one they
// did not recognise, and the second is a line of its own holding exactly
// the two flags in their order and form.
func appendedByEarlierInstaller(extra []envAssignment, lines []string) bool {
	operator, ours := extra[0], extra[1]
	if strings.HasPrefix(strings.TrimSpace(lines[operator.first]), extraArgsKey+"=") {
		return false
	}
	if ours.first != ours.last || ours.afterContinuedComment {
		return false
	}
	value, ok := strings.CutPrefix(lines[ours.first], extraArgsKey+`="`)
	if !ok {
		return false
	}
	if value, ok = strings.CutSuffix(value, `"`); !ok {
		return false
	}
	binDir, configFile, ok := strings.Cut(value, " ")
	if !ok {
		return false
	}
	binDir, ok1 := strings.CutPrefix(binDir, flagBinDir+"=")
	configFile, ok2 := strings.CutPrefix(configFile, flagConfigFile+"=")
	return ok1 && ok2 && binDir != "" && configFile != "" && !strings.ContainsAny(binDir+configFile, " \t\"'\\$`")
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
		return "", fmt.Errorf("KUBELET_EXTRA_ARGS contains quoting, escapes, or expansions (%q) the installer cannot rewrite safely; add the two --image-credential-provider-* flags yourself and use plugin.install.mode=none", val)
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

// errUnloadableEnvFile marks an EnvironmentFile that systemd does not load
// at all: with a "-" before its name in EnvironmentFile= it skips the whole
// file silently, without it the unit does not start.
var errUnloadableEnvFile = errors.New("systemd does not load the file")

// systemdValidUTF8 reports whether s is UTF-8 as systemd's utf8_is_valid
// accepts it (src/basic/utf8.c): valid UTF-8 without the noncharacters
// U+FDD0..U+FDEF and U+xFFFE/U+xFFFF, which Go's utf8.ValidString allows.
func systemdValidUTF8(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if (r >= 0xFDD0 && r <= 0xFDEF) || r&0xFFFE == 0xFFFE {
			return false
		}
	}
	return true
}

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
// there, and line-based rewriting would not. A file systemd does not load
// at all is refused with errUnloadableEnvFile: one with a NUL byte
// (read_full_stream) or with a key or value that is not valid UTF-8
// (load_env_file_push, systemdValidUTF8).
func parseEnvFile(content string) ([]envAssignment, error) {
	if i := strings.IndexByte(content, 0); i >= 0 {
		return nil, fmt.Errorf("%w: line %d holds a NUL byte, and systemd loads no variable from such a file; remove it", errUnloadableEnvFile, strings.Count(content[:i], "\n")+1)
	}
	for i := 0; i < len(content); i++ {
		if content[i] == '\r' && (i+1 == len(content) || content[i+1] != '\n') {
			return nil, fmt.Errorf("the file has a carriage return inside a line (byte %d), which systemd reads as a line break; the installer cannot rewrite the file safely", i)
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
	push := func() error {
		if lastKeyWS >= 0 {
			key = key[:lastKeyWS]
		}
		for _, part := range []struct{ what, text string }{{"name", string(key)}, {"value", string(value)}} {
			if !systemdValidUTF8(part.text) {
				return fmt.Errorf("%w: the variable %s on line %d (%q) is not valid UTF-8, and systemd loads no variable from such a file; fix that line", errUnloadableEnvFile, part.what, first+1, part.text)
			}
		}
		out = append(out, envAssignment{key: string(key), value: string(value), first: first, last: line, afterContinuedComment: startsContinued})
		key, value = nil, nil
		return nil
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
				if err := push(); err != nil {
					return nil, err
				}
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
				if err := push(); err != nil {
					return nil, err
				}
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
		if err := push(); err != nil {
			return nil, err
		}
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
