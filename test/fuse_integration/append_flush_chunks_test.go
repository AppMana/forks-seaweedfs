package fuse_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A log that is flushed after every line, by fsync or by reopening it, adds
// one small chunk per flush; folding the appended tail keeps the entry's chunk
// list bounded across flushes.
func TestAppendFlushesKeepEntryChunkCountBounded(t *testing.T) {
	const (
		lines = 400
		// the tail run (16) plus one chunk per doubling up to the chunk size
		maxChunks = 40
	)
	config := DefaultTestConfig()
	framework := NewFuseTestFramework(t, config)
	defer framework.Cleanup()
	require.NoError(t, framework.Setup(config))

	dir := filepath.Join(framework.GetMountPoint(), "append_flush")
	require.NoError(t, os.Mkdir(dir, 0755))

	line := func(i int) []byte { return []byte(fmt.Sprintf("%05d %s\n", i, bytes.Repeat([]byte{'l'}, 100+i%200))) }

	t.Run("fsync", func(t *testing.T) {
		path := filepath.Join(dir, "fsync.log")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		require.NoError(t, err)
		var want []byte
		for i := 0; i < lines; i++ {
			_, err := f.Write(line(i))
			require.NoError(t, err)
			require.NoError(t, f.Sync())
			want = append(want, line(i)...)
		}
		require.NoError(t, f.Close())
		assertAppendedEntry(t, framework, "/append_flush/fsync.log", path, want, maxChunks)
	})

	t.Run("reopen", func(t *testing.T) {
		path := filepath.Join(dir, "reopen.log")
		var want []byte
		for i := 0; i < lines; i++ {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
			require.NoError(t, err)
			_, err = f.Write(line(i))
			require.NoError(t, err)
			require.NoError(t, f.Close())
			want = append(want, line(i)...)
		}
		assertAppendedEntry(t, framework, "/append_flush/reopen.log", path, want, maxChunks)
	})
}

func assertAppendedEntry(t *testing.T, framework *FuseTestFramework, filerPath, mountPath string, want []byte, maxChunks int) {
	t.Helper()
	got, err := os.ReadFile(mountPath)
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, want), "content: %d bytes, want %d", len(got), len(want))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + framework.GetFilerAddr() + filerPath + "?metadata=true")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var entry struct {
		Chunks []struct {
			FileID string `json:"file_id"`
			Size   uint64 `json:"size"`
		} `json:"chunks"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&entry))
	t.Logf("%s: %d bytes in %d chunks", filerPath, len(want), len(entry.Chunks))
	require.LessOrEqual(t, len(entry.Chunks), maxChunks, "chunks in the committed entry")
}
