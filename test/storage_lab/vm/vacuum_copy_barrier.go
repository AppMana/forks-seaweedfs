package main

// vacuumCopyBarrier runs on the victim as
// python3 - <volume dir> <volume server pid file> <marker> <timeout seconds>.
// It watches for a compaction copy (*.cpd), stops the volume server with
// SIGSTOP once every thread has stopped while the copy still exists, and
// only then writes the marker listing the frozen copies. If the copy was
// committed before the stop took hold, it resumes the server and keeps
// watching. It exits 1, leaving the server running, if no copy is frozen in
// time.
const vacuumCopyBarrier = `import glob, os, signal, sys, time
directory, pid_file, marker, timeout = sys.argv[1], sys.argv[2], sys.argv[3], float(sys.argv[4])
with open(pid_file) as f:
    pid = int(f.read())
pattern = os.path.join(directory, '*.cpd')
deadline = time.monotonic() + timeout

def all_threads_stopped():
    for stat in glob.glob('/proc/%d/task/*/stat' % pid):
        try:
            with open(stat) as f:
                state = f.read().rsplit(')', 1)[1].split()[0]
        except FileNotFoundError:
            continue
        if state not in ('T', 't'):
            return False
    return True

while time.monotonic() < deadline:
    if glob.glob(pattern):
        os.kill(pid, signal.SIGSTOP)
        while not all_threads_stopped():
            if time.monotonic() >= deadline:
                os.kill(pid, signal.SIGCONT)
                sys.exit('volume server %d did not stop' % pid)
            time.sleep(0.0005)
        frozen = sorted(glob.glob(pattern))
        if frozen:
            with open(marker + '.tmp', 'w') as f:
                f.write('\n'.join(frozen) + '\n')
            os.rename(marker + '.tmp', marker)
            print('stopped volume server %d during %s' % (pid, ' '.join(frozen)), flush=True)
            sys.exit(0)
        os.kill(pid, signal.SIGCONT)
        print('copy committed before volume server %d stopped; resumed' % pid, flush=True)
    time.sleep(0.001)
sys.exit('no compaction copy appeared in %s within %gs' % (directory, timeout))
`
