package needle

import (
	"bytes"
	"fmt"

	. "github.com/seaweedfs/seaweedfs/weed/storage/types"
)

func writeNeedleByVersion(version Version, n *Needle, offset uint64, bytesBuffer *bytes.Buffer) (size Size, actualSize int64, err error) {
	// Switch logic moved from needle_write.go
	switch version {
	case Version1:
		size, actualSize, err = writeNeedleV1(n, offset, bytesBuffer)
	case Version2:
		size, actualSize, err = writeNeedleV2(n, offset, bytesBuffer)
	case Version3:
		size, actualSize, err = writeNeedleV3(n, offset, bytesBuffer)
	default:
		err = fmt.Errorf("unsupported version: %d", version)
	}
	return
}

// writeNeedleFramingByVersion encodes everything of the record except n.Data;
// dataAt is where n.Data belongs within the buffer.
func writeNeedleFramingByVersion(version Version, n *Needle, offset uint64, bytesBuffer *bytes.Buffer) (size Size, actualSize int64, dataAt int, err error) {
	switch version {
	case Version1:
		return writeNeedleV1Framing(n, offset, bytesBuffer, false)
	case Version2:
		return writeNeedleV2Framing(n, offset, bytesBuffer, false)
	case Version3:
		return writeNeedleV3Framing(n, offset, bytesBuffer, false)
	default:
		err = fmt.Errorf("unsupported version: %d", version)
	}
	return
}
