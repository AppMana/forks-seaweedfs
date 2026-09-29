package needle

import (
	"bytes"

	. "github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func writeNeedleV3(n *Needle, offset uint64, bytesBuffer *bytes.Buffer) (size Size, actualSize int64, err error) {
	size, actualSize, _, err = writeNeedleV3Framing(n, offset, bytesBuffer, true)
	return
}

func writeNeedleV3Framing(n *Needle, offset uint64, bytesBuffer *bytes.Buffer, withData bool) (size Size, actualSize int64, dataAt int, err error) {
	return writeNeedleCommon(n, offset, bytesBuffer, Version3, withData, func(n *Needle, header []byte, bytesBuffer *bytes.Buffer, padding int) {
		util.Uint32toBytes(header[0:NeedleChecksumSize], uint32(n.Checksum))
		util.Uint64toBytes(header[NeedleChecksumSize:NeedleChecksumSize+TimestampSize], n.AppendAtNs)
		bytesBuffer.Write(header[0 : NeedleChecksumSize+TimestampSize+padding])
	})
}
