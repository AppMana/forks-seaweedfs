package mount

import (
	"sync/atomic"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// Lightweight per-op latency counters for the WinFsp adapter. Logged as
// deltas once a minute at -v=1 when any op fired; near-zero overhead
// (two atomics per op) so they stay on in production.
const (
	opStatfs = iota
	opGetattr
	opMkdir
	opRmdir
	opUnlink
	opRename
	opCreate
	opOpen
	opTruncate
	opRead
	opWrite
	opFlush
	opRelease
	opOpendir
	opReaddir
	opReleasedir
	opMax
)

var winfspOpNames = [opMax]string{
	"statfs", "getattr", "mkdir", "rmdir", "unlink", "rename", "create",
	"open", "truncate", "read", "write", "flush", "release", "opendir",
	"readdir", "releasedir",
}

type winfspOpStat struct {
	count   atomic.Int64
	totalUs atomic.Int64
}

var winfspStats [opMax]winfspOpStat

// track records latency for one adapter op: defer track(opRead)().
func track(op int) func() {
	t0 := time.Now()
	return func() {
		winfspStats[op].count.Add(1)
		winfspStats[op].totalUs.Add(time.Since(t0).Microseconds())
	}
}

// logWinfspStatsLoop logs per-minute op deltas. Started by NewWinFspHost.
func logWinfspStatsLoop() {
	var lastCount, lastUs [opMax]int64
	for range time.Tick(time.Minute) {
		line := ""
		for op := 0; op < opMax; op++ {
			c := winfspStats[op].count.Load()
			us := winfspStats[op].totalUs.Load()
			dc, dus := c-lastCount[op], us-lastUs[op]
			lastCount[op], lastUs[op] = c, us
			if dc == 0 {
				continue
			}
			line += " " + winfspOpNames[op] + "=" + itoa(dc) + "/" + itoa(dus/dc) + "us"
		}
		if line != "" {
			glog.V(1).Infof("winfsp ops (1m):%s", line)
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
