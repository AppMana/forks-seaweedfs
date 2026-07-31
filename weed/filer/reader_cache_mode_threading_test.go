package filer

import (
	"context"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/wdclient"
)

// The -readerCacheMode override is only useful if it survives the whole
// journey from the mount option down to the ReaderPattern that actually
// decides whether a chunk gets cached whole:
//
//	weed mount -readerCacheMode
//	  -> Option.ReaderCacheMode           (weed/mount/weedfs.go)
//	  -> filer.NewChunkGroupWithMode      (filechunk_group.go)
//	  -> ChunkGroup.readerCacheMode
//	  -> section.setupForRead             (filechunk_section.go)
//	  -> NewChunkReaderAtFromClientWithMode (reader_at.go)
//	  -> NewReaderPatternWithMode         (reader_pattern.go)
//	  -> ChunkReadAt.readerPattern.IsRandomMode()
//
// Every existing test covers one link in isolation, so an upstream merge that
// reverted filechunk_section.go's call site back to the mode-less
// NewChunkReaderAtFromClient would leave the flag parsing green, the
// ReaderPattern unit tests green, and the feature silently dead: the mount
// would accept -readerCacheMode=sequential and go on inferring. These tests
// assert the composition end to end.

func noopLookupFn() wdclient.LookupFileIdFunctionType {
	return func(ctx context.Context, fileId string) (targetUrls []string, err error) {
		return nil, nil
	}
}

// sectionReaderPatternFor drives the real setupForRead path and returns the
// ReaderPattern the section's reader was actually built with.
func sectionReaderPatternFor(t *testing.T, mode ReaderCacheMode) *ReaderPattern {
	t.Helper()

	group, err := NewChunkGroupWithMode(noopLookupFn(), alwaysMissChunkCache{}, []*filer_pb.FileChunk{}, 1, mode)
	if err != nil {
		t.Fatalf("NewChunkGroupWithMode(%q): %v", mode, err)
	}

	section := NewFileChunkSection(0)
	section.setupForRead(context.Background(), group, SectionSize)

	if section.reader == nil {
		t.Fatalf("setupForRead did not build a reader for mode %q", mode)
	}
	if section.reader.readerPattern == nil {
		t.Fatalf("reader for mode %q has no ReaderPattern", mode)
	}
	return section.reader.readerPattern
}

func TestReaderCacheModeReachesSectionReader(t *testing.T) {
	// "sequential" must pin IsRandomMode to false permanently -- including
	// after a burst of wildly non-contiguous reads, which under "auto" would
	// drive the counter negative and flip the classification. That
	// immunity-to-the-heuristic property is the entire point of the override
	// for mmap-heavy workloads.
	seq := sectionReaderPatternFor(t, ReaderCacheModeSequential)
	if seq.IsRandomMode() {
		t.Fatal("mode=sequential: IsRandomMode() = true immediately after construction; want false")
	}
	for i := int64(0); i < 4*ModeChangeLimit; i++ {
		seq.MonitorReadAt(i*512*1024*1024, 4096) // far-apart offsets: textbook random
	}
	if seq.IsRandomMode() {
		t.Fatal("mode=sequential: IsRandomMode() = true after random-looking reads; the override must not be overridable by the heuristic")
	}

	// "random" is the mirror image: pinned true even under perfectly
	// contiguous access.
	rnd := sectionReaderPatternFor(t, ReaderCacheModeRandom)
	if !rnd.IsRandomMode() {
		t.Fatal("mode=random: IsRandomMode() = false immediately after construction; want true")
	}
	for i := int64(0); i < 4*ModeChangeLimit; i++ {
		rnd.MonitorReadAt(i*4096, 4096) // contiguous: textbook sequential
	}
	if !rnd.IsRandomMode() {
		t.Fatal("mode=random: IsRandomMode() = false after sequential reads; the override must not be overridable by the heuristic")
	}
}

func TestReaderCacheModeAutoStillInfers(t *testing.T) {
	auto := sectionReaderPatternFor(t, ReaderCacheModeAuto)
	if auto.IsRandomMode() {
		t.Fatal("mode=auto: a fresh ReaderPattern must start non-random")
	}
	for i := int64(0); i < 4*ModeChangeLimit; i++ {
		auto.MonitorReadAt(i*512*1024*1024, 4096)
	}
	if !auto.IsRandomMode() {
		t.Fatal("mode=auto: IsRandomMode() = false after far-apart reads; auto must still infer, i.e. the override must not be forced on by default")
	}
}

// NewChunkGroup is the mode-less constructor upstream code calls. It must keep
// meaning "auto" so the fork's addition is strictly opt-in.
func TestNewChunkGroupDefaultsToAuto(t *testing.T) {
	group, err := NewChunkGroup(noopLookupFn(), alwaysMissChunkCache{}, []*filer_pb.FileChunk{}, 1)
	if err != nil {
		t.Fatalf("NewChunkGroup: %v", err)
	}
	if group.readerCacheMode != ReaderCacheModeAuto {
		t.Fatalf("NewChunkGroup readerCacheMode = %q; want %q", group.readerCacheMode, ReaderCacheModeAuto)
	}
}

// An unrecognized mode reaching this layer must behave like auto rather than
// panicking or forcing a value; the flag layer is responsible for rejecting
// it (see TestParseReaderCacheModeFailsClosed in weed/command).
func TestUnknownReaderCacheModeBehavesAsAuto(t *testing.T) {
	pattern := sectionReaderPatternFor(t, ReaderCacheMode("nonsense"))
	if pattern.forced != nil {
		t.Fatalf("unknown mode produced a forced ReaderPattern (%v); want inferred/auto behavior", *pattern.forced)
	}
}
