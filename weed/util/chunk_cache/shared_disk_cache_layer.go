package chunk_cache

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
)

const mtimeTouchInterval = 30 * time.Second

type sharedChunkMeta struct {
	relPath    string
	size       int64
	lastAccess time.Time
}

// SharedDiskCacheLayer is a multi-process-safe, content-addressed on-disk
// chunk cache that allows multiple mounts and gateway modalities (CSI mounts,
// S3, Filer HTTP, WebDAV) on the same node to share cached chunks without
// LevelDB lock contention, while enforcing both a per-scope (per-mount /
// per-modality) disk quota and a whole-cache (node-wide) disk quota.
type SharedDiskCacheLayer struct {
	dir             string
	scopeID         string
	scopeLimitBytes int64
	wholeLimitBytes int64

	mu                 sync.Mutex
	scopeEntries       map[string]*sharedChunkMeta
	scopeUsedBytes     int64
	wholeUsedBytes     int64
	reservedBytes      int64
	writers            int
	maintenancePending bool
	maintenanceRunning bool
	closed             bool
	lastScan           time.Time
	retryAfter         time.Time
	work               sync.WaitGroup
	readDir            func(string) ([]os.DirEntry, error)
}

func NewSharedDiskCacheLayer(dir string, scopeID string, scopeLimitBytes int64, wholeLimitBytes int64) *SharedDiskCacheLayer {
	if wholeLimitBytes <= 0 || (scopeLimitBytes > 0 && wholeLimitBytes < scopeLimitBytes) {
		wholeLimitBytes = scopeLimitBytes
	}
	if scopeLimitBytes <= 0 {
		scopeLimitBytes = wholeLimitBytes
	}
	_ = os.MkdirAll(dir, 0755)
	layer := &SharedDiskCacheLayer{
		dir:             dir,
		scopeID:         scopeID,
		scopeLimitBytes: scopeLimitBytes,
		wholeLimitBytes: wholeLimitBytes,
		scopeEntries:    make(map[string]*sharedChunkMeta),
		readDir:         os.ReadDir,
	}
	_, layer.wholeUsedBytes, _ = layer.diskEntries()
	layer.lastScan = time.Now()
	return layer
}

func (l *SharedDiskCacheLayer) chunkRelPath(fid *needle.FileId) string {
	shard := fmt.Sprintf("%02x", uint8(fid.Key))
	name := fmt.Sprintf("%d_%x_%x.dat", fid.VolumeId, uint64(fid.Key), uint32(fid.Cookie))
	return filepath.Join(shard, name)
}

func (l *SharedDiskCacheLayer) chunkFullPath(fid *needle.FileId) string {
	return filepath.Join(l.dir, l.chunkRelPath(fid))
}

// Only the canonical files emitted by this cache belong to its eviction domain.
// In particular, legacy indexed cache volumes must never be treated as chunks.
func isManagedChunkPath(shard, name string) bool {
	if !strings.HasSuffix(name, ".dat") {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(name, ".dat"), "_")
	if len(parts) != 3 {
		return false
	}
	volume, e1 := strconv.ParseUint(parts[0], 10, 32)
	key, e2 := strconv.ParseUint(parts[1], 16, 64)
	cookie, e3 := strconv.ParseUint(parts[2], 16, 32)
	return e1 == nil && e2 == nil && e3 == nil &&
		shard == fmt.Sprintf("%02x", key&0xff) &&
		name == fmt.Sprintf("%d_%x_%x.dat", volume, key, cookie)
}

func (l *SharedDiskCacheLayer) hasChunk(fid *needle.FileId) bool {
	fullPath := l.chunkFullPath(fid)
	f, err := os.Open(fullPath)
	if err != nil {
		return false
	}
	defer f.Close()

	header := make([]byte, cacheHeaderSize)
	n, err := f.ReadAt(header, 0)
	if err != nil || n < cacheHeaderSize {
		return false
	}
	volId, cookie := parseCacheHeader(header)
	return volId == fid.VolumeId && cookie == fid.Cookie
}

func (l *SharedDiskCacheLayer) hasScopeEntry(fid *needle.FileId) bool {
	relPath := l.chunkRelPath(fid)
	l.mu.Lock()
	_, inScope := l.scopeEntries[relPath]
	l.mu.Unlock()
	return inScope && l.hasChunk(fid)
}

