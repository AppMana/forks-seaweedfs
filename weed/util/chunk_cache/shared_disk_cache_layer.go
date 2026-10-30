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

	mu              sync.Mutex
	scopeEntries    map[string]*sharedChunkMeta
	scopeUsedBytes  int64
	wholeUsedBytes  int64
	writesSinceScan int
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
	}
	layer.scanWholeCacheLocked()
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

func (l *SharedDiskCacheLayer) recordScopeEntry(relPath string, size int64, now time.Time, newlyWritten bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if prev, ok := l.scopeEntries[relPath]; ok {
		l.scopeUsedBytes -= prev.size
	} else if newlyWritten {
		l.wholeUsedBytes += size
	}
	l.scopeEntries[relPath] = &sharedChunkMeta{
		relPath:    relPath,
		size:       size,
		lastAccess: now,
	}
	l.scopeUsedBytes += size
	l.writesSinceScan++

	// Enforce per-scope quota first so a single noisy scope evicts its own
	// oldest chunks before contending on the shared whole-cache space.
	if l.scopeLimitBytes > 0 && l.scopeUsedBytes > l.scopeLimitBytes {
		l.evictScopeLocked(l.scopeLimitBytes)
	}

	if l.wholeLimitBytes > 0 && (l.wholeUsedBytes > l.wholeLimitBytes || l.wholeLimitBytes < 64*1024*1024 || l.writesSinceScan >= 16) {
		l.scanWholeCacheLocked()
		if l.wholeUsedBytes > l.wholeLimitBytes {
			l.evictWholeCacheLocked(l.wholeLimitBytes)
		}
	}
}

func (l *SharedDiskCacheLayer) evictScopeLocked(targetBytes int64) {
	if l.scopeUsedBytes <= targetBytes {
		return
	}
	metas := make([]*sharedChunkMeta, 0, len(l.scopeEntries))
	for _, m := range l.scopeEntries {
		metas = append(metas, m)
	}
	slices.SortFunc(metas, func(a, b *sharedChunkMeta) int {
		return a.lastAccess.Compare(b.lastAccess)
	})
	for _, m := range metas {
		if l.scopeUsedBytes <= targetBytes {
			break
		}
		fullPath := filepath.Join(l.dir, m.relPath)
		if err := os.Remove(fullPath); err == nil || os.IsNotExist(err) {
			delete(l.scopeEntries, m.relPath)
			l.scopeUsedBytes -= m.size
			l.wholeUsedBytes = max(0, l.wholeUsedBytes-m.size)
		}
	}
}

func (l *SharedDiskCacheLayer) scanWholeCacheLocked() {
	l.writesSinceScan = 0
	var total int64
	shards, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		shardPath := filepath.Join(l.dir, shard.Name())
		entries, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
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
			if info, err := e.Info(); err == nil {
				total += info.Size()
			}
		}
	}
	l.wholeUsedBytes = total
}

func (l *SharedDiskCacheLayer) evictWholeCacheLocked(targetBytes int64) {
	type diskFileEntry struct {
		relPath string
		size    int64
		modTime time.Time
	}
	var allFiles []diskFileEntry
	var total int64

	shards, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		shardPath := filepath.Join(l.dir, shard.Name())
		entries, err := os.ReadDir(shardPath)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !isManagedChunkPath(shard.Name(), e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			relPath := filepath.Join(shard.Name(), e.Name())
			allFiles = append(allFiles, diskFileEntry{
				relPath: relPath,
				size:    info.Size(),
				modTime: info.ModTime(),
			})
			total += info.Size()
		}
	}
	l.wholeUsedBytes = total
	if l.wholeUsedBytes <= targetBytes {
		return
	}

	slices.SortFunc(allFiles, func(a, b diskFileEntry) int {
		return a.modTime.Compare(b.modTime)
	})
	for _, f := range allFiles {
		if l.wholeUsedBytes <= targetBytes {
			break
		}
		fullPath := filepath.Join(l.dir, f.relPath)
		if err := os.Remove(fullPath); err == nil || os.IsNotExist(err) {
			l.wholeUsedBytes = max(0, l.wholeUsedBytes-f.size)
			if meta, ok := l.scopeEntries[f.relPath]; ok {
				l.scopeUsedBytes = max(0, l.scopeUsedBytes-meta.size)
				delete(l.scopeEntries, f.relPath)
			}
		}
	}
}

func (l *SharedDiskCacheLayer) shutdown() {
	// Shared disk files persist across mounts/restarts; nothing to close.
}
