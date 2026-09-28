// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"strings"
	"testing"
)

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != PasswordLength || PasswordLength < 40 {
			t.Fatalf("password has %d characters, want %d (at least 40)", len(pw), PasswordLength)
		}
		for _, class := range []string{lowerChars, upperChars, digitChars, symbolChars} {
			if !strings.ContainsAny(pw, class) {
				t.Fatalf("password %q has no character of %q", pw, class)
			}
		}
		// '&' and '[' broke docker login against Nexus (NEXUS-40119); ':'
		// is the Basic Auth separator; the rest need escaping somewhere.
		if strings.ContainsAny(pw, "&[]:\"'\\%/ \t\n") {
			t.Fatalf("password %q holds a character that must not be used", pw)
		}
		for _, r := range pw {
			if !strings.ContainsRune(lowerChars+upperChars+digitChars+symbolChars, r) {
				t.Fatalf("password %q holds %q, which is in no class", pw, r)
			}
		}
		if seen[pw] {
			t.Fatalf("GeneratePassword repeated %q", pw)
		}
		seen[pw] = true
	}
}

// The guaranteed characters must not sit at fixed positions: over many
// passwords every position holds characters of every class.
func TestGeneratePassword_ShufflesGuaranteedCharacters(t *testing.T) {
	var firstIsLower, firstIsOther int
	for range 400 {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsRune(lowerChars, rune(pw[0])) {
			firstIsLower++
		} else {
			firstIsOther++
		}
	}
	if firstIsLower == 0 || firstIsOther == 0 {
		t.Errorf("first character: %d lower-case, %d other; the guaranteed characters are not shuffled", firstIsLower, firstIsOther)
	}
}