func (l *SharedDiskCacheLayer) readChunkAt(buffer []byte, fid *needle.FileId, offset uint64) (n int, err error) {
	relPath := l.chunkRelPath(fid)
	fullPath := filepath.Join(l.dir, relPath)
	f, openErr := os.Open(fullPath)
	if openErr != nil {
		if os.IsNotExist(openErr) {
			return 0, storage.ErrorNotFound
		}
		return 0, openErr
	}
	defer f.Close()

	st, statErr := f.Stat()
	if statErr != nil {
		return 0, statErr
	}
	payloadSize := st.Size() - cacheHeaderSize
	if payloadSize <= 0 {
		return 0, nil
	}

	header := make([]byte, cacheHeaderSize)
	headerN, headerErr := f.ReadAt(header, 0)
	if headerErr != nil || headerN < cacheHeaderSize {
		return 0, headerErr
	}
	storedVolumeId, storedCookie := parseCacheHeader(header)
	if storedVolumeId != fid.VolumeId || storedCookie != fid.Cookie {
		return 0, nil
	}

	if offset > uint64(payloadSize) {
		return 0, ErrorOutOfBounds
	}
	wanted := len(buffer)
	if remaining := uint64(payloadSize) - offset; remaining < uint64(wanted) {
		wanted = int(remaining)
	}
	readOffset := int64(cacheHeaderSize) + int64(offset)
	n, err = f.ReadAt(buffer[:wanted], readOffset)
	if err != nil && n == wanted {
		err = nil
	}

	now := time.Now()
	l.mu.Lock()
	if meta, ok := l.scopeEntries[relPath]; ok {
		meta.lastAccess = now
	}
	l.mu.Unlock()
	if now.Sub(st.ModTime()) >= mtimeTouchInterval {
		_ = os.Chtimes(fullPath, now, now)
	}
	return n, err
}

func (l *SharedDiskCacheLayer) setChunk(fid *needle.FileId, dataWithHeader []byte) {
	size := int64(len(dataWithHeader))
	if l.scopeLimitBytes > 0 && size > l.scopeLimitBytes {
		return
	}

	if !l.beginWrite(size) {
		return
	}
	defer l.endWrite(size)

	relPath := l.chunkRelPath(fid)
	fullPath := filepath.Join(l.dir, relPath)
	shardDir := filepath.Dir(fullPath)
	if err := os.MkdirAll(shardDir, 0755); err != nil {
		glog.V(1).Infof("shared chunk cache mkdir %s: %v", shardDir, err)
		return
	}

	// If another process already wrote the exact chunk, adopt/touch it without
	// rewriting the bytes on disk.
	if st, err := os.Stat(fullPath); err == nil && st.Size() == size && l.hasChunk(fid) {
		now := time.Now()
		_ = os.Chtimes(fullPath, now, now)
		l.recordScopeEntry(relPath, size, now, false)
		return
	}

	tmpPath := fmt.Sprintf("%s.tmp.%d.%d", fullPath, os.Getpid(), rand.Uint64())
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		glog.V(1).Infof("shared chunk cache create %s: %v", tmpPath, err)
		return
	}
	written, writeErr := f.Write(dataWithHeader)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil || int64(written) != size {
		_ = os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, fullPath); err != nil {
		_ = os.Remove(tmpPath)
		return
	}

	l.recordScopeEntry(relPath, size, time.Now(), true)
}

// Cache persistence is best effort while maintenance is pending. Reservations
// bound concurrent writers without making readers wait for directory I/O.
func (l *SharedDiskCacheLayer) beginWrite(size int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.maintenancePending {
		return false
	}
	if cacheAtLimit(l.scopeUsedBytes+l.reservedBytes, l.scopeLimitBytes) ||
		cacheAtLimit(l.wholeUsedBytes+l.reservedBytes, l.wholeLimitBytes) {
		l.requestMaintenanceLocked()
		return false
	}
	l.writers++
	l.reservedBytes += size
	l.work.Add(1)
	return true
}

func (l *SharedDiskCacheLayer) endWrite(size int64) {
	l.mu.Lock()
	l.writers--
	l.reservedBytes -= size
	l.startMaintenanceLocked()
	l.mu.Unlock()
	l.work.Done()
}

func (l *SharedDiskCacheLayer) recordScopeEntry(relPath string, size int64, now time.Time, newlyWritten bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if prev, ok := l.scopeEntries[relPath]; ok {
		l.scopeUsedBytes -= prev.size
	} else if newlyWritten {
		l.wholeUsedBytes += size
	}
	l.scopeEntries[relPath] = &sharedChunkMeta{relPath: relPath, size: size, lastAccess: now}
	l.scopeUsedBytes += size
	if cacheAtLimit(l.scopeUsedBytes, l.scopeLimitBytes) || cacheAtLimit(l.wholeUsedBytes, l.wholeLimitBytes) || time.Since(l.lastScan) >= 30*time.Second {
		l.requestMaintenanceLocked()
	}
}

func (l *SharedDiskCacheLayer) requestMaintenanceLocked() {
	if l.closed || time.Now().Before(l.retryAfter) {
		return
	}
	l.maintenancePending = true
	l.startMaintenanceLocked()
}

