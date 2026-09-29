package needle

import (
	"bytes"
	"math"

	. "github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func writeNeedleV2(n *Needle, offset uint64, bytesBuffer *bytes.Buffer) (size Size, actualSize int64, err error) {
	size, actualSize, _, err = writeNeedleV2Framing(n, offset, bytesBuffer, true)
	return
}

func writeNeedleV2Framing(n *Needle, offset uint64, bytesBuffer *bytes.Buffer, withData bool) (size Size, actualSize int64, dataAt int, err error) {
	return writeNeedleCommon(n, offset, bytesBuffer, Version2, withData, func(n *Needle, header []byte, bytesBuffer *bytes.Buffer, padding int) {
		util.Uint32toBytes(header[0:NeedleChecksumSize], uint32(n.Checksum))
		bytesBuffer.Write(header[0 : NeedleChecksumSize+padding])
	})
}

// writeNeedleCommon encodes the v2/v3 record into bytesBuffer. With withData
// false it leaves out n.Data and returns in dataAt the buffer position where
// the payload belongs, so the caller can write the payload from n.Data
// without copying it; with withData true dataAt is where it was written.
func writeNeedleCommon(n *Needle, offset uint64, bytesBuffer *bytes.Buffer, version Version, withData bool, writeFooter func(n *Needle, header []byte, bytesBuffer *bytes.Buffer, padding int)) (size Size, actualSize int64, dataAt int, err error) {
	bytesBuffer.Reset()
	header := make([]byte, NeedleHeaderSize+TimestampSize)
	CookieToBytes(header[0:CookieSize], n.Cookie)
	NeedleIdToBytes(header[CookieSize:CookieSize+NeedleIdSize], n.Id)
	if len(n.Name) >= math.MaxUint8 {
		n.NameSize = math.MaxUint8
	} else {
		n.NameSize = uint8(len(n.Name))
	}
	n.DataSize, n.MimeSize = uint32(len(n.Data)), uint8(len(n.Mime))
	if n.DataSize > 0 {
		n.Size = 4 + Size(n.DataSize) + 1
		if n.HasName() {
			n.Size = n.Size + 1 + Size(n.NameSize)
		}
		if n.HasMime() {
			n.Size = n.Size + 1 + Size(n.MimeSize)
		}
		if n.HasLastModifiedDate() {
			n.Size = n.Size + LastModifiedBytesLength
		}
		if n.HasTtl() {
			n.Size = n.Size + TtlBytesLength
		}
		if n.HasPairs() {
			n.Size += 2 + Size(n.PairsSize)
		}
	} else {
		n.Size = 0
	}
	SizeToBytes(header[CookieSize+NeedleIdSize:CookieSize+NeedleIdSize+SizeSize], n.Size)
	bytesBuffer.Write(header[0:NeedleHeaderSize])
	dataAt = bytesBuffer.Len()
	if n.DataSize > 0 {
		util.Uint32toBytes(header[0:4], n.DataSize)
		bytesBuffer.Write(header[0:4])
		dataAt = bytesBuffer.Len()
		if withData {
			bytesBuffer.Write(n.Data)
		}
		util.Uint8toBytes(header[0:1], n.Flags)
		bytesBuffer.Write(header[0:1])
		if n.HasName() {
			util.Uint8toBytes(header[0:1], n.NameSize)
			bytesBuffer.Write(header[0:1])
			bytesBuffer.Write(n.Name[:n.NameSize])
		}
		if n.HasMime() {
			util.Uint8toBytes(header[0:1], n.MimeSize)
			bytesBuffer.Write(header[0:1])
			bytesBuffer.Write(n.Mime)
		}
		if n.HasLastModifiedDate() {
			util.Uint64toBytes(header[0:8], n.LastModified)
			bytesBuffer.Write(header[8-LastModifiedBytesLength : 8])
		}
		if n.HasTtl() && n.Ttl != nil {
			n.Ttl.ToBytes(header[0:TtlBytesLength])
			bytesBuffer.Write(header[0:TtlBytesLength])
		}
		if n.HasPairs() {
			util.Uint16toBytes(header[0:2], n.PairsSize)
			bytesBuffer.Write(header[0:2])
			bytesBuffer.Write(n.Pairs)
		}
	}
	padding := PaddingLength(n.Size, version)
	writeFooter(n, header, bytesBuffer, int(padding))
	size = Size(n.DataSize)
	actualSize = GetActualSize(n.Size, version)
	return size, actualSize, dataAt, nil
}
