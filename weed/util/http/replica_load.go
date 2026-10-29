package http

import (
	"context"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Replica selection by load. Every replica of a chunk holds the same bytes, so
// a read should go to the one answering fastest: a volume server behind a
// saturated link takes seconds per chunk while its peers take milliseconds,
// and a random pick sends a third of the reads to it.

const (
	// replicaCostHalfLife fades a measurement, so a replica that was slow is
	// tried again once its peers' recent costs are higher than its faded one.
	replicaCostHalfLife = 30 * time.Second
	// replicaCostWeight is how much one read moves the average.
	replicaCostWeight = 0.3
	// replicaCostFloor stands in for an unmeasured replica and keeps the
	// in-flight factor meaningful for very cheap ones.
	replicaCostFloor = float64(time.Millisecond)
)

type replicaLoad struct {
	mu       sync.Mutex
	costNs   float64 // per MiB read, moving average
	at       time.Time
	inflight atomic.Int64
}

// replicaLoads maps a host to its *replicaLoad. Like unreachable, it is kept
// per host: one saturated link slows every volume on it.
var replicaLoads sync.Map

func loadOf(host string) *replicaLoad {
	if l, ok := replicaLoads.Load(host); ok {
		return l.(*replicaLoad)
	}
	l, _ := replicaLoads.LoadOrStore(host, &replicaLoad{})
	return l.(*replicaLoad)
}

// recordReadCost folds one read of bytes that took elapsed into host's cost.
func recordReadCost(host string, elapsed time.Duration, bytes int) {
	mib := math.Max(1, float64(bytes)/(1<<20))
	cost := float64(elapsed) / mib
	l := loadOf(host)
	l.mu.Lock()
	if l.at.IsZero() {
		l.costNs = cost
	} else {
		l.costNs = (1-replicaCostWeight)*l.fadedLocked(time.Now()) + replicaCostWeight*cost
	}
	l.at = time.Now()
	l.mu.Unlock()
}

func (l *replicaLoad) fadedLocked(now time.Time) float64 {
	if l.at.IsZero() {
		return 0
	}
	age := now.Sub(l.at)
	return l.costNs * math.Exp2(-float64(age)/float64(replicaCostHalfLife))
}

// beginReplicaRead counts a read to host as in flight; the returned function
// ends it and records its cost. A read the caller cancelled says nothing about
// the replica and is not recorded.
func beginReplicaRead(ctx context.Context, host string) func(bytes int, err error) {
	l := loadOf(host)
	l.inflight.Add(1)
	start := time.Now()
	return func(bytes int, err error) {
		l.inflight.Add(-1)
		if ctx.Err() != nil {
			return
		}
		recordReadCost(host, time.Since(start), bytes)
	}
}

// replicaScore is the expected cost of sending one more read to host.
func replicaScore(host string, now time.Time) float64 {
	v, ok := replicaLoads.Load(host)
	if !ok {
		return replicaCostFloor
	}
	l := v.(*replicaLoad)
	l.mu.Lock()
	cost := l.fadedLocked(now)
	l.mu.Unlock()
	return (math.Max(cost, replicaCostFloor)) * float64(1+l.inflight.Load())
}

// byReplicaLoad orders urls cheapest first; equal scores keep their order,
// which is the lookup's own shuffle.
func byReplicaLoad(urls []string) []string {
	if len(urls) < 2 {
		return urls
	}
	now := time.Now()
	scores := make(map[string]float64, len(urls))
	for _, u := range urls {
		host := hostOf(u)
		if _, ok := scores[host]; !ok {
			scores[host] = replicaScore(host, now)
		}
	}
	sorted := append([]string(nil), urls...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return scores[hostOf(sorted[i])] < scores[hostOf(sorted[j])]
	})
	return sorted
}
