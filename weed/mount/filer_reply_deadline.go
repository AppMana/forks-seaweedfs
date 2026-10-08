package mount

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

// filerReplyTimeout bounds how long a FUSE request waits on the filer. The
// kernel holds the caller's directory lock (and, for reads, its pages) until
// we reply, so a stalled filer must become an error, not an indefinite D-state
// wait. A variable so tests can shorten it.
var filerReplyTimeout = 60 * time.Second

// errFilerTimeout reports a filer call this mount stopped waiting for. The
// call itself is not cancelled: a mutation may still complete on the filer,
// and its event then reaches this mount through the metadata subscription.
var errFilerTimeout = errors.New("filer did not reply in time")

// awaitFilerReply runs call detached and returns its result, or
// errFilerTimeout once ctx ends. Filer mutations are not cancelled
// mid-flight: a cancelled create or rename can leave the filer with half of
// it. The abandoned call finishes on its own goroutine.
func awaitFilerReply[T any](ctx context.Context, what string, call func() (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := call()
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("%s: %w (%v)", what, errFilerTimeout, ctx.Err())
	}
}

// asFilerTimeout names a deadline this mount set as a filer timeout.
func asFilerTimeout(what string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errFilerTimeout) {
		return fmt.Errorf("%s: %w (%v)", what, errFilerTimeout, err)
	}
	return err
}

// filerReplyContext bounds a wait that stops without cancelling filer work
// (the mutation stream's waits) or a read that is safe to cancel.
func filerReplyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, filerReplyTimeout)
}

// waitConnReady waits until conn can carry an RPC or ctx ends. Opening a
// stream on a connection still dialing blocks without any deadline.
func waitConnReady(ctx context.Context, conn *grpc.ClientConn) error {
	conn.Connect()
	for {
		state := conn.GetState()
		switch state {
		case connectivity.Ready:
			return nil
		case connectivity.Shutdown:
			return errors.New("connection shut down")
		}
		if !conn.WaitForStateChange(ctx, state) {
			return fmt.Errorf("connection %s: %w", state, ctx.Err())
		}
	}
}
