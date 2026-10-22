package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/util/chunk_cache"
)

var cmdCacheMigrate = &Command{
	UsageLine: "cache.migrate -source=/old/cache -target=/managed/chunks -scope=mount-id",
	Short:     "Copy a quiescent legacy read cache into the shared managed cache",
	Long:      "Copies legacy read-cache chunks with bounded memory and byte verification. Refuses active legacy caches. Never removes source files. Retain the source until deployment acceptance. This is not a data-volume or writeback-cache migration.",
}

func init() {
	source := cmdCacheMigrate.Flag.String("source", "", "Exact legacy directory containing c0_2/c1_3/c2_2 indexed cache segments")
	target := cmdCacheMigrate.Flag.String("target", "", "Shared managed chunk directory (must differ from source)")
	scope := cmdCacheMigrate.Flag.String("scope", "", "Destination cache scope")
	capacity := cmdCacheMigrate.Flag.Int64("cacheCapacityMB", 10240, "Per-scope cache capacity in MiB")
	whole := cmdCacheMigrate.Flag.Int64("wholeCacheCapacityMB", 51200, "Whole-cache capacity in MiB")
	budget := cmdCacheMigrate.Flag.Int64("maxCopyMB", 10240, "Maximum source chunk bytes to copy, including cache headers")
	deadline := cmdCacheMigrate.Flag.Duration("timeout", 5*time.Minute, "Maximum migration duration")
	cmdCacheMigrate.Run = func(_ *Command, args []string) bool {
		if len(args) != 0 || *source == "" || *target == "" || *scope == "" || *capacity <= 0 || *whole < *capacity || *budget <= 0 || *deadline <= 0 || *whole > (1<<63-1)>>20 || *budget > (1<<63-1)>>20 {
			fmt.Fprintln(os.Stderr, "explicit source, target, scope and valid positive capacity/budget/timeout required")
			return false
		}
		src, err := filepath.Abs(*source)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return false
		}
		dst, err := filepath.Abs(*target)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return false
		}
		if src == dst {
			fmt.Fprintln(os.Stderr, "source and target must differ")
			return false
		}
		cache := chunk_cache.NewSharedTieredChunkCache(0, dst, *scope, *capacity, *whole, 1<<20)
		defer cache.Shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), *deadline)
		defer cancel()
		n, err := cache.MigrateLegacyCache(ctx, src, *budget<<20)
		if err != nil {
			fmt.Fprintf(os.Stderr, "migration incomplete after %d verified bytes; source retained: %v\n", n, err)
			return false
		}
		fmt.Printf("Copied and verified %d cache bytes; source retained.\n", n)
		return true
	}
}
