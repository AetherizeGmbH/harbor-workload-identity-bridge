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

FROM gcr.io/go-containerregistry/crane:v0.21.8@sha256:e85fa14e99e5ef7351822ed887ce322bc1e22f9ef8e9eb54ebf2d7a9473bfef9 AS crane

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN apk add --no-cache curl ca-certificates jq openssl
COPY --from=crane /ko-app/crane /usr/local/bin/crane
ENTRYPOINT ["/bin/sh"]
