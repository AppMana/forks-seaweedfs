package mount

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// listDirectoryForAdapter preserves the directory cache bound for path-based
// adapters too. An oversized cache build must read through, never enumerate
// the incomplete local cache as if it were a complete directory.
func (wfs *WFS) listDirectoryForAdapter(ctx context.Context, dir util.FullPath, each filer.ListEachEntryFunc) error {
	return wfs.listDirectoryPrefixForAdapter(ctx, dir, "", each)
}

// listDirectoryPrefixForAdapter lists only the children whose names start
// with prefix, in listing order.
func (wfs *WFS) listDirectoryPrefixForAdapter(ctx context.Context, dir util.FullPath, prefix string, each filer.ListEachEntryFunc) error {
	err := wfs.ensureDirectoryVisited(ctx, dir)
	if err == nil {
		// The cache holds its read lock while visiting entries. Adapter
		// callbacks may read local file handles; Flush holds those handles
		// while updating the cache. Invoke callbacks outside the cache lock
		// to avoid that inversion, retaining only one bounded page.
		const pageSize = 128
		// Names sort bytewise, so the prefixed ones are contiguous from the
		// prefix itself.
		start, includeStart := prefix, prefix != ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries := make([]*filer.Entry, 0, pageSize)
			pastPrefix := false
			last, err := wfs.metaCache.ListDirectoryEntries(ctx, dir, start, includeStart, pageSize, func(entry *filer.Entry) (bool, error) {
				if !strings.HasPrefix(entry.Name(), prefix) {
					pastPrefix = true
					return false, nil
				}
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
			if pastPrefix || last == "" || last == start {
				return nil
			}
			start, includeStart = last, false
		}
	}
	var tooLarge *meta_cache.DirectoryTooLargeError
	if !errors.As(err, &tooLarge) {
		return err
	}
	stop := errors.New("adapter listing complete")
	return wfs.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		err := filer_pb.SeaweedList(ctx, client, string(dir), prefix, func(pbEntry *filer_pb.Entry, _ bool) error {
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

// caseFoldPrefixMaxVariants bounds how many prefixes a case-folded lookup
// lists. Each is one bounded range read, local or from the filer.
const caseFoldPrefixMaxVariants = 16

// caseFoldPrefixes returns every spelling, under simple case folding, of the
// longest leading part of name that has at most limit spellings. Folding maps
// rune to rune, so every name that equals name case-folded starts with exactly
// one of them. Runes outside any case orbit cost nothing, so a prefix usually
// runs well into the name.
func caseFoldPrefixes(name string, limit int) []string {
	prefixes := []string{""}
	for len(name) > 0 {
		r, size := utf8.DecodeRuneInString(name)
		if r == utf8.RuneError {
			// EqualFold reads every invalid byte, and U+FFFD itself, as the
			// same rune, so no byte prefix can cover what follows.
			break
		}
		orbit := []rune{r}
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			orbit = append(orbit, folded)
		}
		if len(prefixes)*len(orbit) > limit {
			break
		}
		next := make([]string, 0, len(prefixes)*len(orbit))
		for _, prefix := range prefixes {
			for _, variant := range orbit {
				next = append(next, prefix+string(variant))
			}
		}
		prefixes = next
		name = name[size:]
	}
	return prefixes
}

// findCaseFoldedChild returns the child of dir whose name equals requested
// under simple case folding, the first in listing order when several do, or
// "" when none does. It lists only the names that start with one of
// requested's case-folded prefixes, never the whole directory.
func (wfs *WFS) findCaseFoldedChild(ctx context.Context, dir util.FullPath, requested string) (string, error) {
	var actual string
	for _, prefix := range caseFoldPrefixes(requested, caseFoldPrefixMaxVariants) {
		err := wfs.listDirectoryPrefixForAdapter(ctx, dir, prefix, func(entry *filer.Entry) (bool, error) {
			if !strings.EqualFold(requested, entry.Name()) {
				return true, nil
			}
			if actual == "" || entry.Name() < actual {
				actual = entry.Name()
			}
			return false, nil
		})
		if err != nil {
			return "", err
		}
	}
	return actual, nil
}
