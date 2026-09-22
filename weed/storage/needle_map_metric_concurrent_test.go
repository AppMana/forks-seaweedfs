package storage

import (
	"sync"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	. "github.com/seaweedfs/seaweedfs/weed/storage/types"
)

func TestNeedleMapMetricConcurrentMaxima(t *testing.T) {
	var mm mapMetric
	var wg sync.WaitGroup
	const workers = 16
	const iterations = 1000
	start := make(chan struct{})
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for i := iterations; i > 0; i-- {
				value := uint64(worker*iterations + i)
				mm.MaybeSetMaxFileKey(Uint64ToNeedleId(value))
				mm.MaybeSetMaxNeedleEnd(ToOffset(int64(value*8)), Size(16), needle.GetCurrentVersion())
				_ = mm.MaxFileKey()
				_ = mm.MaxNeedleEnd()
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	if got := mm.MaxFileKey(); got != Uint64ToNeedleId(workers*iterations) {
		t.Fatalf("max key regressed: %d", got)
	}
	wantEnd := int64(workers*iterations*8) + needle.GetActualSize(Size(16), needle.GetCurrentVersion())
	if got := mm.MaxNeedleEnd(); got != wantEnd {
		t.Fatalf("max end regressed: got %d, want %d", got, wantEnd)
	}
}
