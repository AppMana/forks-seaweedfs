package page_writer

import (
	"github.com/seaweedfs/seaweedfs/weed/util"
	"testing"
)

func TestUploadPipelineRangeHelpersCoverDeclaredBytes(t *testing.T) {
	up := NewUploadPipeline(nil, 4096, nil, 16, t.TempDir(), nil)
	t.Cleanup(up.Shutdown)
	writeRange(t, up, 1024, 2048)
	for offset := int64(1024); offset < 2048; offset += 4 {
		var got [4]byte
		if stop := up.MaybeReadDataAt(got[:], offset, 0); stop != offset+4 || util.BytesToUint32(got[:]) != uint32(offset) {
			t.Fatalf("declared range unwritten at %d: stop=%d data=%v", offset, stop, got)
		}
	}
}
