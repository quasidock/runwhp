#!/bin/sh
# Copies the kernel modules runwhp needs when it boots the WSL kernel out of
# the running WSL distro, into runwhp's module store. Run once per WSL kernel
# version, from PowerShell in a runwhp checkout:
#
#   wsl -e sh hack/wsl-modules.sh "$env:LOCALAPPDATA\runwhp\modules"
#
# The WSL kernel builds virtio_blk as a module; everything else runwhp uses is
# built in. The distro's kernel must be the one runwhp boots, which holds
# unless .wslconfig names a custom kernel.
set -e
if [ $# -ne 1 ]; then
	echo "usage: $0 WINDOWS-PATH-TO-MODULE-STORE" >&2
	exit 2
fi
version=$(uname -r)
dest=$(wslpath -u "$1")/$version
src=/lib/modules/$version/kernel/drivers/block/virtio_blk.ko
if [ ! -f "$src" ]; then
	echo "$src not found; the WSL kernel modules are not available in this distro" >&2
	exit 1
fi
mkdir -p "$dest"
cp "$src" "$dest/"
printf 'copied virtio_blk.ko for %s to %s\n' "$version" "$dest"
