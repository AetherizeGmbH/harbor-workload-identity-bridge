# Sonatype Nexus Repository for the Nexus e2e (tests/03-nexus.tftest.hcl,
# ADR-0036 decision i), labelled with the platform it is built on.
#
# 3.76.1 is the last Community Edition release without the EULA gate:
# 3.77 and later refuse service until an operator accepts the EULA, which
# this project never does on anyone's behalf. renovate.json holds the pin
# below 3.77.
#
# Sonatype publishes this release for linux/amd64 only. The CRI plugin of
# containerd resolves an image with the node's platform, so on an arm64
# host a kind node cannot start the pinned image, not even after
# `kind load`. The second stage copies the pinned image's filesystem into
# an image of the build platform: the binaries stay amd64 and run through
# the host kernel's binfmt handler (Docker Desktop registers Rosetta and
# qemu with the F flag, so they work inside the kind node's containers
# too). On an amd64 host the copy changes nothing but the image id. The
# instructions after the copy repeat the pinned image's configuration
# (`docker image inspect`), with its default heap, which the harness
# overrides.
FROM --platform=linux/amd64 sonatype/nexus3:3.76.1@sha256:390b8ec7e712c680ed8b449b766695a7c5ebbf41e489317618e63ca199f2cc09 AS nexus

FROM scratch
COPY --from=nexus / /
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    container=oci \
    SONATYPE_DIR=/opt/sonatype \
    NEXUS_HOME=/opt/sonatype/nexus \
    NEXUS_DATA=/nexus-data \
    NEXUS_CONTEXT= \
    SONATYPE_WORK=/opt/sonatype/sonatype-work \
    DOCKER_TYPE=rh-docker \
    INSTALL4J_ADD_VM_PARAMS="-Xms2703m -Xmx2703m -XX:MaxDirectMemorySize=2703m -Djava.util.prefs.userRoot=/nexus-data/javaprefs"
WORKDIR /opt/sonatype
VOLUME ["/nexus-data"]
EXPOSE 8081
USER nexus
CMD ["/opt/sonatype/nexus/bin/nexus", "run"]
