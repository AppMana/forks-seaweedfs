package http

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// chunkServer serves payload for every GET. The first request is cut off after
// cutAt bytes (the connection is closed mid-body); later requests are served in
// full, honoring Range unless ignoreRange is set. It records each request's
// Range header.
func chunkServer(t *testing.T, payload []byte, cutAt int, ignoreRange bool) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		first := len(ranges) == 1
		mu.Unlock()

		if first {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload[:cutAt])
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		if ignoreRange {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			w.Write(payload)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(payload))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), ranges...)
	}
}

func testPayload(size int) []byte {
	p := make([]byte, size)
	for i := range p {
		p[i] = byte(i*7 + i/251)
	}
	return p
}

// A whole-chunk read cut off partway resumes from the bytes it already has
// instead of fetching the chunk again.
func TestRetriedFetchChunkDataResumesAfterPartialRead(t *testing.T) {
	const size, cut = 8 << 20, 5 << 20
	payload := testPayload(size)
	srv, ranges := chunkServer(t, payload, cut, false)

	buf := make([]byte, size)
	n, err := RetriedFetchChunkData(context.Background(), buf, []string{srv.URL + "/1,01"}, nil, false, true, 0, "1,01", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if n != size || !bytes.Equal(buf, payload) {
		t.Fatalf("got %d bytes, content equal=%v", n, bytes.Equal(buf, payload))
	}
	got := ranges()
	if len(got) != 2 || got[0] != "" {
		t.Fatalf("requests = %q, want a plain GET then one retry", got)
	}
	if want := "bytes=" + strconv.Itoa(cut) + "-" + strconv.Itoa(size-1); got[1] != want {
		t.Fatalf("retry asked for %q, want only the remainder %q", got[1], want)
	}
}

// A server that ignores Range and returns the whole object still yields the
// right bytes.
func TestRetriedFetchChunkDataResumeFallsBackToWholeObject(t *testing.T) {
	const size, cut = 1 << 20, 300 << 10
	payload := testPayload(size)
	srv, ranges := chunkServer(t, payload, cut, true)

	buf := make([]byte, size)
	n, err := RetriedFetchChunkData(context.Background(), buf, []string{srv.URL + "/1,02"}, nil, false, true, 0, "1,02", nil)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if n != size || !bytes.Equal(buf, payload) {
		t.Fatalf("got %d bytes, content equal=%v", n, bytes.Equal(buf, payload))
	}
	if got := ranges(); len(got) != 2 {
		t.Fatalf("requests = %q, want two", got)
	}
}

func TestContentRangeStart(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"bytes 5242880-8388607/8388608", 5242880, true},
		{"bytes 0-9/10", 0, true},
		{"", 0, false},
		{"bytes */10", 0, false},
	} {
		got, ok := contentRangeStart(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("contentRangeStart(%q) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// A mismatched partial response is not a whole-object fallback. Consuming it
// at offset zero poisons the prefix, which a later successful retry conceals.
func TestRetriedFetchChunkDataRejectsWrongPartialRange(t *testing.T) {
	payload := []byte("abcdef")
	first, _ := chunkServer(t, payload, 2, false)
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 3-5/6")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[3:])
	}))
	t.Cleanup(wrong.Close)
	last := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(payload))
	}))
	t.Cleanup(last.Close)
	buf := make([]byte, len(payload))
	n, err := RetriedFetchChunkData(context.Background(), buf,
		[]string{first.URL, wrong.URL, last.URL}, nil, false, true, 0, "range-repro", nil)
	if err != nil || n != len(payload) || !bytes.Equal(buf, payload) {
		t.Fatalf("retry must preserve the verified prefix: n=%d err=%v got=%q want=%q", n, err, buf, payload)
	}
}

func TestDirectResumeRejectsInvalidPartialBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		from         int
	}{
		{"wrong offset", "bytes 3-5/6", 2},
		{"malformed range", "bytes 2-not-a-range", 2},
		{"wrong total", "bytes 2-5/7", 2},
		{"unsolicited suffix", "bytes 3-5/6", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Range", tc.header)
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("XXXXXX"))
			}))
			defer srv.Close()
			buf := []byte("ab????")
			n, retry, err := readUrlDirectToBufferFrom(context.Background(), srv.URL, "", buf, tc.from)
			if err == nil || !retry || n != tc.from || string(buf) != "ab????" {
				t.Fatalf("invalid range consumed: n=%d retry=%v err=%v bytes=%q", n, retry, err, buf)
			}
		})
	}
}

func TestDirectResumePreservesVerifiedShortSubrange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 2-3/6")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("cd"))
	}))
	defer srv.Close()
	buf := []byte("ab????")
	n, retry, err := readUrlDirectToBufferFrom(context.Background(), srv.URL, "", buf, 2)
	if n != 4 || !retry || !errors.Is(err, io.ErrUnexpectedEOF) || string(buf) != "abcd??" {
		t.Fatalf("verified subrange lost: n=%d retry=%v err=%v bytes=%q", n, retry, err, buf)
	}
}
