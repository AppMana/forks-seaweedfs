#!/bin/bash
set -euo pipefail
export PATH=/usr/sbin:/usr/bin:/sbin:/bin

image=$1
filesystem=$2
tests=$3
run_uid=$4
run_gid=$5
test_regex=$6
expected_size=$7
mode=$8

case "$filesystem" in xfs|btrfs) ;; *) echo "unsupported filesystem" >&2; exit 2 ;; esac
case "$mode" in normal|enospc) ;; *) echo "unsupported mode" >&2; exit 2 ;; esac
case "$image" in /tmp/seaweedfs-fs-lab-*/disk.img) ;; *) echo "unsafe image path" >&2; exit 2 ;; esac
[[ -f "$image" && ! -L "$image" ]] || { echo "image must be a regular non-symlink" >&2; exit 2; }
[[ $(stat -c %u "$image") = "$run_uid" && $(stat -c %h "$image") = 1 ]] || {
  echo "image ownership/link validation failed" >&2; exit 2
}
[[ "$expected_size" = 2147483648 || "$expected_size" = 8589934592 ]] || {
  echo "unsafe expected image size" >&2; exit 2
}
[[ $(stat -c %s "$image") = "$expected_size" ]] || { echo "unexpected image size" >&2; exit 2; }
[[ -f "$tests" && ! -L "$tests" ]] || { echo "tests must be a regular non-symlink" >&2; exit 2; }

mountpoint=/mnt/seaweedfs-storage-lab
mkdir -p "$mountpoint"
loop=
mounted=0
cleanup() {
  set +e
  if [[ $mounted = 1 ]]; then umount "$mountpoint"; fi
  if [[ -n "$loop" ]]; then losetup -d "$loop"; fi
}
trap cleanup EXIT INT TERM

loop=$(losetup --find --show --nooverlap "$image")
[[ "$loop" =~ ^/dev/loop[0-9]+$ ]] || { echo "unexpected loop device: $loop" >&2; exit 2; }
losetup -j "$image" | grep -Fq "$loop:" || { echo "loop backing-file validation failed" >&2; exit 2; }

if [[ $filesystem = xfs ]]; then
  mkfs.xfs -f -q "$loop"
  mount -t xfs -o noatime,inode64,logbufs=8 "$loop" "$mountpoint"
else
  mkfs.btrfs -f -q "$loop"
  mount -t btrfs -o noatime,space_cache=v2 "$loop" "$mountpoint"
fi
mounted=1
chown "$run_uid:$run_gid" "$mountpoint"
# bwrap's user namespace may map the requested uid rather than preserve the
# outer numeric owner. This is a private mount, so use normal /tmp semantics.
chmod 1777 "$mountpoint"

# The test sees only its real filesystem as /tmp, a synthetic /dev, read-only
# binaries, and loopback. It cannot see the loop device, image, host root or home.
bwrap --unshare-all --die-with-parent --new-session \
  --uid "$run_uid" --gid "$run_gid" --cap-drop ALL --clearenv \
  --ro-bind /usr /usr --symlink usr/bin /bin --symlink usr/lib /lib \
  --symlink usr/lib64 /lib64 --proc /proc --dev /dev \
  --bind "$mountpoint" /tmp --size 1048576 --tmpfs /artifacts \
  --ro-bind "$tests" /artifacts/tests \
  --remount-ro /artifacts --chdir /tmp \
  --setenv PATH /usr/bin:/bin --setenv HOME /tmp --setenv TMPDIR /tmp \
  --setenv GOMAXPROCS 2 --setenv EXPECT_FILESYSTEM "$filesystem" \
  --setenv SEAWEEDFS_TEST_DISPOSABLE_FS "$([[ $mode = enospc ]] && echo 1 || echo 0)" \
  /bin/sh -ec '
    actual=$(/usr/bin/stat -f -c %T /tmp)
    [ "$actual" = "$EXPECT_FILESYSTEM" ] || {
      echo "filesystem probe: got $actual want $EXPECT_FILESYSTEM" >&2; exit 1;
    }
    /usr/bin/python3 -c "
import os
assert os.getuid() != 0
with open(\"/proc/self/status\") as stream:
    caps = next(line.split()[1] for line in stream if line.startswith(\"CapEff:\"))
assert int(caps, 16) == 0
with open(\"/proc/net/dev\") as stream:
    interfaces = {line.split(\":\")[0].strip() for line in stream if \":\" in line}
assert interfaces == {\"lo\"}
assert not os.path.exists(\"/home/administrator\")
assert not os.path.exists(\"/dev/loop0\")
"
    echo "PASS: native filesystem boundary probe ($actual)"
    exec /artifacts/tests -test.v -test.count=1 -test.timeout=10m -test.run "$1"
  ' storage-lab "$test_regex"

sync
umount "$mountpoint"
mounted=0
if [[ $filesystem = xfs ]]; then
  xfs_repair -n "$loop"
else
  btrfs check --readonly "$loop"
fi
