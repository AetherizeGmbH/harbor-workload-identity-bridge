# 1. Record architecture decisions

## Status

Accepted

## Context

We need to document architectural decisions made for the Harbor Workload Identity Bridge so future maintainers (including a future-us) can understand why the code looks the way it does. Without an explicit record, we repeat debates and quietly drift away from intentional choices.

## Decision

Significant architectural decisions are recorded in this `docs/adr/` directory using the [Michael Nygard ADR format][nygard]. One file per decision, sequentially numbered, named `NNNN-kebab-case-title.md`. Each ADR fits on roughly one page and contains Context, Decision, Status, Consequences sections, optionally Alternatives Considered.

An ADR is required whenever we:

- Pick between two reasonable libraries
- Decide to write custom code instead of using a library
- Make a security tradeoff
- Commit to an API or protocol decision that is hard to revisit
- Split or merge components

We do not write ADRs for trivial choices (naming, file layout inside a package).

ADRs are immutable once Accepted. To reverse a decision we add a new ADR that supersedes the previous one. Both files remain; the old one's Status is updated to `Superseded by ADR-NNNN`.

## Consequences

- Reviewers see why a non-obvious approach was chosen and can engage with the reasoning, not only the code.
- Onboarding is faster: new contributors read the ADRs in order to reconstruct the design.
- We pay a small upfront cost (writing the ADR) for every significant decision. The cost is intentional; if a decision is not worth a one-pager, it probably is not significant.

[nygard]: https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions
