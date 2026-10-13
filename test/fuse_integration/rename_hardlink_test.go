//go:build linux

package fuse_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestRenameOverMultiplyLinkedTarget is pjdfstest rename/23: renaming over a
// name whose inode has another hard link removes only that name, so the
// surviving link's nlink drops to 1 and its ctime advances.
func TestRenameOverMultiplyLinkedTarget(t *testing.T) {
	framework := NewFuseTestFramework(t, DefaultTestConfig())
	defer framework.Cleanup()
	require.NoError(t, framework.Setup(DefaultTestConfig()))

	dir := filepath.Join(framework.GetMountPoint(), "rename_multiply_linked")
	require.NoError(t, os.Mkdir(dir, 0755))
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	dstLink := filepath.Join(dir, "dstlnk")

	require.NoError(t, os.WriteFile(src, []byte("source"), 0644))
	require.NoError(t, os.WriteFile(dst, []byte("target"), 0644))
	require.NoError(t, os.Link(dst, dstLink))

	var before unix.Stat_t
	require.NoError(t, unix.Lstat(dstLink, &before))
	require.EqualValues(t, 2, before.Nlink, "nlink after link")
	time.Sleep(1100 * time.Millisecond)

	require.NoError(t, os.Rename(src, dst))

	var after unix.Stat_t
	require.NoError(t, unix.Lstat(dstLink, &after))
	assert.EqualValues(t, 1, after.Nlink, "surviving link's nlink after rename over its other name")
	assert.Greater(t, after.Ctim.Sec, before.Ctim.Sec, "surviving link's ctime after rename over its other name")

	got, err := os.ReadFile(dstLink)
	require.NoError(t, err)
	assert.Equal(t, "target", string(got), "surviving link keeps the target's content")
	got, err = os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "source", string(got), "renamed name has the source's content")

	require.NoError(t, os.Remove(dst))
	require.NoError(t, os.Remove(dstLink))
}
