package chunk_cache

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/storage/idx"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// MigrateLegacyCache copies a quiescent legacy cache directory into this shared
// cache. It never deletes or opens source data/index files for writing. All
// legacy LevelDB locks are acquired before copying, so an active old mount is
// rejected rather than racing cache rotation. Callers must retain the source
// until deployment acceptance; partial imports are safe to retry.
// maxBytes bounds copied payload plus headers, not the retained source size.
func (c *TieredChunkCache) MigrateLegacyCache(ctx context.Context, source string, maxBytes int64) (int64, error) {
	if c == nil || c.sharedDisk == nil || maxBytes <= 0 {
		return 0, fmt.Errorf("shared cache and positive migration budget required")
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return 0, err
	}
	var bases []string
	var locks []*leveldb.DB
	defer func() {
		for _, db := range locks {
			_ = db.Close()
		}
	}()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".idx") {
			continue
		}
		base := strings.TrimSuffix(name, ".idx")
		parts := strings.Split(base, "_")
		if len(parts) != 3 || !(parts[0] == "c0" && parts[1] == "2" || parts[0] == "c1" && parts[1] == "3" || parts[0] == "c2" && parts[1] == "2") {
			continue
		}
		n, parseErr := strconv.ParseUint(parts[2], 10, 32)
		if parseErr != nil || strconv.FormatUint(n, 10) != parts[2] {
			continue
		}
		base = filepath.Join(source, base)
		db, openErr := leveldb.OpenFile(base+".ldb", &opt.Options{ReadOnly: true, ErrorIfMissing: true})
		if openErr != nil {
			return 0, fmt.Errorf("legacy cache must be quiescent and readable: %s: %w", base, openErr)
		}
		locks = append(locks, db)
		bases = append(bases, base)
	}
	if len(bases) == 0 {
		return 0, fmt.Errorf("no recognized legacy cache segments in %s", source)
	}
	var copied int64
	for _, base := range bases {
		n, copyErr := c.migrateLegacySegment(ctx, base, maxBytes-copied)
		copied += n
		if copyErr != nil {
			return copied, copyErr
		}
	}
	return copied, nil
}

func (c *TieredChunkCache) migrateLegacySegment(ctx context.Context, base string, budget int64) (int64, error) {
	index, err := os.Open(base + ".idx")
	if err != nil {
		return 0, err
	}
	defer index.Close()
	stat, err := index.Stat()
	if err != nil {
		return 0, err
	}
	// Bound the index scan and memory even if the source is malformed.
	if stat.Size()%types.NeedleMapEntrySize != 0 || stat.Size()/types.NeedleMapEntrySize > 1_000_000 {
		return 0, fmt.Errorf("invalid or oversized legacy index %s", base)
	}
	type location struct {
		offset types.Offset
		size   types.Size
	}
	latest := make(map[types.NeedleId]location)
	err = idx.WalkIndexFile(index, 0, func(key types.NeedleId, offset types.Offset, size types.Size) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		latest[key] = location{offset, size}
		return nil
	})
	if err != nil {
		return 0, err
	}
	data, err := os.Open(base + ".dat")
	if err != nil {
		return 0, err
	}
	defer data.Close()
	var copied int64
	for key, loc := range latest {
		if err := ctx.Err(); err != nil {
			return copied, err
		}
		if loc.size <= 0 {
			continue
		}
		size := int64(loc.size)
		if size < cacheHeaderSize || size > 64<<20 || size > budget-copied {
			return copied, fmt.Errorf("legacy chunk exceeds migration bounds")
		}
		payload := make([]byte, int(size))
		if _, err := data.ReadAt(payload, loc.offset.ToActualOffset()); err != nil {
			return copied, err
		}
		volume, cookie := parseCacheHeader(payload)
		fid := &needle.FileId{VolumeId: volume, Key: key, Cookie: cookie}
		c.sharedDisk.setChunk(fid, payload)
		got := make([]byte, len(payload)-cacheHeaderSize)
		n, err := c.sharedDisk.readChunkAt(got, fid, 0)
		if err != nil || n != len(got) || !bytes.Equal(got, payload[cacheHeaderSize:]) {
			return copied, fmt.Errorf("migrated chunk verification failed: %s", fid.String())
		}
		copied += size
	}
	return copied, nil
}
