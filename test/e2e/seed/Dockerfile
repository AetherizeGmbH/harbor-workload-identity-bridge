# Image used by the e2e seed_image Job to prepare Harbor with a test
# image. Combines:
#   - curl    : create the Harbor project via Harbor's REST API
#   - jq      : parse Harbor API responses
#   - openssl : grab Harbor's self-signed TLS leaf off the wire so we
#               can add it to the system trust store before crane runs
#               (chart secret naming for the cert is fragile)
#   - crane   : copy alpine:3.20 into harbor.e2e/your-project/alpine:test3
#
# The crane image at gcr.io/go-containerregistry/crane is distroless and
# only ships the crane binary, so we copy that into a small alpine base
# that also has the userland tools above.

FROM gcr.io/go-containerregistry/crane:v0.22.1@sha256:1f968817b95790bed063f71175aa6b8ff879fa17064020415f3e18bb6e6a36e1 AS crane

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
# Both base images are pinned by digest (Renovate bumps them); the
# packages come from the Alpine CDN at build time. A failed fetch there
# makes `apk add` exit non-zero (CI saw exit 4) and, with it, the whole e2e
# run: retry with backoff (2s, 8s, 18s, 32s) before giving up. Package
# versions are not pinned: an Alpine branch keeps only the newest build of
# a package, so a pinned version vanishes with the next security update
# and every build fails until someone bumps it.
RUN for attempt in 1 2 3 4 5; do \
      apk add --no-cache curl ca-certificates jq openssl && exit 0; \
      [ "$attempt" = 5 ] && break; \
      echo "apk add failed (attempt $attempt of 5), retrying" >&2; \
      sleep $((attempt * attempt * 2)); \
    done; \
    exit 1
COPY --from=crane /ko-app/crane /usr/local/bin/crane
# Every tool the seed and check Jobs call, so a broken image fails here
# and not as a confusing error in a Job several stages later.
RUN curl --version >/dev/null && jq --version >/dev/null && openssl version >/dev/null \
    && crane version >/dev/null && command -v update-ca-certificates >/dev/null
ENTRYPOINT ["/bin/sh"]
