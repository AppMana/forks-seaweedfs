package s3api

import (
	"runtime"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// TestNewChunkUploadOption_NoPayloadBuffer pins that the chunk-copy upload
// option carries no body buffer. upload_content streams the payload from the
// caller's slice without copying it or using a byte pool, so any buffer here
// would be a chunk-sized allocation per copied chunk that nothing reads.
func TestNewChunkUploadOption_NoPayloadBuffer(t *testing.T) {
	assign := &filer_pb.AssignVolumeResponse{
		FileId: "1,foo",
		Location: &filer_pb.Location{
			Url:       "127.0.0.1:8080",
			PublicUrl: "127.0.0.1:8080",
		},
		Fsync: true,
	}

	opt := newChunkUploadOption(assign, true)
	if opt.BytesBuffer != nil {
		t.Fatalf("BytesBuffer = %p (cap %d), want nil: the upload streams the payload",
			opt.BytesBuffer, opt.BytesBuffer.Cap())
	}
	if opt.UploadUrl != "http://127.0.0.1:8080/1,foo?fsync=true" {
		t.Errorf("UploadUrl = %q", opt.UploadUrl)
	}
	if !opt.IsInputCompressed {
		t.Error("IsInputCompressed must follow the source chunk")
	}
	if opt.Cipher {
		t.Error("Cipher must be false: chunk-copy data is already encrypted if the source had a CipherKey")
	}

	// The option itself must be small: a few hundred bytes of strings and the
	// struct, never anything sized like a chunk.
	const calls = 100
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < calls; i++ {
		_ = newChunkUploadOption(assign, false)
	}
	runtime.ReadMemStats(&after)
	if perCall := (after.TotalAlloc - before.TotalAlloc) / calls; perCall > 4096 {
		t.Errorf("newChunkUploadOption allocates %d bytes per call, want well under a chunk", perCall)
	}
}
