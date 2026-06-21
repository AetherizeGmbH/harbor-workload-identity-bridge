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

FROM gcr.io/go-containerregistry/crane:v0.21.6@sha256:0ae17ecb34315aa7cbff28f6eddee3b7adae0b2f90101260d990804db1eb0084 AS crane

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
RUN apk add --no-cache curl ca-certificates jq openssl
COPY --from=crane /ko-app/crane /usr/local/bin/crane
ENTRYPOINT ["/bin/sh"]
