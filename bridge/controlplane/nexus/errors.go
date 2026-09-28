// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package nexus

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Sentinel errors. Every error the client returns for an HTTP status is an
// *APIError, which matches the sentinel of its status class through
// errors.Is. Nexus does not report "already exists" through one status: a
// duplicate role is 400, a duplicate user 500 (DuplicateUserException,
// verified on Nexus 3.76.1, ADR-0033). Callers therefore never infer
// existence from a create error; they read the object again.
var (
	// ErrBadRequest: 400. Nexus uses it for validation errors, unknown
	// roles or privileges, a duplicate role id and read-only objects.
	ErrBadRequest = errors.New("nexus: bad request")

	// ErrUnauthorized: 401, the credentials were rejected.
	ErrUnauthorized = errors.New("nexus: credentials rejected")

	// ErrForbidden: 403, the credentials lack a permission. Nexus also
	// answers 403 to a request without credentials, which this client
	// never sends to a protected endpoint.
	ErrForbidden = errors.New("nexus: permission denied")

	// ErrNotFound: 404, or no exact match in a user listing.
	ErrNotFound = errors.New("nexus: not found")

	// ErrConflict: 409. Nexus uses it when the id in a role update's body
	// differs from the path.
	ErrConflict = errors.New("nexus: conflict")

	// ErrRateLimited: 429. Nexus 3.93 and later answer it to credentials
	// that failed to log in too often, with Retry-After (RetryAfter). The
	// client never retries; the caller waits at least that long.
	ErrRateLimited = errors.New("nexus: rate limited")

	// ErrServer: any 5xx.
	ErrServer = errors.New("nexus: server error")

	// ErrUnexpectedStatus: any other status that is not a success.
	ErrUnexpectedStatus = errors.New("nexus: unexpected status")
)

// APIError is a response from Nexus whose status is not a success.
type APIError struct {
	// Op names the operation, e.g. `create user "bridge-…"`.
	Op string

	// StatusCode is the HTTP status.
	StatusCode int

	// Message is Nexus's explanation from the response body, sanitized to
	// one line of at most maxMessageLen characters. It is empty when the
	// body carried none the client can render (an HTML page, a truncated
	// JSON document) and is replaced by messageWithheld when it contained
	// a credential or password of the request.
	Message string

	// RetryAfter is the delay the Retry-After header asked for; it is
	// meaningful only when HasRetryAfter is true.
	RetryAfter    time.Duration
	HasRetryAfter bool

	// endpointNotFound marks a 404 from a listing endpoint, which always
	// exists on Nexus: the URL points somewhere else. Such an error must
	// not read as "the object does not exist" (a deletion that trusted it
	// would leave a user with a valid password behind).
	endpointNotFound bool
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s: Nexus answered %d", e.Op, e.StatusCode)
	if text := http.StatusText(e.StatusCode); text != "" {
		msg += " " + text
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if e.HasRetryAfter {
		msg += fmt.Sprintf(" (Retry-After %s)", e.RetryAfter)
	}
	if e.endpointNotFound {
		msg += " (a listing endpoint Nexus always serves was not found: check that the Nexus URL points at the server's base URL)"
	}
	return msg
}

// Is reports whether the status matches the sentinel target.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrBadRequest:
		return e.StatusCode == http.StatusBadRequest
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound && !e.endpointNotFound
	case ErrConflict:
		return e.StatusCode == http.StatusConflict
	case ErrRateLimited:
		return e.StatusCode == http.StatusTooManyRequests
	case ErrServer:
		return e.StatusCode >= 500 && e.StatusCode <= 599
	case ErrUnexpectedStatus:
		if e.endpointNotFound {
			return true
		}
		switch e.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
			http.StatusNotFound, http.StatusConflict, http.StatusTooManyRequests:
			return false
		}
		return e.StatusCode < 500 || e.StatusCode > 599
	}
	return false
}

// listingError marks a 404 from a listing endpoint (see endpointNotFound).
func listingError(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		apiErr.endpointNotFound = true
	}
	return err
}

// RetryAfter returns the delay a rate-limited or unavailable Nexus asked
// for with Retry-After. ok is false when err carries no such header. The
// value is Nexus's; the caller bounds it by its own maximum wait.
func RetryAfter(err error) (d time.Duration, ok bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.HasRetryAfter {
		return apiErr.RetryAfter, true
	}
	return 0, false
}

