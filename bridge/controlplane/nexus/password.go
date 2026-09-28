// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// PasswordLength is the length of the passwords GeneratePassword returns.
// 48 characters from a 74-character alphabet carry about 298 bits.
const PasswordLength = 48

// Character classes of generated passwords. Every password holds at least
// one character of each class, so a password policy an operator sets with
// Nexus's nexus.password.validator that demands them is met. The symbol
// class leaves out '&' and '[' (with them "docker login" against Nexus
// failed, NEXUS-40119), ':' (the Basic Auth separator), and the characters
// JSON or a text/plain body could escape or trim: quotes, '\\', '%', '/',
// whitespace and control characters. A validator that forbids one of the
// remaining symbols makes user creation fail with 400 (ADR-0036).
const (
	lowerChars  = "abcdefghijklmnopqrstuvwxyz"
	upperChars  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digitChars  = "0123456789"
	symbolChars = "!#*+-.=@^_~,"
)

// GeneratePassword returns a random password for a Nexus user: PasswordLength
// characters from crypto/rand, with at least one lower-case letter, one
// upper-case letter, one digit and one symbol.
func GeneratePassword() (string, error) {
	classes := []string{lowerChars, upperChars, digitChars, symbolChars}
	all := lowerChars + upperChars + digitChars + symbolChars
	out := make([]byte, 0, PasswordLength)
	for _, class := range classes {
		c, err := pick(class)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	for len(out) < PasswordLength {
		c, err := pick(all)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	// Fisher-Yates, so the guaranteed characters sit at random positions.
	for i := len(out) - 1; i > 0; i-- {
		j, err := randIndex(i + 1)
		if err != nil {
			return "", err
		}
		out[i], out[j] = out[j], out[i]
	}
	return string(out), nil
}

func pick(chars string) (byte, error) {
	i, err := randIndex(len(chars))
	if err != nil {
		return 0, err
	}
	return chars[i], nil
}

// randIndex returns a uniform random index in [0, n).
func randIndex(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("generate password: %w", err)
	}
	return int(v.Int64()), nil
}