func (l *SharedDiskCacheLayer) startMaintenanceLocked() {
	if l.closed || !l.maintenancePending || l.maintenanceRunning || l.writers != 0 {
		return
	}
	l.maintenanceRunning = true
	l.work.Add(1)
	go func() {
		defer l.work.Done()
		err := l.maintain()
		l.mu.Lock()
		l.maintenancePending = false
		l.maintenanceRunning = false
		l.lastScan = time.Now()
		if err != nil || cacheAtLimit(l.scopeUsedBytes, l.scopeLimitBytes) || cacheAtLimit(l.wholeUsedBytes, l.wholeLimitBytes) {
			l.retryAfter = time.Now().Add(time.Second)
		}
		l.mu.Unlock()
	}()
}

type sharedDiskEntry struct {
	relPath string
	size    int64
	modTime time.Time
}

// One inventory supplies both accounting and eviction candidates. An incomplete
// inventory must not replace the last known usage with an undercount.
func (l *SharedDiskCacheLayer) diskEntries() ([]sharedDiskEntry, int64, error) {
	var files []sharedDiskEntry
	var total int64
	shards, err := l.readDir(l.dir)
	if err != nil {
		return nil, 0, err
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		shardPath := filepath.Join(l.dir, shard.Name())
		entries, err := l.readDir(shardPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			name := e.Name()
			if stem, _, temporary := strings.Cut(name, ".tmp."); temporary && isManagedChunkPath(shard.Name(), stem) {
				if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 5*time.Minute {
					_ = os.Remove(filepath.Join(shardPath, name))
				}
				continue
			}
			if !isManagedChunkPath(shard.Name(), name) {
				continue
			}
			info, err := e.Info()
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, 0, err
			}
			files = append(files, sharedDiskEntry{filepath.Join(shard.Name(), name), info.Size(), info.ModTime()})
			total += info.Size()
		}
	}
	return files, total, nil
}

func cacheAtLimit(used, limit int64) bool { return limit > 0 && used >= limit }

func cacheLowWatermark(limit int64) int64 { return limit - limit/10 }

func (l *SharedDiskCacheLayer) maintain() error {
	started := time.Now()
	files, total, err := l.diskEntries()
	if err != nil {
		return err
	}
	present := make(map[string]int64, len(files))
	for _, f := range files {
		present[f.relPath] = f.size
	}
	l.mu.Lock()
	l.wholeUsedBytes = total
	var scope []sharedDiskEntry
	l.scopeUsedBytes = 0
	for path, meta := range l.scopeEntries {
		if size, ok := present[path]; ok {
			meta.size = size
			l.scopeUsedBytes += size
			scope = append(scope, sharedDiskEntry{path, size, meta.lastAccess})
		} else {
			delete(l.scopeEntries, path)
		}
	}
	scopeFull := cacheAtLimit(l.scopeUsedBytes, l.scopeLimitBytes)
	wholeFull := cacheAtLimit(total, l.wholeLimitBytes)
	l.mu.Unlock()
	if scopeFull {
		slices.SortFunc(scope, func(a, b sharedDiskEntry) int { return a.modTime.Compare(b.modTime) })
		l.evict(scope, cacheLowWatermark(l.scopeLimitBytes), true, started)
	}
	if wholeFull {
		slices.SortFunc(files, func(a, b sharedDiskEntry) int { return a.modTime.Compare(b.modTime) })
		l.evict(files, cacheLowWatermark(l.wholeLimitBytes), false, started)
	}
	return nil
}

func (l *SharedDiskCacheLayer) evict(files []sharedDiskEntry, target int64, scopeOnly bool, started time.Time) {
	for _, f := range files {
		l.mu.Lock()
		used := l.wholeUsedBytes
		if scopeOnly {
			used = l.scopeUsedBytes
		}
		meta := l.scopeEntries[f.relPath]
		touched := meta != nil && meta.lastAccess.After(started)
		l.mu.Unlock()
		if used <= target {
			return
		}
		if touched {
			continue
		}
		path := filepath.Join(l.dir, f.relPath)
		info, err := os.Stat(path)
		if err == nil && (info.Size() != f.size || info.ModTime().After(f.modTime)) {
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			continue
		}
		// A previous scope pass may already have removed this candidate.
		if os.IsNotExist(err) {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			continue
		}
		l.mu.Lock()
		l.wholeUsedBytes = max(0, l.wholeUsedBytes-f.size)
		if meta := l.scopeEntries[f.relPath]; meta != nil {
			l.scopeUsedBytes = max(0, l.scopeUsedBytes-meta.size)
			delete(l.scopeEntries, f.relPath)
		}
		l.mu.Unlock()
	}
}

func (l *SharedDiskCacheLayer) shutdown() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.work.Wait()
}
