package s3api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

func TestCopyChunksConcurrentlyReturnsChunksInOrder(t *testing.T) {
	got, err := copyChunksConcurrently(context.Background(), 10, func(ctx context.Context, i int) (*filer_pb.FileChunk, error) {
		return &filer_pb.FileChunk{Offset: int64(i)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range got {
		if c.Offset != int64(i) {
			t.Fatalf("chunk %d has offset %d", i, c.Offset)
		}
	}
}

func TestCopyChunksConcurrentlyWaitsForCancelledSiblingCleanup(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	boom := errors.New("copy failed")
	go func() {
		_, err := copyChunksConcurrently(context.Background(), 2, func(ctx context.Context, i int) (*filer_pb.FileChunk, error) {
			if i == 0 {
				<-started
				return nil, boom
			}
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-release // Model cleanup of an in-flight HTTP copy.
			return nil, ctx.Err()
		})
		done <- err
	}()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("sibling was not cancelled")
	}
	select {
	case err := <-done:
		close(release)
		t.Fatalf("copy returned before cancelled sibling finished cleanup: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("lost original error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("copy failed to return after sibling cleanup")
	}
}

// One failed chunk loses the whole copy, so the chunk copies still running
// must be cancelled and the ones not started must not run at all.
func TestCopyChunksConcurrentlyCancelsSiblingsOnFirstError(t *testing.T) {
	const count = 20
	var started, sawCancel int32
	boom := errors.New("destination volume timed out")

	_, err := copyChunksConcurrently(context.Background(), count, func(ctx context.Context, i int) (*filer_pb.FileChunk, error) {
		atomic.AddInt32(&started, 1)
		if i == 0 {
			return nil, boom
		}
		select {
		case <-ctx.Done():
			atomic.AddInt32(&sawCancel, 1)
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return &filer_pb.FileChunk{}, nil
		}
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the first chunk's error", err)
	}
	if n := atomic.LoadInt32(&started); n > chunkCopyConcurrency {
		t.Fatalf("%d chunk copies started, want at most %d: later chunks must be skipped once the copy failed", n, chunkCopyConcurrency)
	}
	if got := atomic.LoadInt32(&sawCancel); got != atomic.LoadInt32(&started)-1 {
		t.Fatalf("%d of %d running siblings saw the cancellation", got, atomic.LoadInt32(&started)-1)
	}
}

// A request that is already gone copies nothing.
func TestCopyChunksConcurrentlyHonorsCancelledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var started int32
	_, err := copyChunksConcurrently(ctx, 8, func(ctx context.Context, i int) (*filer_pb.FileChunk, error) {
		atomic.AddInt32(&started, 1)
		return &filer_pb.FileChunk{}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if started != 0 {
		t.Fatalf("%d chunk copies ran for a cancelled request", started)
	}
}

// A request cancelled mid-copy reaches the chunk copies that are running.
func TestCopyChunksConcurrentlyPropagatesRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan struct{}, chunkCopyConcurrency)
	go func() {
		<-running
		cancel()
	}()
	_, err := copyChunksConcurrently(ctx, 4, func(ctx context.Context, i int) (*filer_pb.FileChunk, error) {
		running <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return nil, errors.New("chunk copy was not cancelled")
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
