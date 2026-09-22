package needle

import (
	"errors"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

func TestReadNeedleBodyRejectsTruncatedFieldsWithoutPanic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    []byte
		version Version
	}{
		{"data_size", []byte{1}, Version2},
		{"v2_checksum", make([]byte, 4), Version2},
		{"v3_timestamp", make([]byte, 4+NeedleChecksumSize), Version3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &Needle{Size: types.Size(len(tc.body))}
			if err := n.ReadNeedleBodyBytes(tc.body, tc.version); !errors.Is(err, ErrorCorrupted) {
				t.Fatalf("got %v, want corruption error", err)
			}
		})
	}
}
