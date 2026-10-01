package mount

import (
	"context"
	"errors"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// listDirectoryForAdapter preserves the directory cache bound for path-based
// adapters too. An oversized cache build must read through, never enumerate
// the incomplete local cache as if it were a complete directory.
func (wfs *WFS) listDirectoryForAdapter(ctx context.Context, dir util.FullPath, each filer.ListEachEntryFunc) error {
	err := wfs.ensureDirectoryVisited(dir)
	if err == nil {
		// The cache holds its read lock while visiting entries. Adapter
		// callbacks may read local file handles; Flush holds those handles
		// while updating the cache. Invoke callbacks outside the cache lock
		// to avoid that inversion, retaining only one bounded page.
		const pageSize = 128
		start := ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries := make([]*filer.Entry, 0, pageSize)
			last, err := wfs.metaCache.ListDirectoryEntries(ctx, dir, start, false, pageSize, func(entry *filer.Entry) (bool, error) {
				entries = append(entries, entry)
				return true, nil
			})
			if err != nil {
				return err
			}
			for _, entry := range entries {
				more, err := each(entry)
				if err != nil || !more {
					return err
				}
			}
			if last == "" || last == start {
				return nil
			}
			start = last
		}
	}
	var tooLarge *meta_cache.DirectoryTooLargeError
	if !errors.As(err, &tooLarge) {
		return err
	}
	stop := errors.New("adapter listing complete")
	return wfs.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		err := filer_pb.SeaweedList(ctx, client, string(dir), "", func(pbEntry *filer_pb.Entry, _ bool) error {
			if !wfs.option.IncludeSystemEntries && meta_cache.IsHiddenSystemEntry(string(dir), pbEntry.Name) {
				return nil
			}
			if wfs.option.UidGidMapper != nil && pbEntry.Attributes != nil {
				pbEntry.Attributes.Uid, pbEntry.Attributes.Gid = wfs.option.UidGidMapper.FilerToLocal(pbEntry.Attributes.Uid, pbEntry.Attributes.Gid)
			}
			more, err := each(filer.FromPbEntry(string(dir), pbEntry))
			if err != nil {
				return err
			}
			if !more {
				return stop
			}
			return nil
		}, "", false, 0)
		if errors.Is(err, stop) {
			return nil
		}
		return err
	})
}
