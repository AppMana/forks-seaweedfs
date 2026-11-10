package chunk_cache

import (
	"bytes"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
)

func TestSharedCacheSlowMaintenanceDoesNotBlockReads(t *testing.T) {
	c := NewSharedTieredChunkCache(0, t.TempDir(), "test", 1, 1, 1024)
	c.onDiskCacheSizeLimit0 = 0
	defer c.Shutdown()
	c.SetChunk("1,0100000001", bytes.Repeat([]byte("a"), 700))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.sharedDisk.readDir = func(path string) ([]os.DirEntry, error) {
		once.Do(func() { close(entered); <-release })
		return os.ReadDir(path)
	}
	defer close(release)
	done := make(chan struct{})
	go func() { c.SetChunk("1,0200000001", bytes.Repeat([]byte("b"), 700)); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not start")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cache insertion waits for directory maintenance")
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		b := make([]byte, 700)
		n, err := c.ReadChunkAt(b, "1,0200000001", 0)
		if err != nil || n != 700 || !bytes.Equal(b, bytes.Repeat([]byte("b"), 700)) {
			t.Errorf("read during maintenance: n=%d err=%v", n, err)
		}
	}()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("cached read waits for directory maintenance")
	}
}

func TestSharedCacheMaintenanceBatchesEviction(t *testing.T) {
	c := NewSharedTieredChunkCache(0, t.TempDir(), "test", 1024, 1024, 1024)
	defer c.Shutdown()
	var scans atomic.Int64
	c.sharedDisk.readDir = func(path string) ([]os.DirEntry, error) {
		if path == c.sharedDisk.dir {
			scans.Add(1)
		}
		return os.ReadDir(path)
	}
	data := bytes.Repeat([]byte("x"), 16*1024)
	for i := 1; i <= 128; i++ {
		c.SetChunk(fmt.Sprintf("1,%x00000001", i), data)
		c.sharedDisk.work.Wait()
	}
	if n := scans.Load(); n == 0 || n > 24 {
		t.Fatalf("128 writes required %d whole-directory inventories; want 1..24", n)
	}
	_, total, err := c.sharedDisk.diskEntries()
	if err != nil || total > 1024*1024 {
		t.Fatalf("quota after maintenance: bytes=%d err=%v", total, err)
	}
}

func TestSharedCacheShutdownWaitsAndSkipsNewPersistence(t *testing.T) {
	c := NewSharedTieredChunkCache(0, t.TempDir(), "test", 1, 1, 1024)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.sharedDisk.readDir = func(path string) ([]os.DirEntry, error) {
		once.Do(func() { close(entered); <-release })
		return os.ReadDir(path)
	}
	c.SetChunk("1,0100000001", bytes.Repeat([]byte("a"), 700))
	c.SetChunk("1,0200000001", bytes.Repeat([]byte("b"), 700))
	<-entered
	done := make(chan struct{})
	go func() { c.Shutdown(); close(done) }()
	for i := 3; i < 30; i++ {
		c.SetChunk(fmt.Sprintf("1,%x00000001", i), bytes.Repeat([]byte("c"), 700))
	}
	if sharedDiskHasChunk(c, "1,0300000001") {
		t.Error("persisted another chunk during maintenance")
	}
	select {
	case <-done:
		t.Error("shutdown returned before maintenance finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join maintenance")
	}
	c.SetChunk("1,ff00000001", []byte("after shutdown"))
	if sharedDiskHasChunk(c, "1,ff00000001") {
		t.Error("persisted after shutdown")
	}
}

func TestSharedCacheFailedInventoryPreservesAccounting(t *testing.T) {
	c := NewSharedTieredChunkCache(0, t.TempDir(), "test", 1, 1, 1024)
	defer c.Shutdown()
	c.SetChunk("1,0100000001", bytes.Repeat([]byte("a"), 700))
	c.sharedDisk.readDir = func(string) ([]os.DirEntry, error) { return nil, os.ErrPermission }
	c.SetChunk("1,0200000001", bytes.Repeat([]byte("b"), 700))
	c.sharedDisk.work.Wait()
	c.sharedDisk.mu.Lock()
	total := c.sharedDisk.wholeUsedBytes
	c.sharedDisk.mu.Unlock()
	if total != 1416 {
		t.Fatalf("failed inventory lost accounting: %d", total)
	}
	c.SetChunk("1,0300000001", bytes.Repeat([]byte("c"), 700))
	if sharedDiskHasChunk(c, "1,0300000001") {
		t.Fatal("failed inventory allowed unbounded growth")
	}
}

func TestSharedCacheConcurrentMaintenanceAndReads(t *testing.T) {
	c := NewSharedTieredChunkCache(0, t.TempDir(), "test", 64, 64, 1024)
	defer c.Shutdown()
	var writers sync.WaitGroup
	for i := 1; i <= 8; i++ {
		writers.Add(1)
		go func(i int) {
			defer writers.Done()
			data := bytes.Repeat([]byte{byte(i)}, 8192)
			for j := 0; j < 30; j++ {
				id := fmt.Sprintf("1,%x00000001", i*100+j)
				c.SetChunk(id, data)
				got := make([]byte, len(data))
				n, _ := c.ReadChunkAt(got, id, 0)
				if n > 0 && (n != len(data) || !bytes.Equal(got, data)) {
					t.Errorf("corrupt cache read for %s: %d", id, n)
				}
			}
		}(i)
	}
	writers.Wait()
	c.sharedDisk.work.Wait()
	_, total, err := c.sharedDisk.diskEntries()
	if err != nil || total > 64*1024+8192+cacheHeaderSize {
		t.Fatalf("unbounded concurrent cache writes: %d %v", total, err)
	}
}

func sharedDiskHasChunk(c *TieredChunkCache, id string) bool {
	fid, _ := needle.ParseFileIdFromString(id)
	return c.sharedDisk.hasChunk(fid)
}
