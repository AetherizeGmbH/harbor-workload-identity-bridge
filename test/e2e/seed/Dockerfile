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

FROM gcr.io/go-containerregistry/crane:v0.21.7@sha256:6f95e0e453f09944a625fa219d6879aa2dfaf3f4c7fe3ac9bf41a0a41d45ec46 AS crane

FROM alpine:3.20
RUN apk add --no-cache curl ca-certificates jq openssl
COPY --from=crane /ko-app/crane /usr/local/bin/crane
ENTRYPOINT ["/bin/sh"]
