package storage

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/syndtr/goleveldb/leveldb/opt"

	"github.com/seaweedfs/seaweedfs/weed/storage/idx"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/util"

	"github.com/syndtr/goleveldb/leveldb"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	. "github.com/seaweedfs/seaweedfs/weed/storage/types"
)

// mark it every watermarkBatchSize operations
const watermarkBatchSize = 10000

var watermarkKey = []byte("idx_entry_watermark")

type LevelDbNeedleMap struct {
	baseNeedleMapper
	dbFileName    string
	db            *leveldb.DB
	ldbOpts       *opt.Options
	ldbAccessLock sync.RWMutex
	// Serialize the complete index/LevelDB mutation, not just the index append.
	// Otherwise concurrent updates can publish a different order to each store
	// and advance the replay watermark past a mutation not yet in LevelDB.
	mutationLock sync.Mutex
	exitChan     chan bool
	// no need to use atomic
	accessFlag  int64
	ldbTimeout  int64
	recordCount uint64
}

func NewLevelDbNeedleMap(dbFileName string, indexFile *os.File, opts *opt.Options, ldbTimeout int64, version needle.Version) (m *LevelDbNeedleMap, err error) {
	m = &LevelDbNeedleMap{dbFileName: dbFileName}
	m.indexFile = indexFile
	if stat, err := indexFile.Stat(); err != nil {
		return nil, fmt.Errorf("stat index %s: %w", indexFile.Name(), err)
	} else {
		m.indexFileOffset = stat.Size()
	}
	glog.V(1).Infof("Opening %s...", dbFileName)

	if m.ldbTimeout == 0 {
		if m.db, err = leveldb.OpenFile(dbFileName, opts); err != nil {
			if errors.IsCorrupted(err) {
				m.db, err = leveldb.RecoverFile(dbFileName, opts)
			}
			if err != nil {
				return
			}
		}
		// LOG is diagnostic output, not a durable index checkpoint. Its mtime
		// may advance independently of the WAL, including during compaction.
		// Always replay the authoritative index tail from the stored watermark.
		// Reuse this open DB rather than opening/closing it a second time.
		if err = replayLevelDbIndex(m.db, dbFileName, indexFile); err != nil {
			_ = m.db.Close()
			return nil, fmt.Errorf("replay index %s: %w", indexFile.Name(), err)
		}
		glog.V(1).Infof("Loading %s... , watermark: %d", dbFileName, getWatermark(m.db))
		m.recordCount = uint64(m.indexFileOffset / NeedleMapEntrySize)
		watermark := (m.recordCount / watermarkBatchSize) * watermarkBatchSize
		err = setWatermark(m.db, watermark)
		if err != nil {
			_ = m.db.Close()
			return nil, err
		}
	}
	mm, indexLoadError := newNeedleMapMetricFromIndexFile(indexFile, version)
	if indexLoadError != nil {
		_ = m.db.Close()
		return nil, indexLoadError
	}
	m.mapMetric = *mm
	m.ldbTimeout = ldbTimeout
	if m.ldbTimeout > 0 {
		m.ldbOpts = opts
		m.exitChan = make(chan bool, 1)
		m.accessFlag = 0
		go lazyLoadingRoutine(m)
	}
	return
}

func generateLevelDbFile(dbFileName string, indexFile *os.File) (err error) {
	db, err := leveldb.OpenFile(dbFileName, nil)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); err == nil {
			err = closeErr
		}
	}()
	return replayLevelDbIndex(db, dbFileName, indexFile)
}

