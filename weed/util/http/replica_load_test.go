package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func forgetReplicaLoad(t *testing.T) {
	t.Helper()
	forget := func() {
		replicaLoads.Range(func(host, _ any) bool {
			replicaLoads.Delete(host)
			return true
		})
	}
	forget()
	t.Cleanup(forget)
}

// A replica whose link is saturated answers an 8 MiB chunk in seconds while
// its peers answer in milliseconds; reads must go to the peers.
func TestReachableFirstPrefersTheFasterReplica(t *testing.T) {
	forgetUnreachable(t)
	forgetReplicaLoad(t)
	urls := []string{"http://slow:8080/3,x", "http://a:8080/3,x", "http://b:8080/3,x"}
	recordReadCost("slow:8080", 2*time.Second, 8<<20)
	recordReadCost("a:8080", 80*time.Millisecond, 8<<20)
	recordReadCost("b:8080", 90*time.Millisecond, 8<<20)
	assertOrder(t, ReachableFirst(urls), urls[1], urls[2], urls[0])
}

// Equal replicas are spread by the reads already in flight to each.
func TestReachableFirstSpreadsReadsInFlight(t *testing.T) {
	forgetUnreachable(t)
	forgetReplicaLoad(t)
	urls := []string{"http://a:8080/3,x", "http://b:8080/3,x"}
	recordReadCost("a:8080", 50*time.Millisecond, 8<<20)
	recordReadCost("b:8080", 50*time.Millisecond, 8<<20)
	var done []func(int, error)
	for i := 0; i < 3; i++ {
		done = append(done, beginReplicaRead(context.Background(), "a:8080"))
	}
	assertOrder(t, ReachableFirst(urls), urls[1], urls[0])
	for _, d := range done {
		d(8<<20, nil)
	}
}

// A replica measured slow long ago is tried again once its cost has faded,
// so one bad minute does not exile it.
func TestSlowReplicaCostFades(t *testing.T) {
	forgetUnreachable(t)
	forgetReplicaLoad(t)
	urls := []string{"http://a:8080/3,x", "http://slow:8080/3,x"}
	recordReadCost("a:8080", 50*time.Millisecond, 8<<20)
	recordReadCost("slow:8080", 2*time.Second, 8<<20)
	assertOrder(t, ReachableFirst(urls), urls...)
	load, _ := replicaLoads.Load("slow:8080")
	load.(*replicaLoad).at = time.Now().Add(-10 * time.Minute)
	assertOrder(t, ReachableFirst(urls), urls[1], urls[0])
}

// Unreachable hosts stay last whatever their measured cost.
func TestReachableFirstKeepsUnansweringHostLastOverLoad(t *testing.T) {
	forgetUnreachable(t)
	forgetReplicaLoad(t)
	urls := []string{"http://a:8080/3,x", "http://b:8080/3,x"}
	recordReadCost("a:8080", time.Millisecond, 8<<20)
	recordReadCost("b:8080", time.Second, 8<<20)
	recordUnreachable("a:8080")
	assertOrder(t, ReachableFirst(urls), urls[1], urls[0])
}

// End to end: of three replicas, the one that takes 200 ms per chunk is asked
// for only a handful of 40 reads, whichever order the lookup returned.
func TestRetriedFetchChunkDataAvoidsTheSlowReplica(t *testing.T) {
	forgetUnreachable(t)
	forgetReplicaLoad(t)
	payload := make([]byte, 1<<20)
	serve := func(delay time.Duration, hits *int32) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(hits, 1)
			time.Sleep(delay)
			w.Write(payload)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	var slowHits, fastHits int32
	slow := serve(200*time.Millisecond, &slowHits)
	a, b := serve(0, &fastHits), serve(0, &fastHits)
	orders := [][]string{
		{slow.URL + "/3,abc", a.URL + "/3,abc", b.URL + "/3,abc"},
		{a.URL + "/3,abc", slow.URL + "/3,abc", b.URL + "/3,abc"},
		{b.URL + "/3,abc", a.URL + "/3,abc", slow.URL + "/3,abc"},
	}
	for i := 0; i < 40; i++ {
		buffer := make([]byte, len(payload))
		if _, err := RetriedFetchChunkData(context.Background(), buffer, orders[i%3], nil, false, true, 0, "3,abc", nil); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&slowHits); got > 3 {
		t.Fatalf("slow replica served %d of 40 reads, want at most 3", got)
	}
}

// A failed or zero-byte read (e.g. HTTP 404/500 or early EOF) returns in sub-ms
// with 0 bytes and must not record a near-zero per-MiB cost that makes the
// broken replica look faster than healthy peers.
func TestFailedReplicaReadDoesNotRecordFastCost(t *testing.T) {
	forgetUnreachable(t)
	forgetReplicaLoad(t)
	urls := []string{"http://failing:8080/3,x", "http://good:8080/3,x"}
	recordReadCost("failing:8080", 100*time.Millisecond, 1<<20)
	recordReadCost("good:8080", 50*time.Millisecond, 1<<20)

	for i := 0; i < 3; i++ {
		done := beginReplicaRead(context.Background(), "failing:8080")
		done(0, errors.New("500 internal server error"))
	}

	assertOrder(t, ReachableFirst(urls), urls[1], urls[0])
}

// An unmeasured replica should receive one initial probe (when inflight == 0),
// but while that first probe is still in flight (costNs == 0 && inflight > 0),
// concurrent reads must not herd onto the unmeasured replica ahead of a healthy
// measured peer.
func TestUnmeasuredReplicaWithProbeInFlightDoesNotHerd(t *testing.T) {
	forgetUnreachable(t)
	forgetReplicaLoad(t)
	urls := []string{"http://measured:8080/3,x", "http://unmeasured:8080/3,x"}
	recordReadCost("measured:8080", 40*time.Millisecond, 1<<20)

	// With 0 reads in flight, the unmeasured host is probed first.
	assertOrder(t, ReachableFirst(urls), urls[1], urls[0])

	// While that first probe is in flight on unmeasured:8080, concurrent reads
	// must prefer the known-healthy measured replica instead of piling 39 reads
	// onto the unmeasured replica before its first measurement completes.
	probeDone := beginReplicaRead(context.Background(), "unmeasured:8080")
	defer probeDone(1<<20, nil)

	assertOrder(t, ReachableFirst(urls), urls[0], urls[1])
}
