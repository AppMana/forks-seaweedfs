package operation

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// A volume server that accepts the connection but never reads the body or
// answers must not hold an upload forever: each attempt ends at the option's
// AttemptTimeout.
func TestUploadAttemptTimeoutBoundsAStalledVolumeServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var heldLock sync.Mutex
	var held []net.Conn
	defer func() {
		heldLock.Lock()
		defer heldLock.Unlock()
		for _, c := range held {
			c.Close()
		}
	}()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			heldLock.Lock()
			held = append(held, c)
			heldLock.Unlock()
		}
	}()

	uploader := newUploader(&http.Client{})
	option := &UploadOption{
		UploadUrl:      "http://" + listener.Addr().String() + "/3,01637037d6",
		MaxAttempts:    2,
		AttemptTimeout: 200 * time.Millisecond,
	}
	done := make(chan error, 1)
	go func() {
		_, err := uploader.UploadData(context.Background(), make([]byte, 8<<20), option)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("upload to a stalled volume server succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upload to a stalled volume server did not give up")
	}
}
