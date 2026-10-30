package filer

import (
	"context"
	"fmt"
	"io"
	"slices"
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

// cappedListStore mirrors stores such as FoundationDB which enforce a smaller
// maximum than the caller's requested directory-list limit.
type cappedListStore struct {
	*stubFilerStore
	max int64
}

func (s *cappedListStore) ListDirectoryEntries(ctx context.Context, dirPath util.FullPath, startFileName string, includeStartFile bool, limit int64, each ListEachEntryFunc) (string, error) {
	if limit > s.max {
		limit = s.max
	}
	return s.stubFilerStore.ListDirectoryEntries(ctx, dirPath, startFileName, includeStartFile, limit, each)
}

func (s *cappedListStore) ListDirectoryPrefixedEntries(ctx context.Context, dirPath util.FullPath, startFileName string, includeStartFile bool, limit int64, prefix string, each ListEachEntryFunc) (string, error) {
	if limit > s.max {
		limit = s.max
	}
	return s.stubFilerStore.ListDirectoryPrefixedEntries(ctx, dirPath, startFileName, includeStartFile, limit, prefix, each)
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

// seedFullDayOfLogs writes one minute file per minute per filer for a whole day
// and returns the map of chunkID -> entries along with all seeded timestamps.
// Each minute file holds a window starting 50s into that minute and spanning
// 50s (within LogFlushInterval) into the next minute.
func seedFullDayOfLogs(t *testing.T, f *Filer, day string, filerIds ...string) (map[string][]*filer_pb.LogEntry, []int64) {
	t.Helper()
	ctx := context.Background()
	dayStart, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{SystemLogDir, SystemLogDir + "/" + day} {
		if err := f.Store.InsertEntry(ctx, &Entry{FullPath: util.FullPath(dir), Attr: Attr{Mode: 0o755 | 1<<31}}); err != nil {
			t.Fatal(err)
		}
	}
	chunksByID := make(map[string][]*filer_pb.LogEntry, 24*60*len(filerIds))
	var allTsNs []int64
	for m := 0; m < 24*60; m++ {
		minuteStart := dayStart.Add(time.Duration(m) * time.Minute)
		for i, id := range filerIds {
			name := fmt.Sprintf("%02d-%02d.%s", m/60, m%60, id)
			chunkID := "1,0" + name
			entry := &Entry{
				FullPath: util.NewFullPath(SystemLogDir+"/"+day, name),
				Chunks:   []*filer_pb.FileChunk{{FileId: chunkID, Size: 1}},
			}
			if err := f.Store.InsertEntry(ctx, entry); err != nil {
				t.Fatal(err)
			}
			firstTs := minuteStart.Add(50*time.Second + time.Duration(i)*time.Nanosecond).UnixNano()
			secondTs := minuteStart.Add(100*time.Second + time.Duration(i)*time.Nanosecond).UnixNano()
			chunksByID[chunkID] = []*filer_pb.LogEntry{{TsNs: firstTs}, {TsNs: secondTs}}
			allTsNs = append(allTsNs, firstTs, secondTs)
		}
	}
	slices.Sort(allTsNs)
	return chunksByID, allTsNs
}

// A caught-up subscriber re-reads the persisted log on every flush. The read must
// list only the minute files at or after its scan start, not every file the day
// has accumulated: on a networked store a full-day listing per subscriber per
// flush is megabytes per second that the filer then discards.
func TestPersistedLogReadsListOnlyFromTheScanStartMinute(t *testing.T) {
	stub := newStubFilerStore()
	store := &countingListStore{stubFilerStore: stub, yields: map[string]int{}}
	f, _ := newTestFilerWithStubStore(t)
	f.Store = NewFilerStoreWrapper(store)

	day := "2026-10-28"
	chunksByID, allTsNs := seedFullDayOfLogs(t, f, day, "filerA", "filerB", "filerC")
	dayDir := SystemLogDir + "/" + day

	origLoad := loadLogFileEntriesFn
	loadLogFileEntriesFn = func(_ *wdclient.MasterClient, c *filer_pb.FileChunk) ([]*filer_pb.LogEntry, bool, error) {
		return chunksByID[c.GetFileIdString()], true, nil
	}
	defer func() { loadLogFileEntriesFn = origLoad }()

	start := time.Date(2026, 10, 28, 23, 58, 30, 0, time.UTC)
	scanFrom := persistedLogScanStart(start)
	// Files whose minute is at or after the scan start minute, for three filers.
	want := 3 * (24*60 - (scanFrom.Hour()*60 + scanFrom.Minute()))

	var wantTsNs []int64
	for _, ts := range allTsNs {
		if ts > start.UnixNano() {
			wantTsNs = append(wantTsNs, ts)
		}
	}

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
		var gotTsNs []int64
		for _, ref := range refs {
			for _, chunk := range ref.GetChunks() {
				for _, entry := range chunksByID[chunk.GetFileIdString()] {
					if entry.GetTsNs() > start.UnixNano() {
						gotTsNs = append(gotTsNs, entry.GetTsNs())
					}
				}
			}
		}
		slices.Sort(gotTsNs)
		if !slices.Equal(gotTsNs, wantTsNs) {
			t.Fatalf("refs covered %d entries %v, want all %d entries after start %v", len(gotTsNs), gotTsNs, len(wantTsNs), wantTsNs)
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
		var gotTsNs []int64
		for {
			entry, err := visitor.GetNext()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			gotTsNs = append(gotTsNs, entry.GetTsNs())
		}
		if !slices.Equal(gotTsNs, wantTsNs) {
			t.Fatalf("read %d entries %v, want all %d entries after start %v", len(gotTsNs), gotTsNs, len(wantTsNs), wantTsNs)
		}
	})
}

func TestPersistedLogReadsPaginateStoreCappedListings(t *testing.T) {
	stub := newStubFilerStore()
	store := &cappedListStore{stubFilerStore: stub, max: 1000}
	f, _ := newTestFilerWithStubStore(t)
	f.Store = NewFilerStoreWrapper(store)

	day := "2026-10-28"
	chunksByID, allTsNs := seedFullDayOfLogs(t, f, day, "filerA")
	start := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)

	origLoad := loadLogFileEntriesFn
	loadLogFileEntriesFn = func(_ *wdclient.MasterClient, c *filer_pb.FileChunk) ([]*filer_pb.LogEntry, bool, error) {
		return chunksByID[c.GetFileIdString()], true, nil
	}
	defer func() { loadLogFileEntriesFn = origLoad }()

	t.Run("CollectLogFileRefs", func(t *testing.T) {
		refs, _, err := f.CollectLogFileRefs(context.Background(), log_buffer.NewMessagePosition(start.UnixNano(), 0), 0)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := len(refs), 24*60; got != want {
			t.Fatalf("refs = %d, want all %d minute files", got, want)
		}
	})

	t.Run("collectPersistedLogBuffer", func(t *testing.T) {
		visitor, err := f.collectPersistedLogBuffer(log_buffer.NewMessagePosition(start.UnixNano(), 0), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer visitor.Close()

		var got []int64
		for {
			entry, err := visitor.GetNext()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, entry.GetTsNs())
		}
		if !slices.Equal(got, allTsNs) {
			t.Fatalf("read %d entries, want all %d entries", len(got), len(allTsNs))
		}
	})
}

