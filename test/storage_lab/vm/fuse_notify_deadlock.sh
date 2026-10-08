#!/bin/sh
# Guest side of TestFuseReverseInvalidationLab. Runs as root inside a disposable
# VM: one weed server, a victim mount A and a mutator mount B of the same tree.
#
# Each iteration stalls one rename on A inside the kernel's directory lock (A's
# first mutation dials a new filer connection, and new connections are dropped),
# then makes B change names in that directory so A's metadata subscription
# invalidates them in A's kernel. The iteration records whether a thread of A
# sits in fuse_reverse_inval_entry (uninterruptible, waiting for the directory
# lock the stalled rename holds) and, in kill mode, whether SIGKILL can still
# end A: a thread in that wait keeps /dev/fuse open, so the connection is never
# aborted and the rename never returns.
#
# usage: fuse_notify_deadlock.sh <weed> <iterations> <wait|kill>
set -u
W=$1
ITERATIONS=$2
MODE=$3
FILER_GRPC=18888
LOG=/var/log/notify-lab
mkdir -p $LOG /srv/data /mnt/a /mnt/b
note() { echo "$(date +%T) $*" >>$LOG/progress.txt; }

sysctl -qw kernel.hung_task_panic=0
sysctl -qw kernel.hung_task_timeout_secs=10
sysctl -qw kernel.hung_task_warnings=-1
dmesg -C

drop_new() { iptables -I OUTPUT -o lo -p tcp --dport $FILER_GRPC --syn -j DROP; }
allow_new() { while iptables -D OUTPUT -o lo -p tcp --dport $FILER_GRPC --syn -j DROP 2>/dev/null; do :; done; }
allow_new

pkill -x weed; sleep 1
rm -rf /srv/data/*
$W server -dir=/srv/data -ip=127.0.0.1 -ip.bind=127.0.0.1 -filer -filer.port=8888 \
  -master.port=9333 -volume.port=8080 -volume.max=20 >$LOG/server.log 2>&1 &
SERVER=$!
for _ in $(seq 120); do
  curl -sf http://127.0.0.1:8888/ >/dev/null 2>&1 && break
  sleep 1
done

start_mount() { # name -> pid
  name=$1
  rm -rf /var/cache/notify-$name
  $W mount -filer=127.0.0.1:8888 -dir=/mnt/$name -filer.path=/ -cacheDir=/var/cache/notify-$name \
    -dirAutoCreate >>$LOG/mount-$name.log 2>&1 &
  pid=$!
  for _ in $(seq 60); do
    mountpoint -q /mnt/$name && break
    sleep 0.5
  done
  echo $pid
}

fuse_conn() { # mountpoint -> fuse connection id (device minor), read without touching the mount
  awk -v m="$1" '$5 == m && $9 ~ /^fuse/ { split($3, d, ":"); print d[2] }' /proc/self/mountinfo | tail -n 1
}

notify_blocked() { # pid -> number of threads waiting in fuse_reverse_inval_entry
  n=0
  for s in /proc/$1/task/*/stack; do
    grep -q fuse_reverse_inval_entry "$s" 2>/dev/null && n=$((n + 1))
  done
  echo $n
}

B=$(start_mount b)
echo "server and mount b ready (b=$B)"

i=0
while [ $i -lt "$ITERATIONS" ]; do
  i=$((i + 1))
  d=$MODE-$i
  A=$(start_mount a)
  conn=$(fuse_conn /mnt/a)
  # B mutates first so its own mutation stream is already connected.
  mkdir -p /mnt/b/$d && echo x >/mnt/b/$d/x
  for _ in $(seq 100); do test -e /mnt/a/$d/x && break; sleep 0.1; done
  ls /mnt/a/$d >/dev/null

  note "iteration $i: mount a=$A ready, stalling rename"
  drop_new
  start=$(date +%s.%N)
  mv /mnt/a/$d/x /mnt/a/$d/y &
  MV=$!
  sleep 1
  k=0
  while [ $k -lt 10 ]; do
    k=$((k + 1))
    echo $k >/mnt/b/$d/f$k
    sleep 0.3
  done
  sleep 2
  blocked=$(notify_blocked "$A")
  note "iteration $i: $blocked notifier threads in fuse_reverse_inval_entry"
  stuck=0
  if [ "$MODE" = kill ]; then
    kill -9 "$A"
    sleep 8
    if [ -d /proc/$A ]; then
      stuck=1
      for s in /proc/$A/task/*/stack; do
        if grep -q fuse_ "$s" 2>/dev/null; then echo "== $s"; cat "$s"; fi
      done >$LOG/stuck-$i.txt
      echo 1 >/sys/fs/fuse/connections/$conn/abort
    fi
    allow_new
    wait $MV 2>/dev/null
    rename_seconds=$(awk -v s="$start" -v e="$(date +%s.%N)" 'BEGIN { printf "%.1f", e - s }')
    wait "$A" 2>/dev/null
  else
    # The stalled rename does not finish while new filer connections fail
    # (neither the stream nor its unary fallback has a deadline). Hold the
    # stall past the guest's 10 s hung-task threshold, then let it complete.
    sleep 25
    stalled=$(notify_blocked "$A")
    test "$stalled" -gt "$blocked" && blocked=$stalled
    allow_new
    wait $MV
    rename_seconds=$(awk -v s="$start" -v e="$(date +%s.%N)" 'BEGIN { printf "%.1f", e - s }')
    for _ in $(seq 30); do test -e /mnt/a/$d/f10 && break; sleep 0.2; done
    test -e /mnt/a/$d/y && test -e /mnt/a/$d/f10 || echo "ITERATION $i: names not visible on A after the rename" >&2
    kill "$A"
    wait "$A" 2>/dev/null
  fi
  umount -l /mnt/a 2>/dev/null
  note "iteration $i: rename took $rename_seconds s, weed unkillable=$stuck"
  hung=$(dmesg | grep -c "task weed:.*blocked for more than")
  echo "RESULT iteration=$i mode=$MODE notify_blocked_threads=$blocked weed_unkillable=$stuck rename_seconds=$rename_seconds hung_task_reports=$hung"
done
dmesg | grep -A30 "blocked for more than" >$LOG/hung-task.txt
kill "$B"; wait "$B" 2>/dev/null; umount -l /mnt/b 2>/dev/null
kill "$SERVER"; wait "$SERVER" 2>/dev/null
echo NOTIFY_LAB_COMPLETE
