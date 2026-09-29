package operation

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/security"
)

// streamCaptureClient records what the transport would put on the wire.
type streamCaptureClient struct {
	mu               sync.Mutex
	contentLength    int64
	transferEncoding []string
	header           http.Header
	body             []byte
}

func (c *streamCaptureClient) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.contentLength = req.ContentLength
	c.transferEncoding = req.TransferEncoding
	c.header = req.Header.Clone()
	c.body = body
	c.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusCreated,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"name":"n","size":1}`)),
	}, nil
}

// discardClient consumes the request body the way a transport would, without
// retaining it, so allocation measurements see only the uploader's own copies.
type discardClient struct{}

func (discardClient) Do(req *http.Request) (*http.Response, error) {
	if _, err := io.Copy(io.Discard, req.Body); err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusCreated,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"name":"n","size":1}`)),
	}, nil
}

// legacyMultipartBody is the multipart body upload_content produced before it
// streamed the payload: every header and byte, for a given boundary.
func legacyMultipartBody(t *testing.T, boundary string, data []byte, opt UploadOption, compressed bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.SetBoundary(boundary); err != nil {
		t.Fatalf("set boundary: %v", err)
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": opt.Filename}))
	h.Set("Idempotency-Key", opt.UploadUrl)
	mimeType := opt.MimeType
	if mimeType == "" {
		mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(opt.Filename)))
	}
	if mimeType != "" {
		h.Set("Content-Type", mimeType)
	}
	if compressed {
		h.Set("Content-Encoding", "gzip")
	}
	if opt.Md5 != "" {
		h.Set("Content-MD5", opt.Md5)
	}
	part, err := w.CreatePart(h)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes()
}

func replicationPayload(n int) []byte {
	payload := make([]byte, n)
	for i := range payload {
		payload[i] = byte(i*7 + i>>9)
	}
	return payload
}

// TestReplicationUploadSendsExactLegacyBody pins the wire format: an exact
// Content-Length (a chunked body makes the receiver reserve its 256 MiB
// unknown-size ceiling) and a body byte-identical to the buffered path.
func TestReplicationUploadSendsExactLegacyBody(t *testing.T) {
	cases := []struct {
		name string
		opt  UploadOption
	}{
		{
			name: "replica with pairs md5 jwt",
			opt: UploadOption{
				UploadUrl:     "http://volume-b:8080/3,01637037d6?type=replicate&ts=1790667986",
				Filename:      "Newtonsoft.Json.pdb",
				IsReplication: true,
				MimeType:      "application/octet-stream",
				PairMap:       map[string]string{"Seaweed-Owner": "harbor"},
				Jwt:           security.EncodedJwt("jwt-token"),
				Md5:           "1B2M2Y8AsgTpgAmY7PhCfg==",
				MaxAttempts:   1,
			},
		},
		{
			name: "replica of a compressed needle, mime from extension",
			opt: UploadOption{
				UploadUrl:         "http://volume-c:8080/4,0a?type=replicate",
				Filename:          "layer.json",
				IsInputCompressed: true,
				IsReplication:     true,
				MaxAttempts:       1,
			},
		},
		{
			name: "replica with non-ascii filename",
			opt: UploadOption{
				UploadUrl:     "http://volume-d:8080/5,0b?type=replicate",
				Filename:      "Übersicht données.bin",
				IsReplication: true,
				MaxAttempts:   1,
			},
		},
	}
	payload := replicationPayload(8<<20 + 13)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &streamCaptureClient{}
			opt := tc.opt
			opt.BytesBuffer = new(bytes.Buffer)
			if _, err := newUploader(client).UploadData(context.Background(), payload, &opt); err != nil {
				t.Fatalf("upload: %v", err)
			}
			_, params, err := mime.ParseMediaType(client.header.Get("Content-Type"))
			if err != nil {
				t.Fatalf("content type %q: %v", client.header.Get("Content-Type"), err)
			}
			want := legacyMultipartBody(t, params["boundary"], payload, tc.opt, tc.opt.IsInputCompressed)
			if !bytes.Equal(client.body, want) {
				t.Fatalf("body differs from the buffered multipart body: got %d bytes, want %d", len(client.body), len(want))
			}
			if client.contentLength != int64(len(want)) {
				t.Fatalf("Content-Length = %d, want exact %d", client.contentLength, len(want))
			}
			if len(client.transferEncoding) != 0 {
				t.Fatalf("Transfer-Encoding = %v, want none", client.transferEncoding)
			}
			for k, v := range tc.opt.PairMap {
				if got := client.header.Get(k); got != v {
					t.Fatalf("pair header %s = %q, want %q", k, got, v)
				}
			}
			if tc.opt.Jwt != "" {
				if got, want := client.header.Get("Authorization"), security.BearerPrefix+string(tc.opt.Jwt); got != want {
					t.Fatalf("Authorization = %q, want %q", got, want)
				}
			}
		})
	}
}

