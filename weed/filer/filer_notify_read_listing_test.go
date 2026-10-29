package filer

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"github.com/seaweedfs/seaweedfs/weed/util/log_buffer"
	"github.com/seaweedfs/seaweedfs/weed/wdclient"
)

// countingListStore counts the entries the store hands out per directory, which
// is what a networked store (etcd, a SQL server) ships to the filer.
type countingListStore struct {
	*stubFilerStore
	mu     sync.Mutex
	yields map[string]int
}

func (s *countingListStore) count(dirPath util.FullPath, each ListEachEntryFunc) ListEachEntryFunc {
	return func(entry *Entry) (bool, error) {
		s.mu.Lock()
		s.yields[string(dirPath)]++
		s.mu.Unlock()
		return each(entry)
	}
}

func (s *countingListStore) ListDirectoryEntries(ctx context.Context, dirPath util.FullPath, startFileName string, includeStartFile bool, limit int64, each ListEachEntryFunc) (string, error) {
	return s.stubFilerStore.ListDirectoryEntries(ctx, dirPath, startFileName, includeStartFile, limit, s.count(dirPath, each))
}

func (s *countingListStore) ListDirectoryPrefixedEntries(ctx context.Context, dirPath util.FullPath, startFileName string, includeStartFile bool, limit int64, prefix string, each ListEachEntryFunc) (string, error) {
	return s.stubFilerStore.ListDirectoryPrefixedEntries(ctx, dirPath, startFileName, includeStartFile, limit, prefix, s.count(dirPath, each))
}

// seedFullDayOfLogs writes one minute file per minute per filer for a whole day.
func seedFullDayOfLogs(t *testing.T, f *Filer, day string, filerIds ...string) {
	t.Helper()
	ctx := context.Background()
	for _, dir := range []string{SystemLogDir, SystemLogDir + "/" + day} {
		if err := f.Store.InsertEntry(ctx, &Entry{FullPath: util.FullPath(dir), Attr: Attr{Mode: 0o755 | 1<<31}}); err != nil {
			t.Fatal(err)
		}
	}
	for m := 0; m < 24*60; m++ {
		for _, id := range filerIds {
			name := fmt.Sprintf("%02d-%02d.%s", m/60, m%60, id)
			entry := &Entry{
				FullPath: util.NewFullPath(SystemLogDir+"/"+day, name),
				Chunks:   []*filer_pb.FileChunk{{FileId: "1,0" + name, Size: 1}},
			}
			if err := f.Store.InsertEntry(ctx, entry); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A caught-up subscriber re-reads the persisted log on every flush. The read must
// list only the minute files at or after its scan start, not every file the day
// has accumulated: on a networked store a full-day listing per subscriber per
// flush is megabytes per second that the filer then discards.
func TestPersistedLogReadsListOnlyFromTheScanStartMinute(t *testing.T) {
	stub := newStubFilerStore()
	store := &countingListStore{stubFilerStore: stub, yields: map[string]int{}}
	f, _ := newTestFilerWithStubStore()
	f.Store = NewFilerStoreWrapper(store)

	day := "2026-10-28"
	seedFullDayOfLogs(t, f, day, "filerA", "filerB", "filerC")
	dayDir := SystemLogDir + "/" + day

	start := time.Date(2026, 10, 28, 23, 58, 30, 0, time.UTC)
	scanFrom := persistedLogScanStart(start)
	// Files whose minute is at or after the scan start minute, for three filers.
	want := 3 * (24*60 - (scanFrom.Hour()*60 + scanFrom.Minute()))

	t.Run("CollectLogFileRefs", func(t *testing.T) {
		store.yields = map[string]int{}
		refs, _, err := f.CollectLogFileRefs(context.Background(), log_buffer.NewMessagePosition(start.UnixNano(), 0), 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != want {
			t.Fatalf("refs = %d, want %d", len(refs), want)
		}
		if got := store.yields[dayDir]; got != want {
			t.Fatalf("listed %d minute files from %s, want %d", got, dayDir, want)
		}
	})

	t.Run("collectPersistedLogBuffer", func(t *testing.T) {
		store.yields = map[string]int{}
		visitor, err := f.collectPersistedLogBuffer(log_buffer.NewMessagePosition(start.UnixNano(), 0), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer visitor.Close()
		if got := store.yields[dayDir]; got != want {
			t.Fatalf("listed %d minute files from %s, want %d", got, dayDir, want)
		}
	})
}

func TestPersistedLogReadsUseUTCRegardlessOfStartPositionTimezone(t *testing.T) {
	f, _ := newTestFilerWithStubStore()

	// Log files on disk are always written under UTC day and hour-minute paths
	// (see logFlushFunc). Seed a file at 2026-10-28 16:00 UTC.
	utcTime := time.Date(2026, 10, 28, 16, 0, 30, 0, time.UTC)
	entryTsNs := utcTime.Add(10 * time.Second).UnixNano()

	ctx := context.Background()
	for _, dir := range []string{SystemLogDir, SystemLogDir + "/2026-10-28"} {
		if err := f.Store.InsertEntry(ctx, &Entry{FullPath: util.FullPath(dir), Attr: Attr{Mode: 0o755 | 1<<31}}); err != nil {
			t.Fatal(err)
		}
	}
	chunkId := "1,016-00.filerA"
	if err := f.Store.InsertEntry(ctx, &Entry{
		FullPath: util.NewFullPath(SystemLogDir+"/2026-10-28", "16-00.filerA"),
		Chunks:   []*filer_pb.FileChunk{{FileId: chunkId, Size: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	origLoad := loadLogFileEntriesFn
	loadLogFileEntriesFn = func(_ *wdclient.MasterClient, c *filer_pb.FileChunk) ([]*filer_pb.LogEntry, bool, error) {
		if c.GetFileIdString() == chunkId {
			return []*filer_pb.LogEntry{{TsNs: entryTsNs}}, true, nil
		}
		return nil, true, nil
	}
	defer func() { loadLogFileEntriesFn = origLoad }()

	// In UTC+9, 2026-10-28 16:00:30 UTC is 2026-10-29 01:00:30 +09:00 (next calendar day).
	// In UTC+2, 2026-10-28 16:00:30 UTC is 2026-10-28 18:00:30 +02:00 (same day, later hour).
	for _, loc := range []*time.Location{
		time.FixedZone("UTC+9", 9*3600),
		time.FixedZone("UTC+2", 2*3600),
		time.FixedZone("UTC-7", -7*3600),
	} {
		pos := log_buffer.MessagePosition{Time: utcTime.In(loc)}

		has, err := f.HasPersistedLogFiles(pos)
		if err != nil {
			t.Fatalf("%s HasPersistedLogFiles: %v", loc, err)
		}
		if !has {
			t.Fatalf("%s HasPersistedLogFiles = false, want true for 2026-10-28 UTC", loc)
		}

		refs, _, err := f.CollectLogFileRefs(ctx, pos, 0)
		if err != nil {
			t.Fatalf("%s CollectLogFileRefs: %v", loc, err)
		}
		if len(refs) != 1 {
			t.Fatalf("%s CollectLogFileRefs = %d refs, want 1", loc, len(refs))
		}

		visitor, err := f.collectPersistedLogBuffer(pos, 0)
		if err != nil {
			t.Fatalf("%s collectPersistedLogBuffer: %v", loc, err)
		}
		entry, err := visitor.GetNext()
		visitor.Close()
		if err != nil || entry.GetTsNs() != entryTsNs {
			t.Fatalf("%s visitor.GetNext() = (%v, %v), want TsNs=%d", loc, entry, err, entryTsNs)
		}
	}
}
