// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// accessCandidate is what the token match reads from one HarborAccess or
// NexusAccess. Both kinds share the identity model (ADR-0010): the
// ServiceAccount a token must be issued for, and the trust policy's issuer
// and audience. One match for both keeps the security boundary in one
// place.
type accessCandidate struct {
	namespace, name string
	// deleting is set while the object has a deletionTimestamp.
	deleting bool
	// subject is the token subject of spec.serviceAccountRef.
	subject string
	// issuer and audience are spec.trustPolicy's.
	issuer, audience string
}

// matchAccess returns the index of the candidate whose ServiceAccount is
// the token's subject, whose issuer is the token's and whose audience, the
// bridge's, appears in the token's aud claim, together with the aud value
// that matched; -1 when none matches.
//
// A candidate that is being deleted never matches: deletion is the
// revocation, and a finalizer can hold the object in the cache for as long
// as the registry refuses the account's deletion. When nothing else
// matches, the first such candidate is returned as deleting (-1 when there
// is none), for the audit log.
//
// Selection is deterministic: several matches are an operator
// misconfiguration (two objects claim the same workload identity, usually
// with different permissions), and List order is not stable across
// informer caches, so the namespace/name-sorted first match wins and the
// ambiguity is logged. what names the kind in that log line.
func (h *Handler) matchAccess(ctx context.Context, what string, claims *Claims, cands []accessCandidate) (matched int, audience string, deleting int) {
	type match struct {
		i   int
		aud string
	}
	var matches, deletingMatches []match
	for i := range cands {
		c := &cands[i]
		// Defense-in-depth: an object with an empty audience or issuer must
		// never match. The CRDs enforce MinLength=1 on both
		// trustPolicy.audience and trustPolicy.issuer, but the data plane is
		// the security boundary and must not rely solely on CRD validation
		// (an object applied with --validate=false, or a future API revision
		// that relaxes the marker, would otherwise let an empty
		// trustPolicy.audience match a token carrying aud:"" — a silent auth
		// bypass).
		if c.audience == "" || c.issuer == "" {
			continue
		}
		// The bridge serves exactly one audience (ADR-0026). The reconciler
		// already marks an object with another audience not ready; the data
		// plane, as the security boundary, never matches one either. An
		// unset configured audience matches nothing (fail closed).
		if c.audience != h.Config.Audience {
			continue
		}
		// The index already selected on this; kept so the match never
		// depends on how the list was filtered.
		if c.subject != claims.Subject {
			continue
		}
		// Defense-in-depth: the Validator already pins iss to the bridge's
		// configured issuer, and the reconciler refuses to provision an
		// account for an object whose trustPolicy.issuer disagrees with the
		// cluster issuer. Re-checking here means an object is never matched
		// against a token from an issuer it did not declare, even if those
		// upstream invariants regress.
		if c.issuer != claims.Issuer {
			continue
		}
		for _, aud := range claims.Audience {
			// Never honor an empty aud entry, even against a (guarded-above)
			// non-empty audience — keeps the match total over both sides.
			if aud == "" {
				continue
			}
			if aud == c.audience {
				if c.deleting {
					deletingMatches = append(deletingMatches, match{i: i, aud: aud})
				} else {
					matches = append(matches, match{i: i, aud: aud})
				}
				break
			}
		}
	}
	byName := func(ms []match) {
		sort.Slice(ms, func(i, j int) bool {
			a, b := &cands[ms[i].i], &cands[ms[j].i]
			if a.namespace != b.namespace {
				return a.namespace < b.namespace
			}
			return a.name < b.name
		})
	}
	if len(matches) == 0 {
		if len(deletingMatches) == 0 {
			return -1, "", -1
		}
		byName(deletingMatches)
		return -1, "", deletingMatches[0].i
	}
	byName(matches)
	if len(matches) > 1 {
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = cands[m.i].namespace + "/" + cands[m.i].name
		}
		log.FromContext(ctx).WithName("dataplane").Info(
			"multiple "+what+" match this token; selecting deterministically by namespace/name — resolve this ambiguity, the matched CRs grant potentially different permissions",
			"subject", claims.Subject,
			"audience", matches[0].aud,
			"matches", strings.Join(names, ","),
			"selected", names[0],
		)
	}
	return matches[0].i, matches[0].aud, -1
}
