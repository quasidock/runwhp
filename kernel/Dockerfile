# Builds runwhp's guest kernel: upstream Linux configured by runwhp.config, in
# a pinned Debian whose packages come from a fixed snapshot, so the same inputs
# give the same bzImage. See "Kernel" in README.md.
#
#   docker build --target kernel  --output type=local,dest=out/kernel  kernel
#   docker build --target config  --output type=local,dest=out/config  kernel
#   docker build --target release --output type=local,dest=out/release kernel
#
# kernel/build.ps1 runs these through WSL. To move to a new upstream release,
# set KERNEL_VERSION and KERNEL_SHA256 (from kernel.org's signed
# sha256sums.asc), set KERNEL_REVISION back to 1 and BUILD_TIMESTAMP to today,
# and regenerate full.config. To publish a rebuild of the same release with a
# changed configuration, increase KERNEL_REVISION and set BUILD_TIMESTAMP.

ARG BUILDER=debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
ARG SNAPSHOT=20261115T000000Z
ARG KERNEL_VERSION=6.18.64
ARG KERNEL_SHA256=020d45510c539972e6f14c5c71566878d3b7ef56fb082b3e09134f2ebaade562
ARG KERNEL_REVISION=1
ARG BUILD_TIMESTAMP=2026-11-20T00:00:00Z

FROM ${BUILDER} AS deps
ARG SNAPSHOT
RUN set -eu; \
    rm -f /etc/apt/sources.list.d/*; \
    for suite in "debian trixie trixie-updates" "debian-security trixie-security"; do \
      set -- $suite; archive=$1; shift; \
      printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/%s/%s\nSuites: %s\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\n' \
        "$archive" "$SNAPSHOT" "$*" >> /etc/apt/sources.list.d/snapshot.sources; \
    done; \
    apt-get -o Acquire::Check-Valid-Until=false -o Acquire::Retries=5 update; \
    DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 install -y --no-install-recommends \
      bc bison ca-certificates curl flex gcc libc6-dev libelf-dev libssl-dev lz4 make xz-utils; \
    rm -rf /var/lib/apt/lists/*

FROM deps AS src
ARG KERNEL_VERSION
ARG KERNEL_SHA256
WORKDIR /build
RUN set -eu; \
    tarball=linux-$KERNEL_VERSION.tar.xz; \
    curl -fsSL --retry 5 -o "$tarball" "https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_VERSION%%.*}.x/$tarball"; \
    echo "$KERNEL_SHA256  $tarball" | sha256sum -c -; \
    tar -xJf "$tarball"; \
    mv "linux-$KERNEL_VERSION" linux

FROM src AS configured
COPY Dockerfile build.sh check-config.sh runwhp.config full.config /build/runwhp/
RUN sh /build/runwhp/build.sh configure

# The resolved configuration, for regenerating full.config.
FROM scratch AS config
COPY --from=configured /build/configured/full.config /

FROM configured AS built
ARG BUILDER
ARG SNAPSHOT
ARG KERNEL_VERSION
ARG KERNEL_SHA256
ARG KERNEL_REVISION
ARG BUILD_TIMESTAMP
RUN sh /build/runwhp/build.sh kernel

FROM scratch AS kernel
COPY --from=built /build/out/ /

# Everything published: the kernel, plus the corresponding source (the
# upstream tarball and these build files) that GPLv2 asks to go with it.
FROM built AS packaged
ARG KERNEL_VERSION
RUN set -eu; \
    mkdir /build/release; \
    cp /build/out/* /build/release/; \
    cp "/build/linux-$KERNEL_VERSION.tar.xz" /build/release/; \
    cd /build/runwhp; \
    chmod 0644 *; chmod 0755 *.sh; \
    tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --format=ustar \
      -cf /build/release/build.tar -- *

FROM scratch AS release
COPY --from=packaged /build/release/ /