func replayLevelDbIndex(db *leveldb.DB, dbFileName string, indexFile *os.File) error {
	watermark := getWatermark(db)
	if stat, err := indexFile.Stat(); err != nil {
		return fmt.Errorf("stat index %s: %w", indexFile.Name(), err)
	} else {
		// A watermark past the end of the .idx means the .ldb is stale relative
		// to the index it must mirror (e.g. an interrupted compaction left the
		// old .ldb beside a freshly swapped, shorter .idx). Trusting it would
		// replay zero entries and silently poison the needle map, so rebuild
		// from offset 0 instead.
		// Compare in entries, not bytes: watermark*NeedleMapEntrySize can
		// overflow uint64 for a corrupted watermark and wrap past the size check.
		if watermark > uint64(stat.Size())/NeedleMapEntrySize {
			glog.Warningf("stale watermark %d for %s (filesize %d); rebuilding leveldb from start", watermark, dbFileName, stat.Size())
			watermark = 0
		}
		glog.V(1).Infof("generateLevelDbFile %s, watermark %d, num of entries:%d", dbFileName, watermark, (uint64(stat.Size())-watermark*NeedleMapEntrySize)/NeedleMapEntrySize)
	}
	// Keep replay bounded in memory and avoid a WAL record/transaction per
	// entry. Do not advance the watermark until the whole replay succeeds.
	type replayValue struct {
		offset Offset
		size   Size
	}
	pending := make(map[NeedleId]replayValue)
	records := 0
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		keys := make([]NeedleId, 0, len(pending))
		for key := range pending {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		// One iterator amortizes lookup setup across the bounded batch. Seek
		// directly to requested keys, never scan unrelated database ranges.
		iterator := db.NewIterator(nil, nil)
		defer iterator.Release()
		batch := new(leveldb.Batch)
		positioned, valid := false, false
		for _, key := range keys {
			value := pending[key]
			entry := needle_map.ToBytes(key, value.offset, value.size)
			if !positioned || (valid && bytes.Compare(iterator.Key(), entry[:NeedleIdSize]) < 0) {
				valid = iterator.Seek(entry[:NeedleIdSize])
				positioned = true
			}
			found := valid && bytes.Equal(iterator.Key(), entry[:NeedleIdSize])
			if err := iterator.Error(); err != nil {
				return err
			}
			same := found && bytes.Equal(iterator.Value(), entry[NeedleIdSize:])
			if found {
				// Adjacent needle IDs need only Next, not another tree seek.
				// Gaps still jump directly, so unrelated ranges are not scanned.
				valid = iterator.Next()
				if err := iterator.Error(); err != nil {
					return err
				}
			}
			live := !value.offset.IsZero() && !value.size.IsDeleted()
			if live {
				if same {
					continue
				}
				batch.Put(entry[:NeedleIdSize], entry[NeedleIdSize:])
			} else if found {
				batch.Delete(entry[:NeedleIdSize])
			}
		}
		if batch.Len() > 0 {
			if err := db.Write(batch, nil); err != nil {
				return err
			}
		}
		clear(pending)
		records = 0
		return nil
	}
	err := idx.WalkIndexFile(indexFile, watermark, func(key NeedleId, offset Offset, size Size) error {
		// The final record for a repeated key wins inside each batch. A fresh
		// iterator in the next batch observes all prior applied mutations.
		pending[key] = replayValue{offset, size}
		records++
		if records >= 4096 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}

func (m *LevelDbNeedleMap) Get(key NeedleId) (element *needle_map.NeedleValue, ok bool) {
	if m.ldbTimeout > 0 {
		if err := m.ensureLdbLoaded(); err != nil {
			return nil, false
		}
		defer m.ldbAccessLock.RUnlock()
	}
	return m.getFromDb(key)
}

func (m *LevelDbNeedleMap) getFromDb(key NeedleId) (element *needle_map.NeedleValue, ok bool) {
	bytes := make([]byte, NeedleIdSize)
	NeedleIdToBytes(bytes[0:NeedleIdSize], key)
	data, err := m.db.Get(bytes, nil)
	if err != nil || len(data) != OffsetSize+SizeSize {
		return nil, false
	}
	offset := BytesToOffset(data[0:OffsetSize])
	size := BytesToSize(data[OffsetSize : OffsetSize+SizeSize])
	return &needle_map.NeedleValue{Key: key, Offset: offset, Size: size}, true
}

func (m *LevelDbNeedleMap) Put(key NeedleId, offset Offset, size Size) error {
	m.mutationLock.Lock()
	defer m.mutationLock.Unlock()
	var oldSize Size
	var watermark uint64
	if m.ldbTimeout > 0 {
		if err := m.ensureLdbLoaded(); err != nil {
			return err
		}
		defer m.ldbAccessLock.RUnlock()
	}
	if oldNeedle, ok := m.getFromDb(key); ok {
		oldSize = oldNeedle.Size
	}
	// write to index file first
	if err := m.appendToIndexFile(key, offset, size); err != nil {
		return fmt.Errorf("cannot write to indexfile %s: %v", m.indexFile.Name(), err)
	}
	m.logPut(key, oldSize, size)
	m.recordCount++
	if m.recordCount%watermarkBatchSize != 0 {
		watermark = 0
	} else {
		watermark = (m.recordCount / watermarkBatchSize) * watermarkBatchSize
		glog.V(1).Infof("put cnt:%d for %s,watermark: %d", m.recordCount, m.dbFileName, watermark)
	}
	return levelDbWrite(m.db, key, offset, size, watermark != 0, watermark)
}

func getWatermark(db *leveldb.DB) uint64 {
	data, err := db.Get(watermarkKey, nil)
	if err != nil || len(data) != 8 {
		glog.V(1).Infof("read previous watermark from db: %v, %d", err, len(data))
		return 0
	}
	return util.BytesToUint64(data)
}

func setWatermark(db *leveldb.DB, watermark uint64) error {
	glog.V(3).Infof("set watermark %d", watermark)
	var wmBytes = make([]byte, 8)
	util.Uint64toBytes(wmBytes, watermark)
	if err := db.Put(watermarkKey, wmBytes, nil); err != nil {
		return fmt.Errorf("failed to setWatermark: %w", err)
	}
	return nil
}

func levelDbWrite(db *leveldb.DB, key NeedleId, offset Offset, size Size, updateWatermark bool, watermark uint64) error {

	bytes := needle_map.ToBytes(key, offset, size)

	if err := db.Put(bytes[0:NeedleIdSize], bytes[NeedleIdSize:NeedleIdSize+OffsetSize+SizeSize], nil); err != nil {
		return fmt.Errorf("failed to write leveldb: %w", err)
	}
	// set watermark
	if updateWatermark {
		return setWatermark(db, watermark)
	}
	return nil
}

func levelDbDelete(db *leveldb.DB, key NeedleId) error {
	bytes := make([]byte, NeedleIdSize)
	NeedleIdToBytes(bytes, key)
	return db.Delete(bytes, nil)
}

func (m *LevelDbNeedleMap) Delete(key NeedleId, offset Offset) error {
	m.mutationLock.Lock()
	defer m.mutationLock.Unlock()
	var watermark uint64
	if m.ldbTimeout > 0 {
		if err := m.ensureLdbLoaded(); err != nil {
			return err
		}
		defer m.ldbAccessLock.RUnlock()
	}
	oldNeedle, found := m.getFromDb(key)
	if !found || oldNeedle.Size.IsDeleted() {
		return nil
	}
	// write to index file first
	if err := m.appendToIndexFile(key, offset, TombstoneFileSize); err != nil {
		return err
	}
	m.logDelete(oldNeedle.Size)
	m.recordCount++
	if m.recordCount%watermarkBatchSize != 0 {
		watermark = 0
	} else {
		watermark = (m.recordCount / watermarkBatchSize) * watermarkBatchSize
	}
	return levelDbWrite(m.db, key, oldNeedle.Offset, -oldNeedle.Size, watermark != 0, watermark)
}

func (m *LevelDbNeedleMap) Close() {
	if m.indexFile != nil {
		indexFileName := m.indexFile.Name()
		if err := m.indexFile.Sync(); err != nil {
			glog.Warningf("sync file %s failed: %v", indexFileName, err)
		}
		if err := m.indexFile.Close(); err != nil {
			glog.Warningf("close index file %s failed: %v", indexFileName, err)
		}
	}

	if m.db != nil {
		if err := m.db.Close(); err != nil {
			glog.Warningf("close levelDB failed: %v", err)
		}
	}
	if m.ldbTimeout > 0 {
		m.exitChan <- true
	}
}

func (m *LevelDbNeedleMap) Destroy() error {
	m.Close()
	os.Remove(m.indexFile.Name())
	return os.RemoveAll(m.dbFileName)
}

func (m *LevelDbNeedleMap) UpdateNeedleMap(v *Volume, indexFile *os.File, opts *opt.Options, ldbTimeout int64) error {
	if v.nm != nil {
		v.nm.Close()
		v.nm = nil
	}
	defer func() {
		if v.tmpNm != nil {
			v.tmpNm.Close()
			v.tmpNm = nil
		}
	}()
	levelDbFile := v.FileName(".ldb")
	m.indexFile = indexFile
	err := os.RemoveAll(levelDbFile)
	if err != nil {
		return err
	}
	if err = os.Rename(v.FileName(".cpldb"), levelDbFile); err != nil {
		return fmt.Errorf("rename %s: %v", levelDbFile, err)
	}

	db, err := leveldb.OpenFile(levelDbFile, opts)
	if err != nil {
		if errors.IsCorrupted(err) {
			db, err = leveldb.RecoverFile(levelDbFile, opts)
		}
		if err != nil {
			return err
		}
	}
	m.db = db

	stat, e := indexFile.Stat()
	if e != nil {
		glog.Fatalf("stat file %s: %v", indexFile.Name(), e)
		return e
	}
	m.indexFileOffset = stat.Size()
	m.recordCount = uint64(stat.Size() / NeedleMapEntrySize)

	//set watermark
	watermark := (m.recordCount / watermarkBatchSize) * watermarkBatchSize
	err = setWatermark(db, uint64(watermark))
	if err != nil {
		glog.Fatalf("setting watermark failed %s: %v", indexFile.Name(), err)
		return err
	}
	v.nm = m
	v.tmpNm = nil
	m.ldbTimeout = ldbTimeout
	if m.ldbTimeout > 0 {
		m.ldbOpts = opts
		m.exitChan = make(chan bool, 1)
		m.accessFlag = 0
		go lazyLoadingRoutine(m)
	}
	return e
}

func (m *LevelDbNeedleMap) DoOffsetLoading(v *Volume, indexFile *os.File, startFrom uint64) (err error) {
	glog.V(0).Infof("loading idx to leveldb from offset %d for file: %s", startFrom, indexFile.Name())
	version := needle.GetCurrentVersion()
	if v != nil {
		version = v.Version()
	}
	dbFileName := v.FileName(".cpldb")
	db, dbErr := leveldb.OpenFile(dbFileName, nil)
	defer func() {
		if dbErr == nil {
			db.Close()
		}
		if err != nil {
			os.RemoveAll(dbFileName)
		}

	}()
	if dbErr != nil {
		if errors.IsCorrupted(dbErr) {
			db, dbErr = leveldb.RecoverFile(dbFileName, nil)
		}
		if dbErr != nil {
			return dbErr
		}
	}

	err = idx.WalkIndexFile(indexFile, startFrom, func(key NeedleId, offset Offset, size Size) (e error) {
		m.mapMetric.FileCounter++
		m.mapMetric.MaybeSetMaxNeedleEnd(offset, size, version)
		bytes := make([]byte, NeedleIdSize)
		NeedleIdToBytes(bytes[0:NeedleIdSize], key)
		// fresh loading
		if startFrom == 0 {
			m.mapMetric.FileByteCounter += uint64(size)
			e = levelDbWrite(db, key, offset, size, false, 0)
			return e
		}
		// increment loading
		data, err := db.Get(bytes, nil)
		if err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "not found") {
				// unexpected error
				return err
			}
			// new needle, unlikely happen
			m.mapMetric.FileByteCounter += uint64(size)
			e = levelDbWrite(db, key, offset, size, false, 0)
		} else {
			// needle is found
			oldSize := BytesToSize(data[OffsetSize : OffsetSize+SizeSize])
			oldOffset := BytesToOffset(data[0:OffsetSize])
			if !offset.IsZero() && !size.IsDeleted() {
				// updated needle
				m.mapMetric.FileByteCounter += uint64(size)
				if !oldOffset.IsZero() && !oldSize.IsDeleted() {
					m.mapMetric.DeletionCounter++
					m.mapMetric.DeletionByteCounter += uint64(oldSize)
				}
				e = levelDbWrite(db, key, offset, size, false, 0)
			} else {
				// deleted needle
				m.mapMetric.DeletionCounter++
				m.mapMetric.DeletionByteCounter += uint64(oldSize)
				e = levelDbDelete(db, key)
			}
		}
		return e
	})
	return err
}

