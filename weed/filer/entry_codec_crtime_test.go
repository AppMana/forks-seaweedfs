package filer

import (
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

func TestCreationTimePrecisionSurvivesStorageCodec(t *testing.T) {
	for _, nanos := range []int32{0, 100, 123456700, 999999900} {
		want := time.Unix(1700000000, int64(nanos))
		entry := &Entry{FullPath: "/birth", Attr: Attr{Mode: 0644, Crtime: want, Mtime: want, Ctime: want}}
		if got := EntryAttributeToPb(entry).CrtimeNs; got != nanos {
			t.Errorf("allocated encode: nanos=%d want=%d", got, nanos)
		}
		attributes := &filer_pb.FuseAttributes{CrtimeNs: 42}
		EntryAttributeToExistingPb(entry, attributes)
		if attributes.CrtimeNs != nanos {
			t.Errorf("reused encode: nanos=%d want=%d", attributes.CrtimeNs, nanos)
		}
		if got := PbToEntryAttribute(&filer_pb.FuseAttributes{Crtime: want.Unix(), CrtimeNs: nanos}).Crtime; !got.Equal(want) {
			t.Errorf("decode=%v want=%v", got, want)
		}
		data, err := entry.EncodeAttributesAndChunks()
		if err != nil {
			t.Fatal(err)
		}
		var restored Entry
		if err := restored.DecodeAttributesAndChunks(data); err != nil {
			t.Fatal(err)
		}
		if !restored.Crtime.Equal(want) {
			t.Errorf("stored round trip=%v want=%v", restored.Crtime, want)
		}
	}
}

func TestCreationTimeNanosecondsParticipateInEquality(t *testing.T) {
	one := &Entry{Attr: Attr{Crtime: time.Unix(1700000000, 100)}}
	two := &Entry{Attr: Attr{Crtime: time.Unix(1700000000, 200)}}
	if EqualEntry(one, two) {
		t.Fatal("subsecond creation-time update would be discarded as unchanged")
	}
}
