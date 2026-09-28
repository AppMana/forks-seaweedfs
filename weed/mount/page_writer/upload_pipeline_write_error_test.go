package page_writer

import (
	"errors"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/operation"
)

type failedWriteChunk struct {
	PageChunk
	n   int
	err error
}

func (c *failedWriteChunk) WriteDataAt([]byte, int64, int64) (int, error) { return c.n, c.err }

func TestUploadPipelinePreservesPartialWriteError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err, want error
	}{
		{"disk-full", syscall.ENOSPC, syscall.ENOSPC},
		{"short-without-error", nil, io.ErrShortWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := NewUploadPipeline(nil, 1024, nil, 0, t.TempDir(), nil)
			defer up.Shutdown()
			up.writableChunks[0] = &failedWriteChunk{PageChunk: NewMemChunk(0, 1024), n: 2, err: tc.err}
			n, err := up.SaveDataAt([]byte("four"), 0, false, 1)
			if n != 2 || !errors.Is(err, tc.want) {
				t.Fatalf("partial write = (%d, %v), want (2, %v)", n, err, tc.want)
			}
		})
	}
}

func TestUploadPipelineReturnsSwapWriteError(t *testing.T) {
	up := NewUploadPipeline(nil, 1024, nil, 0, t.TempDir(), nil)
	defer up.Shutdown()
	if n, err := up.SaveDataAt([]byte("first"), 0, false, 1); n != 5 || err != nil {
		t.Fatalf("initial write: %d, %v", n, err)
	}
	// A real OS write failure without filling the host filesystem or a VM.
	if err := up.swapFile.file.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := up.SaveDataAt([]byte("lost"), 5, false, 2)
	if n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed swap write = (%d, %v), want (0, file closed)", n, err)
	}
}

func TestSwapSaveContentReportsReadFailure(t *testing.T) {
	for _, failure := range []string{"healthy", "closed", "truncated"} {
		t.Run(failure, func(t *testing.T) {
			sf := NewSwapFile(t.TempDir(), 1024)
			defer sf.FreeResource()
			chunk := sf.NewSwapFileChunk(0)
			if n, err := chunk.WriteDataAt([]byte("intact"), 0, 1); n != 6 || err != nil {
				t.Fatalf("initial write: %d, %v", n, err)
			}
			var failureErr error
			if failure == "closed" {
				failureErr = sf.file.Close()
			} else if failure == "truncated" {
				failureErr = sf.file.Truncate(1026) // physical chunk starts at 1024: leave only two of six bytes
			}
			if failureErr != nil {
				t.Fatal(failureErr)
			}
			called := false
			var readErr error
			var content []byte
			chunk.SaveContent(func(r io.Reader, offset, size, ts int64, cleanup func()) {
				defer cleanup()
				called = true
				content, readErr = io.ReadAll(r)
				if readErr != nil {
					// The actual uploader must reject this reader before it
					// touches the deliberately nil filer client or request.
					uploader := operation.NewUploaderWithHttpClient(nil)
					_, _, uploadErr, _ := uploader.UploadWithRetry(nil, nil, &operation.UploadOption{}, r)
					if !errors.Is(uploadErr, readErr) {
						t.Errorf("uploader lost swap read failure: %v", uploadErr)
					}
				}
			})
			if failure == "healthy" {
				if !called || readErr != nil || string(content) != "intact" {
					t.Fatalf("healthy save: called=%v content=%q error=%v", called, content, readErr)
				}
				return
			}
			if !called || readErr == nil {
				t.Fatalf("failed swap read silently discarded: callback=%v error=%v", called, readErr)
			}
			if len(content) != 0 {
				t.Fatalf("read failure exposed partial successful prefix %q", content)
			}
		})
	}
}