func TestPersistedLogReadsUseUTCRegardlessOfStartPositionTimezone(t *testing.T) {
	f, _ := newTestFilerWithStubStore(t)

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

func TestPersistedLogReadsAcrossDayBoundaries(t *testing.T) {
	t.Run("midnight scan start when previous day has no 23-59 file", func(t *testing.T) {
		f, _ := newTestFilerWithStubStore(t)
		ctx := context.Background()
		for _, dir := range []string{
			SystemLogDir,
			SystemLogDir + "/2026-10-27",
			SystemLogDir + "/2026-10-28",
		} {
			if err := f.Store.InsertEntry(ctx, &Entry{FullPath: util.FullPath(dir), Attr: Attr{Mode: 0o755 | 1<<31}}); err != nil {
				t.Fatal(err)
			}
		}
		// Previous day's last flush was 23-58; no 23-59 file exists on 2026-10-27.
		if err := f.Store.InsertEntry(ctx, &Entry{
			FullPath: util.NewFullPath(SystemLogDir+"/2026-10-27", "23-58.filerA"),
			Chunks:   []*filer_pb.FileChunk{{FileId: "1,0prev", Size: 1}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.Store.InsertEntry(ctx, &Entry{
			FullPath: util.NewFullPath(SystemLogDir+"/2026-10-28", "00-00.filerA"),
			Chunks:   []*filer_pb.FileChunk{{FileId: "1,0curr", Size: 1}},
		}); err != nil {
			t.Fatal(err)
		}

		start := time.Date(2026, 10, 28, 0, 0, 30, 0, time.UTC)
		wantTsNs := time.Date(2026, 10, 28, 0, 0, 45, 0, time.UTC).UnixNano()

		origLoad := loadLogFileEntriesFn
		loadLogFileEntriesFn = func(_ *wdclient.MasterClient, c *filer_pb.FileChunk) ([]*filer_pb.LogEntry, bool, error) {
			if c.GetFileIdString() == "1,0curr" {
				return []*filer_pb.LogEntry{{TsNs: wantTsNs}}, true, nil
			}
			return nil, true, nil
		}
		defer func() { loadLogFileEntriesFn = origLoad }()

		visitor, err := f.collectPersistedLogBuffer(log_buffer.NewMessagePosition(start.UnixNano(), 0), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer visitor.Close()

		entry, err := visitor.GetNext()
		if err != nil || entry.GetTsNs() != wantTsNs {
			t.Fatalf("GetNext() = (%v, %v), want TsNs=%d", entry, err, wantTsNs)
		}
	})

	t.Run("filer absent on intermediate day resumes on later day", func(t *testing.T) {
		f, _ := newTestFilerWithStubStore(t)
		ctx := context.Background()
		for _, dir := range []string{
			SystemLogDir,
			SystemLogDir + "/2026-10-27",
			SystemLogDir + "/2026-10-28",
			SystemLogDir + "/2026-10-29",
		} {
			if err := f.Store.InsertEntry(ctx, &Entry{FullPath: util.FullPath(dir), Attr: Attr{Mode: 0o755 | 1<<31}}); err != nil {
				t.Fatal(err)
			}
		}

		type seededFile struct {
			day, name, chunkId string
			tsNs               int64
		}
		tA1 := time.Date(2026, 10, 27, 10, 0, 10, 0, time.UTC).UnixNano()
		tB1 := time.Date(2026, 10, 27, 10, 0, 20, 0, time.UTC).UnixNano()
		tB2 := time.Date(2026, 10, 27, 10, 1, 20, 0, time.UTC).UnixNano()
		tB3 := time.Date(2026, 10, 28, 10, 0, 20, 0, time.UTC).UnixNano()
		tB4 := time.Date(2026, 10, 28, 10, 1, 20, 0, time.UTC).UnixNano()
		tA2 := time.Date(2026, 10, 29, 10, 0, 10, 0, time.UTC).UnixNano()

		files := []seededFile{
			{"2026-10-27", "10-00.filerA", "1,0a1", tA1},
			{"2026-10-27", "10-00.filerB", "1,0b1", tB1},
			{"2026-10-27", "10-01.filerB", "1,0b2", tB2},
			{"2026-10-28", "10-00.filerB", "1,0b3", tB3},
			{"2026-10-28", "10-01.filerB", "1,0b4", tB4},
			{"2026-10-29", "10-00.filerA", "1,0a2", tA2},
		}
		byChunk := map[string]int64{}
		for _, sf := range files {
			byChunk[sf.chunkId] = sf.tsNs
			if err := f.Store.InsertEntry(ctx, &Entry{
				FullPath: util.NewFullPath(SystemLogDir+"/"+sf.day, sf.name),
				Chunks:   []*filer_pb.FileChunk{{FileId: sf.chunkId, Size: 1}},
			}); err != nil {
				t.Fatal(err)
			}
		}

		origLoad := loadLogFileEntriesFn
		loadLogFileEntriesFn = func(_ *wdclient.MasterClient, c *filer_pb.FileChunk) ([]*filer_pb.LogEntry, bool, error) {
			if ts, ok := byChunk[c.GetFileIdString()]; ok {
				return []*filer_pb.LogEntry{{TsNs: ts}}, true, nil
			}
			return nil, true, nil
		}
		defer func() { loadLogFileEntriesFn = origLoad }()

		start := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
		visitor, err := f.collectPersistedLogBuffer(log_buffer.NewMessagePosition(start.UnixNano(), 0), 0)
		if err != nil {
			t.Fatal(err)
		}
		defer visitor.Close()

		var got []int64
		for {
			entry, err := visitor.GetNext()
			if err != nil {
				break
			}
			got = append(got, entry.GetTsNs())
		}
		want := []int64{tA1, tB1, tB2, tB3, tB4, tA2}
		if len(got) != len(want) {
			t.Fatalf("got %d entries %v, want %d %v", len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("entry[%d] = %d, want %d", i, got[i], want[i])
			}
		}
	})
}
