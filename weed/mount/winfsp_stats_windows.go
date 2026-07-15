package mount

import (
	"os"
	"strings"
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

// writeWinfspStatsTrace writes one end-of-mount summary when explicitly
// requested by the test harness. The counters are already maintained for the
// production latency log, so enabling this adds no work to filesystem calls.
func writeWinfspStatsTrace() {
	path := os.Getenv("WEED_WINFSP_TRACE_SUMMARY")
	if path == "" {
		return
	}
	var b strings.Builder
	for op := 0; op < opMax; op++ {
		count := winfspStats[op].count.Load()
		if count == 0 {
			continue
		}
		totalUs := winfspStats[op].totalUs.Load()
		b.WriteString(winfspOpNames[op])
		b.WriteByte(' ')
		b.WriteString(itoa(count))
		b.WriteByte(' ')
		b.WriteString(itoa(totalUs))
		b.WriteByte(' ')
		b.WriteString(itoa(totalUs / count))
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		glog.Errorf("write WinFsp operation summary %s: %v", path, err)
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
