package topology

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestVacuumOwnershipCancellationAndAutomaticCoalescing(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "busy"}[busy], func(t *testing.T) {
			topo := NewTopology("vacuum", nil, 32<<20, 5, false)
			var owner int64
			if busy {
				owner = 1
			}
			atomic.StoreInt64(&topo.vacuumLockCounter, owner)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := topo.VacuumWithContext(ctx, nil, .1, 1, 0, "", 0, false); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled request: %v", err)
			}
			if atomic.LoadInt64(&topo.vacuumLockCounter) != owner {
				t.Fatal("canceled request changed another vacuum's ownership")
			}
			ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := topo.VacuumWithContext(ctx, nil, .1, 1, 0, "", 0, true); err != nil {
				t.Fatalf("automatic sweep should not wait: %v", err)
			}
			if busy {
				if err := topo.VacuumWithContext(ctx, nil, .1, 1, 0, "", 0, false); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("busy manual request must honor deadline: %v", err)
				}
			}
			if atomic.LoadInt64(&topo.vacuumLockCounter) != owner {
				t.Fatal("sweep changed another vacuum's ownership")
			}
		})
	}
}

func TestManualVacuumDoesNotSilentlySkipBusyTopology(t *testing.T) {
	topo := NewTopology("vacuum", nil, 32<<20, 5, false)
	// Model the lock held by an automatic sweep. No disk or timing-dependent
	// volume RPC is required to reproduce the lost manual request.
	atomic.StoreInt64(&topo.vacuumLockCounter, 1)
	done := make(chan struct{})
	go func() {
		topo.Vacuum(nil, .1, 1, 0, "requested-collection", 0, false)
		close(done)
	}()
	select {
	case <-done:
		atomic.StoreInt64(&topo.vacuumLockCounter, 0)
		t.Fatal("manual vacuum returned without acquiring ownership or examining its collection")
	case <-time.After(100 * time.Millisecond):
	}
	atomic.StoreInt64(&topo.vacuumLockCounter, 0)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("manual vacuum did not resume after the running sweep finished")
	}
	if atomic.LoadInt64(&topo.vacuumLockCounter) != 0 {
		t.Fatal("vacuum ownership leaked")
	}
}
