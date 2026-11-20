#!/bin/sh
# Configures and builds runwhp's kernel inside the container that
# kernel/Dockerfile describes; it is not meant to run anywhere else.
#
#   build.sh configure   allnoconfig plus runwhp.config, checked; the result
#                        goes to /build/configured/full.config
#   build.sh kernel      checks the result against kernel/full.config and
#                        builds the bzImage into /build/out
set -eu

here=/build/runwhp
out=/build/out
export ARCH=x86_64 LC_ALL=C TZ=UTC
cd /build/linux
mkdir -p "$out"

case "${1:-}" in
configure)
	# Everything not named in runwhp.config is off, so a new upstream
	# option stays off until someone decides otherwise...
	make -s KCONFIG_ALLCONFIG="$here/runwhp.config" allnoconfig
	# ...except CPU vulnerability mitigations, which keep upstream's
	# defaults, including any that later releases add.
	sed -i '/^# CONFIG_MITIGATION_[A-Z0-9_]* is not set$/d' .config
	make -s olddefconfig
	sh "$here/check-config.sh" "$here/runwhp.config" .config
	mkdir -p /build/configured
	cp .config /build/configured/full.config
	;;
kernel)
	if ! diff -u "$here/full.config" .config >&2; then
		echo "build.sh: kernel/full.config does not match the configuration runwhp.config gives;" >&2
		echo "build.sh: regenerate it with kernel/build.ps1 -Target config and review the difference" >&2
		exit 1
	fi
	timestamp=$(date -u -d "$BUILD_TIMESTAMP" '+%Y-%m-%d %H:%M:%S UTC')
	set -- KBUILD_BUILD_TIMESTAMP="$timestamp" KBUILD_BUILD_USER=runwhp \
		KBUILD_BUILD_HOST=runwhp KBUILD_BUILD_VERSION="$KERNEL_REVISION"
	make -s -j"$(nproc)" "$@" bzImage
	release=$(make -s "$@" kernelrelease)
	cp arch/x86/boot/bzImage "$out/bzImage"
	cp .config "$out/config"
	cat >"$out/build-info.json" <<-EOF
	{
	  "version": "$KERNEL_VERSION",
	  "revision": "$KERNEL_REVISION",
	  "release": "$release",
	  "created": "$BUILD_TIMESTAMP",
	  "source": "https://cdn.kernel.org/pub/linux/kernel/v${KERNEL_VERSION%%.*}.x/linux-$KERNEL_VERSION.tar.xz",
	  "sourceSHA256": "$KERNEL_SHA256",
	  "builder": "$BUILDER",
	  "snapshot": "$SNAPSHOT"
	}
	EOF
	sha256sum "$out/bzImage"
	;;
*)
	echo "usage: build.sh configure|kernel" >&2
	exit 2
	;;
esac
