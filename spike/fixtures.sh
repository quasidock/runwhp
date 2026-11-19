#!/bin/sh
# Builds the disk fixtures for the runwhp boot spike. Run inside a WSL distro
# whose kernel matches the one being booted:
#
#   wsl -e sh /mnt/e/quasidock/spike//fixtures.sh /mnt/e/quasidock/out/spike
#
# Needs e2fsprogs. Expects the Linux init binary at $1/init. Produces:
#   virtio_blk.ko  the block driver, which the WSL kernel ships only as a module
#   layer.ext4     a small "image layer" holding /bin/hello and /etc/os-release
set -e
out=$1
cp "/lib/modules/$(uname -r)/kernel/drivers/block/virtio_blk.ko" "$out/"

stage=$(mktemp -d)
mkdir -p "$stage/bin" "$stage/etc" "$stage/data"
cp "$out/init" "$stage/bin/hello"
chmod 755 "$stage/bin/hello"
echo 'NAME="runwhp spike layer"' > "$stage/etc/os-release"
echo 'from the image layer' > "$stage/data/layer.txt"
rm -f "$out/layer.ext4"
mkfs.ext4 -q -F -d "$stage" -L layer "$out/layer.ext4" 16M
rm -rf "$stage"