// parseRetryAfter reads a Retry-After value: delay-seconds or an HTTP date
// (RFC 9110 §10.2.3). A date in the past yields 0. ok is false for an
// absent or malformed value and for a delay that time.Duration cannot hold.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if isDigits(v) {
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil || secs > int64(maxDuration/time.Second) {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	if d := t.Sub(now); d > 0 {
		return d, true
	}
	return 0, true
}

const maxDuration = time.Duration(1<<63 - 1)

func isDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

const (
	// maxErrorBody bounds how much of an error response the client reads
	// to explain the error.
	maxErrorBody = 4 << 10

	// maxMessageLen bounds APIError.Message in runes; it ends up in a
	// single-line status condition.
	maxMessageLen = 512

	// messageWithheld replaces a server message that contained a
	// credential or password of the request.
	messageWithheld = "(Nexus's message is withheld: it contained a credential of the request)"
)

// nexusMessage is one entry of Nexus's error documents:
// {"id":"*","message":"\"Role 'x' already exists, use a unique roleId.\""}
// for a WebApplicationMessageException, and a list of them for validation
// errors ([{"id":"PARAMETER emailAddress","message":"must not be empty"}],
// Content-Type application/vnd.siesta-validation-errors-v1+json).
type nexusMessage struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// renderMessage turns an error body into APIError.Message. truncated
// reports that the body was longer than what was read. secrets are the
// credentials and passwords the request carried: a message that contains
// one of them, or, for a truncated body, ends in the start of one, is
// withheld as a whole instead of being edited, so no transformation can
// leave a fragment of a secret behind.
func renderMessage(body []byte, contentType string, truncated bool, secrets []string) string {
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return ""
	}
	var text string
	switch {
	case isJSON(contentType) || strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "["):
		// A truncated document does not parse; nothing is rendered then.
		text = renderJSONMessage(raw)
	case isPlainText(contentType), contentType == "" && !strings.HasPrefix(raw, "<"):
		// Nexus answers a 500 with text/plain ("ERROR: (ID …) …
		// DuplicateUserException: User … already exists.").
		text = raw
	default:
		// HTML error pages (Jetty's, a proxy's) carry nothing useful.
		return ""
	}
	if text == "" {
		return ""
	}
	clean := sanitize(text)
	if leaks([]string{raw, text, clean}, secrets) || (truncated && endsInSecretPrefix(clean, secrets)) {
		return messageWithheld
	}
	if utf8.RuneCountInString(clean) > maxMessageLen {
		r := []rune(clean)
		clean = string(r[:maxMessageLen]) + "…"
	}
	return clean
}

func renderJSONMessage(raw string) string {
	var s string
	if json.Unmarshal([]byte(raw), &s) == nil {
		return s
	}
	var one nexusMessage
	if json.Unmarshal([]byte(raw), &one) == nil && one.Message != "" {
		return formatNexusMessage(one)
	}
	var many []nexusMessage
	if json.Unmarshal([]byte(raw), &many) == nil {
		parts := make([]string, 0, len(many))
		for _, m := range many {
			if m.Message != "" {
				parts = append(parts, formatNexusMessage(m))
			}
		}
		return strings.Join(parts, "; ")
	}
	return ""
}

// formatNexusMessage renders one entry. Nexus quotes the message of a
// WebApplicationMessageException as a JSON string inside the JSON string;
// the id "*" carries no information.
func formatNexusMessage(m nexusMessage) string {
	msg := m.Message
	var unquoted string
	if strings.HasPrefix(msg, `"`) && json.Unmarshal([]byte(msg), &unquoted) == nil {
		msg = unquoted
	}
	if m.ID == "" || m.ID == "*" {
		return msg
	}
	return m.ID + ": " + msg
}

// sanitize drops invalid UTF-8 and control characters: the text ends up in
// a single-line Kubernetes status condition.
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s))
}

// leaks reports whether any text contains any secret, verbatim or with its
// control characters dropped (the form sanitize would leave).
func leaks(texts, secrets []string) bool {
	for _, s := range secrets {
		for _, form := range []string{s, sanitize(s)} {
			if form == "" {
				continue
			}
			for _, t := range texts {
				if strings.Contains(t, form) {
					return true
				}
			}
		}
	}
	return false
}

// endsInSecretPrefix reports whether text ends with a non-empty prefix of
// a secret: a secret cut off by the body limit.
func endsInSecretPrefix(text string, secrets []string) bool {
	for _, s := range secrets {
		for _, form := range []string{s, sanitize(s)} {
			for n := len(form); n > 0; n-- {
				if strings.HasSuffix(text, form[:n]) {
					return true
				}
			}
		}
	}
	return false
}

func isJSON(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

func isPlainText(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "text/plain"
}