func (m *LevelDbNeedleMap) ensureLdbLoaded() error {
	for {
		m.ldbAccessLock.RLock()
		if m.db != nil {
			return nil
		}
		m.ldbAccessLock.RUnlock()
		m.ldbAccessLock.Lock()
		if m.db == nil {
			if err := reloadLdb(m); err != nil {
				m.ldbAccessLock.Unlock()
				return err
			}
		}
		m.ldbAccessLock.Unlock()
	}
}

func reloadLdb(m *LevelDbNeedleMap) (err error) {
	if m.db != nil {
		return nil
	}
	glog.V(1).Infof("reloading leveldb %s", m.dbFileName)
	m.accessFlag = 1
	if m.db, err = leveldb.OpenFile(m.dbFileName, m.ldbOpts); err != nil {
		if errors.IsCorrupted(err) {
			m.db, err = leveldb.RecoverFile(m.dbFileName, m.ldbOpts)
		}
		if err != nil {
			glog.Fatalf("RecoverFile %s failed:%v", m.dbFileName, err)
			return err
		}
	}
	return nil
}

func unloadLdb(m *LevelDbNeedleMap) (err error) {
	m.ldbAccessLock.Lock()
	defer m.ldbAccessLock.Unlock()
	if m.db != nil {
		glog.V(1).Infof("reached max idle count, unload leveldb, %s", m.dbFileName)
		m.db.Close()
		m.db = nil
	}
	return nil
}

func lazyLoadingRoutine(m *LevelDbNeedleMap) (err error) {
	glog.V(1).Infof("lazyLoadingRoutine %s", m.dbFileName)
	var accessRecord int64
	accessRecord = 1
	for {
		select {
		case exit := <-m.exitChan:
			if exit {
				glog.V(1).Infof("exit from lazyLoadingRoutine")
				return nil
			}
		case <-time.After(time.Hour * 1):
			glog.V(1).Infof("timeout %s", m.dbFileName)
			if m.accessFlag == 0 {
				accessRecord++
				glog.V(1).Infof("accessRecord++")
				if accessRecord >= m.ldbTimeout {
					unloadLdb(m)
				}
			} else {
				glog.V(1).Infof("reset accessRecord %s", m.dbFileName)
				// reset accessRecord
				accessRecord = 0
				m.accessFlag = 0
			}
			continue
		}
	}
}
