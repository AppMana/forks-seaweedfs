#!/bin/sh
# Guest side of TestFuseStuckBackendLab. Runs as root inside a disposable VM:
# one weed server and one mount with request deadlines scaled below the
# guest's hung-task timeout. The guest has 512 MiB, so the mount's read buffer
# budget and Go memory limit are lowered to fit beside the server.
#
# The guest panics on a hung task, as the cluster's nodes do. A FUSE request
# the mount never answers leaves the caller in uninterruptible sleep; a
# SIGKILLed writer's final FLUSH waits that way by design. If any such wait
# outlasts 10 s, the VM panics and the harness loses it.
#
# Phase "stuck": the server is frozen with SIGSTOP and the mount's cache
# directory is deleted, as on appmana-031. Every operation must return within
# 2 x request timeout + 1 s.
# Phase "close": loopback is throttled and a large file is written
# sequentially through a mount with a write buffer cap. close() must return
# within the cap's drain time, and the bytes must read back intact.
#
# usage: fuse_stuck_backend.sh <weed> <request-timeout-s> <kernel-timeout-s> [hung-task-panic]
# hung-task-panic defaults to 1; 0 reports hung tasks instead, to see which
# operation hangs when the gate fails.
set -u
W=$1
REQ=$2
KERNEL=$3
PANIC=${4:-1}
LOG=/var/log/stuck-lab
mkdir -p $LOG /srv/data /mnt/s
note() { echo "$(date +%T) $*" >>$LOG/progress.txt; }

sysctl -qw kernel.hung_task_timeout_secs=10
sysctl -qw kernel.hung_task_warnings=-1
sysctl -qw kernel.hung_task_panic=$PANIC
sysctl -qw kernel.panic=0
dmesg -C
echo "KERNEL $(uname -r) fuse_max_request_timeout=$(cat /proc/sys/fs/fuse/max_request_timeout 2>/dev/null || echo unsupported)"

pkill -x weed; sleep 1
rm -rf /srv/data/*
$W server -dir=/srv/data -ip=127.0.0.1 -ip.bind=127.0.0.1 -filer -filer.port=8888 \
  -master.port=9333 -volume.port=8080 -volume.max=20 >$LOG/server.log 2>&1 &
SERVER=$!
for _ in $(seq 120); do
  curl -sf http://127.0.0.1:8888/ >/dev/null 2>&1 && break
  sleep 1
done

# A weed without request deadlines (the pre-fix baseline) runs the same
# scenario with none, which is what the gate must catch.
DEADLINES=
if $W mount -h 2>&1 | grep -q fuse.requestTimeout; then
  DEADLINES="-fuse.requestTimeout=${REQ}s -fuse.kernelRequestTimeout=${KERNEL}s"
fi
echo "DEADLINES ${DEADLINES:-none}"

start_mount() { # extra mount options -> pid
  rm -rf /var/cache/stuck
  # shellcheck disable=SC2086
  $W mount -filer=127.0.0.1:8888 -dir=/mnt/s -filer.path=/ -cacheDir=/var/cache/stuck \
    -dirAutoCreate -chunkSizeLimitMB=2 -readerCacheSizeMB=64 -memoryLimitMB=200 $DEADLINES "$@" \
    >>$LOG/mount.log 2>&1 &
  pid=$!
  for _ in $(seq 60); do
    mountpoint -q /mnt/s && break
    sleep 0.5
  done
  echo $pid
}

elapsed() { awk -v s="$1" -v e="$(date +%s.%N)" 'BEGIN { printf "%.1f", e - s }'; }

timed() { # name command... : runs the command in the background, reports when it returns
  name=$1; shift
  start=$(date +%s.%N)
  "$@" >/dev/null 2>&1 &
  p=$!
  bound=$((2 * REQ + 1))
  t=0
  while [ -d /proc/$p ] && [ $t -lt $((bound * 10 + 50)) ]; do sleep 0.1; t=$((t + 1)); done
  if [ -d /proc/$p ]; then
    echo "RESULT phase=stuck op=$name seconds=$(elapsed "$start") returned=0"
    note "$name still running: $(cat /proc/$p/wchan 2>/dev/null)"
  else
    wait $p; rc=$?
    echo "RESULT phase=stuck op=$name seconds=$(elapsed "$start") returned=1 rc=$rc"
  fi
}

# Phase stuck.
M=$(start_mount)
head -c 8388608 /dev/urandom >/srv/payload
cp /srv/payload /mnt/s/written
# A writer with dirty data, to be killed while the backend is frozen.
sh -c 'exec 3>/mnt/s/killed; head -c 8388608 /dev/urandom >&3; echo ready; exec sleep 600' >$LOG/writer.out &
WRITER=$!
for _ in $(seq 100); do grep -q ready $LOG/writer.out && break; sleep 0.1; done
exec 4>/mnt/s/held
note "freezing server $SERVER, deleting cache dir"
kill -STOP $SERVER
rm -rf /var/cache/stuck

timed lookup stat /mnt/s/absent
timed mkdir mkdir /mnt/s/dir
timed read dd if=/mnt/s/written of=/dev/null bs=4096 count=1 iflag=direct
timed write_close sh -c 'head -c 8388608 /srv/payload >/mnt/s/new'
timed fsync sh -c 'head -c 8388608 /srv/payload | dd of=/mnt/s/synced conv=fsync bs=1M'
start=$(date +%s.%N)
kill -9 $WRITER
t=0
while [ -d /proc/$WRITER ] && [ $t -lt $(((2 * REQ + 1) * 10 + 50)) ]; do sleep 0.1; t=$((t + 1)); done
if [ -d /proc/$WRITER ]; then
  echo "RESULT phase=stuck op=killed_writer_exit seconds=$(elapsed "$start") returned=0"
else
  echo "RESULT phase=stuck op=killed_writer_exit seconds=$(elapsed "$start") returned=1 rc=137"
fi
exec 4>&-
kill -CONT $SERVER
kill $M; wait $M 2>/dev/null
umount -l /mnt/s 2>/dev/null

# Phase close: 16 MiB/s loopback, 32 MiB cap, 256 MiB file.
CAP=32
RATE=16
tc qdisc add dev lo root tbf rate ${RATE}mbps burst 256kb latency 400ms
M=$(start_mount -writeBufferSizeMB=$CAP)
head -c 268435456 /dev/urandom >/srv/big
python3 - /srv/big /mnt/s/checkpoint.pt <<'EOF'
import sys, time
src, dst = sys.argv[1], sys.argv[2]
# The guest has 512 MiB: stream the source rather than holding it.
s = open(src, 'rb')
f = open(dst, 'wb', buffering=0)
start = time.monotonic()
while True:
    piece = s.read(1 << 20)
    if not piece:
        break
    f.write(piece)
close_start = time.monotonic()
f.close()
end = time.monotonic()
print(f"RESULT phase=close write_seconds={close_start - start:.1f} close_seconds={end - close_start:.1f}")
EOF
tc qdisc del dev lo root
if cmp -s /srv/big /mnt/s/checkpoint.pt; then echo "RESULT phase=close intact=1"; else echo "RESULT phase=close intact=0"; fi
kill $M; wait $M 2>/dev/null
umount -l /mnt/s 2>/dev/null

hung=$(dmesg | grep -c "blocked for more than")
echo "RESULT phase=end hung_task_reports=$hung"
grep -E "unanswered after|replied EIO" $LOG/mount.log | tail -n 20 >$LOG/deadline-replies.txt
kill $SERVER; wait $SERVER 2>/dev/null
echo STUCK_LAB_COMPLETE
