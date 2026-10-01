package s3err

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

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

func extractRequestIDFromBody(body string) string {
	re := regexp.MustCompile(`<RequestId>([^<]+)</RequestId>`)
	matches := re.FindStringSubmatch(body)
	if len(matches) < 2 {
		return ""
	}
	return matches[1]
}
