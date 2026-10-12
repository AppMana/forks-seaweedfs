package operation

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// smallBufferListener gives every accepted connection a small receive
// buffer, so a client's upload is acknowledged only as fast as the server
// reads it, the way a saturated link drains a send buffer.
type smallBufferListener struct{ net.Listener }

func (l smallBufferListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetReadBuffer(32 << 10)
		}
	}
	return c, err
}

// slowVolumeServer reads every upload at rate bytes per second and then
// answers it.
func slowVolumeServer(t *testing.T, rate int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var posts atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		buf := make([]byte, rate/10)
		for {
			if _, err := io.ReadFull(r.Body, buf); err != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"name":"chunk","size":3}`)
	}))
	srv.Listener = smallBufferListener{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &posts
}

// An upload that is slow but keeps moving must not be abandoned, however
// long it takes past the stall timeout: on a saturated 1 GbE link a healthy
// chunk upload can take tens of seconds. An absolute per-attempt bound cut
// such uploads off and resent them until the write failed.
func TestSlowButProgressingUploadIsNotAbandoned(t *testing.T) {
	const (
		stall = 500 * time.Millisecond
		rate  = 1 << 20 // 4 MiB in about 4 s, eight stall timeouts
	)
	srv, posts := slowVolumeServer(t, rate)
	payload := make([]byte, 4<<20)
	rand.Read(payload)
	uploader := newUploader(&http.Client{})
	start := time.Now()
	_, err := uploader.UploadData(context.Background(), payload, &UploadOption{
		UploadUrl:       srv.URL + "/3,01637037d6",
		Filename:        "chunk",
		MaxAttempts:     1,
		StallTimeout:    stall,
		ResponseTimeout: stall,
	})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("slow but progressing upload abandoned after %v: %v", took, err)
	}
	if n := posts.Load(); n != 1 {
		t.Fatalf("upload posted %d times, want 1", n)
	}
	if took < 4*stall {
		t.Fatalf("upload took %v; the server should have needed several stall timeouts", took)
	}
	t.Logf("upload took %v with a %v stall timeout", took, stall)
}

// A server that accepts the connection and then reads nothing more must be
// abandoned about one stall timeout after it stops.
func TestUploadThatStopsMovingIsAbandoned(t *testing.T) {
	const stall = 500 * time.Millisecond
	stalled := startStalledVolumeServer(t)
	uploader := newUploader(&http.Client{})
	start := time.Now()
	payload := make([]byte, 16<<20)
	rand.Read(payload)
	_, err := uploader.UploadData(context.Background(), payload, &UploadOption{
		UploadUrl:       "http://" + stalled.addr() + "/3,01637037d6",
		MaxAttempts:     1,
		StallTimeout:    stall,
		ResponseTimeout: stall,
	})
	took := time.Since(start)
	if err == nil {
		t.Fatal("upload to a server that reads nothing succeeded")
	}
	if took > 4*stall {
		t.Fatalf("stopped upload abandoned after %v, want about %v", took, stall)
	}
	t.Logf("abandoned after %v: %v", took, err)
}

// The Synology volume server, measured 2026-10-12 after its SSD cache was
// rebuilt, sustains about 80 MB/s and answers 2 MiB uploads at 16 in flight
// with a p99 of 1.4 s and a maximum of 2.9 s. Such a server is slow but
// healthy: with the mount's timeouts every upload must land on the first
// attempt.
func TestSlowHealthyNasShapedServerIsNotAbandoned(t *testing.T) {
	const (
		concurrent = 16
		perUpload  = 5 << 20 // 80 MB/s shared by 16 uploads
		answerIn   = 3 * time.Second
	)
	var posts atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		buf := make([]byte, perUpload/10)
		for {
			if _, err := io.ReadFull(r.Body, buf); err != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(answerIn)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"name":"chunk","size":3}`)
	}))
	srv.Listener = smallBufferListener{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)

	uploader := newUploader(&http.Client{})
	errs := make(chan error, concurrent)
	for i := 0; i < concurrent; i++ {
		go func() {
			payload := make([]byte, 2<<20)
			rand.Read(payload)
			_, err := uploader.UploadData(context.Background(), payload, &UploadOption{
				UploadUrl:       srv.URL + "/3,01637037d6",
				Filename:        "chunk",
				MaxAttempts:     1,
				StallTimeout:    UploadStallTimeout,
				ResponseTimeout: MountUploadResponseTimeout,
			})
			errs <- err
		}()
	}
	for i := 0; i < concurrent; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("upload to a slow but healthy server abandoned: %v", err)
		}
	}
	if n := posts.Load(); n != concurrent {
		t.Fatalf("%d posts for %d uploads, want one each", n, concurrent)
	}
}
