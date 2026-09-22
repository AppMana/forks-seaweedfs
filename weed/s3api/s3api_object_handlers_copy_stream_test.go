package s3api

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

func TestStreamCopyChunkRangeHonorsAssignedDurability(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(strconv.FormatBool(durable), func(t *testing.T) {
			payload := []byte("durable streamed copy")
			src := newStreamSrc(t, payload, false, true)
			defer src.Close()
			requested := make(chan string, 1)
			dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requested <- r.URL.Query().Get("fsync")
				_, _ = io.Copy(io.Discard, r.Body)
				if durable {
					http.Error(w, "injected disk sync failure", http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer dst.Close()
			assignment := assignTo(t, dst)
			assignment.Fsync = durable
			err := (&S3ApiServer{}).streamCopyChunkRange(context.Background(), src.URL, "7,src", 0, int64(len(payload)), true, assignment)
			want := ""
			if durable {
				want = "true"
			}
			if got := <-requested; got != want {
				t.Errorf("destination fsync=%q, want %q", got, want)
			}
			if durable && (err == nil || !strings.Contains(err.Error(), "500")) {
				t.Fatalf("sync failure must fail the copy: %v", err)
			}
			if !durable && err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The destination POST of a streamed chunk copy must carry an exact
// Content-Length. A volume server that receives a chunked upload has no
// size to admit it by and reserves the per-request ceiling instead, so a
// handful of copies exhaust its whole concurrent-upload budget.
type streamDst struct {
	contentLength    int64
	transferEncoding string
	bodyLen          int64
	partSHA          []byte
	calls            int32
}

func newStreamDst(t *testing.T) (*httptest.Server, *streamDst) {
	t.Helper()
	rec := &streamDst{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&rec.calls, 1)
		rec.contentLength = r.ContentLength
		if len(r.TransferEncoding) > 0 {
			rec.transferEncoding = r.TransferEncoding[0]
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.bodyLen = int64(len(raw))
		req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
		req.Header = r.Header.Clone()
		mr, err := req.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		part, err := mr.NextPart()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.partSHA, err = io.ReadAll(part)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"size":1}`)
	}))
	return srv, rec
}

func newStreamSrc(t *testing.T, wire []byte, gzipped bool, withLength bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gzipped {
			w.Header().Set("Content-Encoding", "gzip")
		}
		if withLength {
			w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
		} else {
			// Force chunked framing on the source response.
			w.Header().Set("Transfer-Encoding", "chunked")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wire)
	}))
}

func assignTo(t *testing.T, srv *httptest.Server) *filer_pb.AssignVolumeResponse {
	t.Helper()
	host, err := neturl(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &filer_pb.AssignVolumeResponse{
		FileId:   "7,streamtest",
		Location: &filer_pb.Location{Url: host, PublicUrl: host},
	}
}

func TestStreamCopyChunkRange_DestinationPostCarriesContentLength(t *testing.T) {
	payload := bytes.Repeat([]byte("seaweed"), 4096)
	src := newStreamSrc(t, payload, false, true)
	defer src.Close()
	dst, rec := newStreamDst(t)
	defer dst.Close()

	s3a := &S3ApiServer{}
	err := s3a.streamCopyChunkRange(context.Background(), src.URL+"/7,src", "7,src",
		0, int64(len(payload)), true, assignTo(t, dst))
	if err != nil {
		t.Fatalf("stream copy: %v", err)
	}
	if !bytes.Equal(rec.partSHA, payload) {
		t.Fatalf("destination received %d bytes of part data, want %d", len(rec.partSHA), len(payload))
	}
	if rec.transferEncoding == "chunked" {
		t.Fatalf("destination POST was chunked; the volume server cannot size a chunked upload")
	}
	if rec.contentLength != rec.bodyLen {
		t.Fatalf("destination POST Content-Length %d, body was %d bytes", rec.contentLength, rec.bodyLen)
	}
}

func TestStreamCopyChunkRange_GzipPassThroughCarriesCompressedLength(t *testing.T) {
	var wire bytes.Buffer
	zw := gzip.NewWriter(&wire)
	_, _ = zw.Write(bytes.Repeat([]byte("compressible "), 8192))
	_ = zw.Close()
	src := newStreamSrc(t, wire.Bytes(), true, true)
	defer src.Close()
	dst, rec := newStreamDst(t)
	defer dst.Close()

	s3a := &S3ApiServer{}
	err := s3a.streamCopyChunkRange(context.Background(), src.URL+"/7,src", "7,src",
		0, int64(wire.Len()*4), true, assignTo(t, dst))
	if err != nil {
		t.Fatalf("stream copy: %v", err)
	}
	if !bytes.Equal(rec.partSHA, wire.Bytes()) {
		t.Fatalf("destination did not receive the compressed wire bytes verbatim")
	}
	if rec.contentLength != rec.bodyLen {
		t.Fatalf("destination POST Content-Length %d, body was %d bytes", rec.contentLength, rec.bodyLen)
	}
}

func TestStreamCopyChunkRange_UnsizedSourceStillCopies(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 100000)
	src := newStreamSrc(t, payload, false, false)
	defer src.Close()
	dst, rec := newStreamDst(t)
	defer dst.Close()

	s3a := &S3ApiServer{}
	err := s3a.streamCopyChunkRange(context.Background(), src.URL+"/7,src", "7,src",
		0, int64(len(payload)), false, assignTo(t, dst))
	if err != nil {
		t.Fatalf("stream copy: %v", err)
	}
	if !bytes.Equal(rec.partSHA, payload) {
		t.Fatalf("destination received %d bytes of part data, want %d", len(rec.partSHA), len(payload))
	}
	if rec.contentLength > 0 && rec.contentLength != rec.bodyLen {
		t.Fatalf("destination POST Content-Length %d, body was %d bytes", rec.contentLength, rec.bodyLen)
	}
}
