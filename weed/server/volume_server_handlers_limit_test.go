package weed_server

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestTryReserveDataSizeIsAtomic(t *testing.T) {
	const (
		limit    = int64(32)
		attempts = 256
	)

	var counter int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	var admitted int64
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if tryReserveDataSize(&counter, limit, 1) {
				atomic.AddInt64(&admitted, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&counter); got != limit {
		t.Fatalf("reserved %d bytes, want hard limit %d", got, limit)
	}
	if got := atomic.LoadInt64(&admitted); got != limit {
		t.Fatalf("admitted %d requests, want %d", got, limit)
	}
}

func TestTryReserveDataSizeAllowsOneOversizedRequest(t *testing.T) {
	var counter int64
	if !tryReserveDataSize(&counter, 100, 150) {
		t.Fatal("a single oversized request should be admitted to avoid deadlock")
	}
	if tryReserveDataSize(&counter, 100, 1) {
		t.Fatal("a second request was admitted while an oversized request was active")
	}
	if got := atomic.LoadInt64(&counter); got != 150 {
		t.Fatalf("reserved %d bytes, want 150", got)
	}
}
