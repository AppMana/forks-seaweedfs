package mount

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/mount/page_writer"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// Once a chunk upload fails its data is gone, so the handle must reject
// further writes instead of buffering the rest of the file — against a full
// cluster that turned cp into an hours-long crawl that only errored at close.
func TestChunkedDirtyPagesFailWritesAfterUploadError(t *testing.T) {
	fh := &FileHandle{wfs: &WFS{option: &Option{}}}
	pages := newMemoryChunkPages(fh, 1024)
	defer pages.Destroy()
	pages.hasWrites = true

	uploadErr := errors.New("assign volume failure: no writable volumes")
	pages.setLastError(uploadErr)

	if err := pages.AddPage(0, []byte("x"), true, 1); !errors.Is(err, uploadErr) {
		t.Fatalf("AddPage after upload failure = %v, want sticky %v", err, uploadErr)
	}
	if err := pages.FlushData(); !errors.Is(err, uploadErr) {
		t.Fatalf("FlushData after upload failure = %v, want wrapped %v", err, uploadErr)
	}
	// First failure wins; later errors must not mask the root cause.
	pages.setLastError(errors.New("later error"))
	if err := pages.LastError(); !errors.Is(err, uploadErr) {
		t.Fatalf("LastError = %v, want first error %v", err, uploadErr)
	}
}

func TestLaterChunkAllocationFailurePoisonsHandle(t *testing.T) {
	// The first partial chunk fits in memory. The second needs swap, whose
	// configured directory is deliberately a regular file, not a directory.
	badDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badDir, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	fh := &FileHandle{wfs: &WFS{option: &Option{ConcurrentWriters: 1, uniqueCacheDirForWrite: badDir}}}
	pages := &ChunkedDirtyPages{fh: fh}
	// Drain the actual accepted prefix through a local sink. No filer or
	// volume server is needed to prove that sync retains the request error.
	pages.uploadPipeline = page_writer.NewUploadPipeline(util.NewLimitedConcurrentExecutor(1), 1024,
		func(reader io.Reader, offset, size, ts int64, cleanup func()) {
			defer cleanup()
			_, _ = io.Copy(io.Discard, reader)
		},
		1, badDir, nil)
	defer pages.Destroy()
	pw := &PageWriter{fh: fh, chunkSize: 1024, randomWriter: pages}
	if err := pw.AddPage(1020, []byte("abcdefgh"), true, 1); err == nil {
		t.Fatal("cross-chunk write unexpectedly succeeded")
	}
	if pages.LastError() == nil {
		t.Error("later chunk allocation failure was not retained")
	}
	firstError := pages.LastError()
	for attempt := 0; attempt < 2; attempt++ {
		if err := pages.FlushData(); err == nil || !errors.Is(err, firstError) {
			t.Fatalf("flush attempt %d = %v, want retained request failure %v", attempt, err, firstError)
		}
	}
	if err := pages.AddPage(1020, []byte("next"), true, 2); err == nil {
		t.Error("handle accepted more writes after losing part of a request")
	}
}
