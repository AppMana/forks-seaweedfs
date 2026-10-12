package needle

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"runtime"
	"testing"
)

// TestParseUploadHoldsOnlyTheBody: the volume server's upload admission
// reserves a request's Content-Length, so the buffer ParseUpload reads the
// body into must not be larger than that, and reading it must not allocate
// the body more than once. bytes.Buffer.ReadFrom wants 512 free bytes before
// every read, so a body that exactly fills its buffer (a 4 MiB chunk into the
// 4 MiB pre-grow) doubled the buffer just to read EOF: 8 MiB held and 12 MiB
// allocated for a 4 MiB reservation.
func TestParseUploadHoldsOnlyTheBody(t *testing.T) {
	const sizeLimit = 256 << 20
	// A large allocation is rounded up to whole 8 KiB pages.
	const allocationRounding = 8 << 10

	for _, size := range []int{2 << 20, 4<<20 - 1, 4 << 20, 4<<20 + 1, 6 << 20, 16 << 20} {
		for _, multipartBody := range []bool{true, false} {
			name := fmt.Sprintf("%d bytes multipart=%v", size, multipartBody)
			t.Run(name, func(t *testing.T) {
				data := bytes.Repeat([]byte{0xa5}, size)
				req := uploadRequest(t, data, multipartBody)

				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				pu, err := ParseUpload(req, sizeLimit, &bytes.Buffer{})
				runtime.ReadMemStats(&after)
				if err != nil {
					t.Fatalf("ParseUpload: %v", err)
				}
				if !bytes.Equal(pu.Data, data) {
					t.Fatalf("parsed %d bytes, want the %d sent", len(pu.Data), len(data))
				}
				if held := int64(cap(pu.Data)); held > req.ContentLength+allocationRounding {
					t.Errorf("body buffer capacity %d for Content-Length %d", held, req.ContentLength)
				}
				// The single body allocation plus small parser state.
				if allocated := int64(after.TotalAlloc - before.TotalAlloc); !raceEnabled && allocated > req.ContentLength+allocationRounding+64<<10 {
					t.Errorf("allocated %d bytes to read a Content-Length of %d", allocated, req.ContentLength)
				}
			})
		}
	}
}

// TestParseUploadUnknownLengthReadsWholeBody covers chunked requests, which
// have no Content-Length to size the buffer from.
func TestParseUploadUnknownLengthReadsWholeBody(t *testing.T) {
	for _, size := range []int{0, 1, 511, 512, 4 << 20, 4<<20 + 1} {
		for _, multipartBody := range []bool{true, false} {
			t.Run(fmt.Sprintf("%d bytes multipart=%v", size, multipartBody), func(t *testing.T) {
				data := bytes.Repeat([]byte{0x3c}, size)
				req := uploadRequest(t, data, multipartBody)
				req.ContentLength = -1
				pu, err := ParseUpload(req, 256<<20, &bytes.Buffer{})
				if err != nil {
					t.Fatalf("ParseUpload: %v", err)
				}
				if !bytes.Equal(pu.Data, data) {
					t.Fatalf("parsed %d bytes, want the %d sent", len(pu.Data), len(data))
				}
			})
		}
	}
}

func TestParseUploadRejectsBodyOverSizeLimit(t *testing.T) {
	for _, multipartBody := range []bool{true, false} {
		req := uploadRequest(t, bytes.Repeat([]byte{1}, 1025), multipartBody)
		if _, err := ParseUpload(req, 1024, &bytes.Buffer{}); err == nil {
			t.Fatalf("multipart=%v: a 1025 byte body passed a 1024 byte limit", multipartBody)
		}
	}
}

func uploadRequest(t *testing.T, data []byte, multipartBody bool) *http.Request {
	t.Helper()
	if !multipartBody {
		req := httptest.NewRequest(http.MethodPut, "/3,01637037d6", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/octet-stream")
		req.ContentLength = int64(len(data))
		return req
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="chunk"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(header)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/3,01637037d6?type=replicate", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(body.Len())
	return req
}
