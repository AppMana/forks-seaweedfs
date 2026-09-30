package http

import (
	"io"
	"net/http"
	"testing"
)

// countingBody is an endless (or bounded) response body that records how much
// was read and whether it was closed.
type countingBody struct {
	remaining int64 // -1: endless
	read      int64
	closed    bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if b.remaining > 0 && n > b.remaining {
		n = b.remaining
	}
	if b.remaining > 0 {
		b.remaining -= n
	}
	b.read += n
	return int(n), nil
}

func (b *countingBody) Close() error {
	b.closed = true
	return nil
}

// A short unread remainder is drained, so the connection can be reused.
func TestCloseResponseDrainsShortRemainder(t *testing.T) {
	body := &countingBody{remaining: 100}
	CloseResponse(&http.Response{Body: body})
	if body.read != 100 || body.remaining != 0 {
		t.Fatalf("read %d, remaining %d: a short remainder must be drained", body.read, body.remaining)
	}
	if !body.closed {
		t.Fatal("body not closed")
	}
}

// A long unread remainder (the rest of an abandoned chunk) is not pulled from
// the server: at most the drain bound is read before closing.
func TestCloseResponseAbandonsLongRemainder(t *testing.T) {
	body := &countingBody{remaining: 8 << 20}
	CloseResponse(&http.Response{Body: body})
	if body.read > maxResponseDrain+1 {
		t.Fatalf("read %d bytes of an 8 MiB remainder, want at most %d", body.read, maxResponseDrain+1)
	}
	if !body.closed {
		t.Fatal("body not closed")
	}

	endless := &countingBody{remaining: -1}
	CloseResponse(&http.Response{Body: endless})
	if endless.read > maxResponseDrain+1 || !endless.closed {
		t.Fatalf("endless body: read %d closed %v", endless.read, endless.closed)
	}
}

func TestCloseResponseNilSafe(t *testing.T) {
	CloseResponse(nil)
	CloseResponse(&http.Response{})
}
