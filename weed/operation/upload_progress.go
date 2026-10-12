package operation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptrace"
	"sync"
	"time"
)

// ErrUploadStalled is the cause of a write abandoned by a ProgressWatch.
var ErrUploadStalled = errors.New("upload stalled")

// progressPollInterval is how often a ProgressWatch samples the bytes the
// peer acknowledged and checks its deadline.
var progressPollInterval = time.Second

// ProgressWatch abandons a write to a volume server that stops making
// progress, and only then. While the request body is being sent, the write
// is cancelled once no byte has moved for the stall timeout; after the body
// is sent, once nothing has moved for the response timeout and no response
// has arrived. Bytes the peer acknowledges on the connection count as moving
// in both phases, since the body can sit in the kernel's send buffer long
// after the client handed it over. A write that is slow but moving is never
// cut off, however long it takes: on a saturated 1 GbE link a healthy chunk
// upload can take tens of seconds, and an absolute bound abandoned and resent
// such uploads until they failed.
type ProgressWatch struct {
	stall    time.Duration
	response time.Duration
	cancel   context.CancelCauseFunc
	done     chan struct{}

	mu       sync.Mutex
	phase    string
	stopped  bool
	conn     net.Conn
	acked    uint64
	lastMove time.Time
}

// NewProgressWatch returns a context that ProgressWatch cancels with
// ErrUploadStalled, and the watch. The stall timeout runs from now. Stop must
// be called when the write is over.
func NewProgressWatch(parent context.Context, stall, response time.Duration) (context.Context, *ProgressWatch) {
	ctx, cancel := context.WithCancelCause(parent)
	w := &ProgressWatch{stall: stall, response: response, cancel: cancel, done: make(chan struct{}), phase: "sending", lastMove: time.Now()}
	go w.watch()
	return ctx, w
}

func (w *ProgressWatch) watch() {
	ticker := time.NewTicker(min(progressPollInterval, w.stall, w.response))
	defer ticker.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			if w.check() {
				return
			}
		}
	}
}

// check samples the acknowledged bytes and cancels the write once nothing
// has moved for the phase's limit. It reports whether the watch is over.
func (w *ProgressWatch) check() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return true
	}
	now := time.Now()
	if w.conn != nil {
		if acked, ok := tcpBytesAcked(w.conn); ok && acked != w.acked {
			w.acked = acked
			w.lastMove = now
		}
	}
	limit := w.stall
	if w.phase == "awaiting response" {
		limit = w.response
	}
	if now.Sub(w.lastMove) < limit {
		return false
	}
	w.stopped = true
	w.cancel(fmt.Errorf("%w: %s, nothing moved for %v", ErrUploadStalled, w.phase, limit))
	return true
}

// Trace returns ctx carrying an httptrace that hands the watch the
// connection a request runs on, so acknowledged bytes count as progress.
func (w *ProgressWatch) Trace(ctx context.Context) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			acked, _ := tcpBytesAcked(info.Conn)
			w.mu.Lock()
			w.conn, w.acked = info.Conn, acked
			w.mu.Unlock()
		},
	})
}

func (w *ProgressWatch) moved(phase string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.phase = phase
	w.lastMove = time.Now()
}

// Progress records that bytes moved.
func (w *ProgressWatch) Progress() { w.moved("sending") }

// BodySent records that the whole request body is handed over; the response
// timeout runs from here.
func (w *ProgressWatch) BodySent() { w.moved("awaiting response") }

// Responded records that the response arrived; nothing more is timed.
func (w *ProgressWatch) Responded() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
}

// Stop ends the watch; the write it guarded is over.
func (w *ProgressWatch) Stop() {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
	select {
	case <-w.done:
	default:
		close(w.done)
	}
	w.cancel(nil)
}

// Body wraps a request body so reads count as progress and its end starts
// the response timeout.
func (w *ProgressWatch) Body(r io.Reader) io.Reader {
	return &watchedBody{r: r, w: w}
}

type watchedBody struct {
	r io.Reader
	w *ProgressWatch
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.w.Progress()
	}
	if err == io.EOF {
		b.w.BodySent()
	}
	return n, err
}

// StalledCause returns the ProgressWatch cause when ctx was cancelled by one.
func StalledCause(ctx context.Context) error {
	if cause := context.Cause(ctx); errors.Is(cause, ErrUploadStalled) {
		return cause
	}
	return nil
}