// TestReplicationUploadDoesNotCopyPayload is the 2026-09-29 OOM: the primary
// built a full multipart copy of every needle for each replica, while
// admission charged the needle once. Store replication hands each replica
// goroutine its own buffer, so measure with a fresh buffer per upload.
func TestReplicationUploadDoesNotCopyPayload(t *testing.T) {
	const size = 8 << 20
	const uploads = 6
	payload := replicationPayload(size)
	uploader := newUploader(discardClient{})
	upload := func() {
		_, err := uploader.UploadData(context.Background(), payload, &UploadOption{
			UploadUrl:     "http://volume-b:8080/3,01637037d6?type=replicate",
			Filename:      "chunk",
			IsReplication: true,
			BytesBuffer:   new(bytes.Buffer),
			MaxAttempts:   1,
		})
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
	}
	upload() // warm up lazily initialised transport state

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < uploads; i++ {
		upload()
	}
	runtime.ReadMemStats(&after)
	perUpload := (after.TotalAlloc - before.TotalAlloc) / uploads
	if perUpload >= size/8 {
		t.Fatalf("allocated %d bytes per replicated %d-byte upload; the payload must be streamed, not copied", perUpload, size)
	}
	t.Logf("allocated %d bytes per replicated %d-byte upload", perUpload, size)
}

// blockingClient holds every request inside Do, body unread, until released:
// the state of a primary waiting on slow replicas.
type blockingClient struct {
	entered chan struct{}
	release chan struct{}
}

func (c *blockingClient) Do(req *http.Request) (*http.Response, error) {
	c.entered <- struct{}{}
	<-c.release
	if _, err := io.Copy(io.Discard, req.Body); err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusCreated,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"name":"n","size":1}`)),
	}, nil
}

// TestReplicationUploadsInFlightHoldNoPayloadCopies measures the heap held
// while several replica uploads wait on their peers, the path without a
// caller-supplied buffer included (it used the process-wide byte buffer pool).
func TestReplicationUploadsInFlightHoldNoPayloadCopies(t *testing.T) {
	const size = 8 << 20
	const inFlight = 4
	payload := replicationPayload(size)
	client := &blockingClient{entered: make(chan struct{}, inFlight), release: make(chan struct{})}
	uploader := newUploader(client)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var wg sync.WaitGroup
	errs := make(chan error, inFlight)
	for i := 0; i < inFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := uploader.UploadData(context.Background(), payload, &UploadOption{
				UploadUrl:     "http://volume-b:8080/3,01637037d6?type=replicate",
				Filename:      "chunk",
				IsReplication: true,
				MaxAttempts:   1,
			})
			errs <- err
		}()
	}
	for i := 0; i < inFlight; i++ {
		<-client.entered
	}
	runtime.GC()
	var during runtime.MemStats
	runtime.ReadMemStats(&during)
	close(client.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
	}

	held := int64(during.HeapAlloc) - int64(before.HeapAlloc)
	if held >= size/2 {
		t.Fatalf("%d in-flight replica uploads of %d bytes hold %d heap bytes; they must reference the needle, not copy it", inFlight, size, held)
	}
	t.Logf("%d in-flight replica uploads hold %d heap bytes", inFlight, held)
}
