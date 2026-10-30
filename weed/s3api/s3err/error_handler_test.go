package s3err

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/seaweedfs/seaweedfs/weed/util/request_id"
	"github.com/stretchr/testify/assert"
)

type observedResponseWriter struct {
	http.ResponseWriter
	writes int
	err    error
}

func (w *observedResponseWriter) Write(p []byte) (int, error) {
	w.writes++
	n, err := w.ResponseWriter.Write(p)
	w.err = err
	return n, err
}

func (w *observedResponseWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

// A real net/http server rejects a nonempty Write after 204/304;
// ResponseRecorder alone does not enforce this constraint.
func TestWriteResponseBodySemantics(t *testing.T) {
	for _, tc := range []struct {
		name, method      string
		status            int
		payload, wantBody string
		wantWrites        int
	}{
		{"empty-204", "DELETE", 204, "", "", 0},
		{"payload-204", "DELETE", 204, "ignored", "", 0},
		{"not-modified", "GET", 304, "ignored", "", 0},
		{"head", "HEAD", 200, "payload", "", 0},
		{"get", "GET", 200, "payload", "payload", 1},
		{"error", "GET", 404, "missing", "missing", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := make(chan *observedResponseWriter, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ow := &observedResponseWriter{ResponseWriter: w}
				WriteResponse(ow, r, tc.status, []byte(tc.payload), mimeNone)
				observed <- ow
			}))
			defer srv.Close()
			req, err := http.NewRequest(tc.method, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			ow := <-observed
			assert.NoError(t, ow.err)
			assert.Equal(t, tc.wantWrites, ow.writes)
			assert.Equal(t, tc.status, resp.StatusCode)
			assert.Equal(t, tc.wantBody, string(body))
			if tc.method == "HEAD" {
				assert.Equal(t, int64(len(tc.payload)), resp.ContentLength)
			}
		})
	}
}

func TestWriteErrorResponseReusesRequestID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/bucket/object", nil)
	req = mux.SetURLVars(req, map[string]string{
		"bucket": "bucket",
		"object": "object",
	})
	req = req.WithContext(request_id.Set(req.Context(), "req-123"))

	rr := httptest.NewRecorder()
	WriteErrorResponse(rr, req, ErrNoSuchKey)

	assert.Equal(t, "req-123", rr.Header().Get(request_id.AmzRequestIDHeader))
	assert.Equal(t, "req-123", extractRequestIDFromBody(rr.Body.String()))
}

func TestWriteNotModifiedResponseHasNoErrorBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/bucket/object", nil)
	rr := httptest.NewRecorder()
	WriteErrorResponse(rr, req, ErrNotModified)
	assert.Equal(t, http.StatusNotModified, rr.Code)
	assert.Empty(t, rr.Body.String())
	assert.Empty(t, rr.Header().Get("Content-Length"))
}

func TestWriteErrorResponseDrainsRequestBodyBeforeWriting(t *testing.T) {
	body := &trackingReadCloser{data: bytes.Repeat([]byte("a"), 1024)}
	req := httptest.NewRequest(http.MethodPut, "/bucket/object", nil)
	req.Body = body
	req.ContentLength = int64(body.remaining())
	req = mux.SetURLVars(req, map[string]string{
		"bucket": "bucket",
		"object": "object",
	})

	rr := &drainCheckingResponseWriter{
		header: make(http.Header),
		body:   body,
	}

	WriteErrorResponse(rr, req, ErrInternalError)

	assert.Empty(t, rr.writeHeaderErr)
	assert.Equal(t, 0, body.remaining())
	assert.Equal(t, http.StatusInternalServerError, rr.status)
	assert.Len(t, rr.readDeadlines, 2)
	assert.False(t, rr.readDeadlines[0].IsZero())
	assert.True(t, rr.readDeadlines[1].IsZero())
}

func TestWriteResponseOmitsBodyWhenNotAllowed(t *testing.T) {
	payload := []byte("<Error><Code>NoSuchKey</Code></Error>")
	for _, tc := range []struct {
		name              string
		method            string
		status            int
		wantContentLength string
	}{
		{"head request", http.MethodHead, http.StatusNotFound, strconv.Itoa(len(payload))},
		{"no content", http.MethodGet, http.StatusNoContent, ""},
		{"not modified", http.MethodGet, http.StatusNotModified, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/bucket/object", nil)
			rr := httptest.NewRecorder()
			WriteResponse(rr, req, tc.status, payload, MimeXML)
			assert.Empty(t, rr.Body.String())
			assert.Equal(t, tc.wantContentLength, rr.Header().Get("Content-Length"))
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/bucket/object", nil)
	rr := httptest.NewRecorder()
	WriteResponse(rr, req, http.StatusOK, payload, MimeXML)
	assert.Equal(t, string(payload), rr.Body.String())
}

func extractRequestIDFromBody(body string) string {
	re := regexp.MustCompile(`<RequestId>([^<]+)</RequestId>`)
	matches := re.FindStringSubmatch(body)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}

type trackingReadCloser struct {
	data []byte
}

func (t *trackingReadCloser) Read(p []byte) (int, error) {
	if len(t.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, t.data)
	t.data = t.data[n:]
	return n, nil
}

func (t *trackingReadCloser) Close() error {
	return nil
}

func (t *trackingReadCloser) remaining() int {
	return len(t.data)
}

type drainCheckingResponseWriter struct {
	header         http.Header
	body           *trackingReadCloser
	status         int
	writeHeaderErr string
	readDeadlines  []time.Time
}

func (d *drainCheckingResponseWriter) Header() http.Header {
	return d.header
}

func (d *drainCheckingResponseWriter) Write(p []byte) (int, error) {
	if d.status == 0 {
		d.WriteHeader(http.StatusOK)
	}
	return len(p), nil
}

func (d *drainCheckingResponseWriter) WriteHeader(status int) {
	d.status = status
	if d.body.remaining() != 0 {
		d.writeHeaderErr = "request body was not drained before WriteHeader"
	}
}

func (d *drainCheckingResponseWriter) Flush() {}

func (d *drainCheckingResponseWriter) SetReadDeadline(t time.Time) error {
	d.readDeadlines = append(d.readDeadlines, t)
	return nil
}
