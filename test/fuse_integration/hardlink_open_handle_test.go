//go:build linux

package fuse_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// An open descriptor refers to the inode, not to the name it was opened by.
// Removing that name while another hard link survives must leave the
// descriptor's later writes reaching the surviving link.
func writeAfterLosingOneName(t *testing.T, open, survivor string, nameStays bool, remove func()) {
	t.Helper()
	require.NoError(t, os.WriteFile(open, []byte("before"), 0644))
	require.NoError(t, os.Link(open, survivor))
	f, err := os.OpenFile(open, os.O_RDWR, 0)
	require.NoError(t, err)
	remove()
	_, err = f.WriteAt([]byte("after!"), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Eventually(t, func() bool {
		got, err := os.ReadFile(survivor)
		return err == nil && string(got) == "after!"
	}, 5*time.Second, 50*time.Millisecond, "surviving link lost the write made through the open descriptor")
	var st unix.Stat_t
	require.NoError(t, unix.Stat(survivor, &st))
	require.EqualValues(t, 1, st.Nlink, "flush wrote back the descriptor's stale link count")
	if !nameStays {
		_, err = os.Stat(open)
		require.True(t, os.IsNotExist(err), "removed name came back: %v", err)
	}
}

func TestOpenHandleWritesReachSurvivingLinkAfterLocalUnlink(t *testing.T) {
	fw := NewFuseTestFramework(t, DefaultTestConfig())
	defer fw.Cleanup()
	require.NoError(t, fw.Setup(DefaultTestConfig()))
	dir := filepath.Join(fw.GetMountPoint(), "hardlink_local_unlink")
	require.NoError(t, os.Mkdir(dir, 0755))
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	writeAfterLosingOneName(t, a, b, false, func() { require.NoError(t, os.Remove(a)) })
}

func TestOpenHandleWritesReachSurvivingLinkAfterRenameOver(t *testing.T) {
	fw := NewFuseTestFramework(t, DefaultTestConfig())
	defer fw.Cleanup()
	require.NoError(t, fw.Setup(DefaultTestConfig()))
	dir := filepath.Join(fw.GetMountPoint(), "hardlink_rename_over")
	require.NoError(t, os.Mkdir(dir, 0755))
	a, b, src := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "src")
	require.NoError(t, os.WriteFile(src, []byte("source"), 0644))
	writeAfterLosingOneName(t, a, b, true, func() { require.NoError(t, os.Rename(src, a)) })
	got, err := os.ReadFile(a)
	require.NoError(t, err)
	require.Equal(t, "source", string(got))
}

func TestOpenHandleWritesReachSurvivingLinkAfterRemoteUnlink(t *testing.T) {
	config := DefaultTestConfig()
	fw := NewFuseTestFramework(t, config)
	defer fw.Cleanup()
	require.NoError(t, fw.Setup(config))
	mountB, unmountB := fw.mountSecond(t, 0)
	defer unmountB()
	dirA := filepath.Join(fw.GetMountPoint(), "hardlink_remote_unlink")
	dirB := filepath.Join(mountB, "hardlink_remote_unlink")
	require.NoError(t, os.Mkdir(dirA, 0755))
	a, b := filepath.Join(dirA, "a"), filepath.Join(dirA, "b")
	writeAfterLosingOneName(t, a, b, false, func() {
		require.Eventually(t, func() bool {
			_, err := os.Stat(filepath.Join(dirB, "a"))
			return err == nil
		}, 10*time.Second, 50*time.Millisecond)
		require.NoError(t, os.Remove(filepath.Join(dirB, "a")))
		// The descriptor's mount applies the delete before the write.
		require.Eventually(t, func() bool {
			_, err := os.Stat(a)
			return os.IsNotExist(err)
		}, 10*time.Second, 50*time.Millisecond)
	})
}

// A flush through a descriptor opened before the link count changed must not
// write its stale count back: the shared record's count decides when the last
// name's delete frees the content.
func TestOpenHandleFlushKeepsCurrentLinkCount(t *testing.T) {
	fw := NewFuseTestFramework(t, DefaultTestConfig())
	defer fw.Cleanup()
	require.NoError(t, fw.Setup(DefaultTestConfig()))
	dir := filepath.Join(fw.GetMountPoint(), "hardlink_flush_count")
	require.NoError(t, os.Mkdir(dir, 0755))
	a, b, c := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "c")
	require.NoError(t, os.WriteFile(a, []byte("before"), 0644))
	require.NoError(t, os.Link(a, b))
	f, err := os.OpenFile(a, os.O_RDWR, 0)
	require.NoError(t, err)
	require.NoError(t, os.Link(a, c))
	_, err = f.WriteAt([]byte("after!"), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Remove(a))
	require.NoError(t, os.Remove(b))
	got, err := os.ReadFile(c)
	require.NoError(t, err, "last link lost its content")
	require.Equal(t, "after!", string(got))
	var st unix.Stat_t
	require.NoError(t, unix.Stat(c, &st))
	require.EqualValues(t, 1, st.Nlink)
}

func TestOpenHandleWritesReachSurvivingLinkAfterRemoteRenameOver(t *testing.T) {
	config := DefaultTestConfig()
	fw := NewFuseTestFramework(t, config)
	defer fw.Cleanup()
	require.NoError(t, fw.Setup(config))
	mountB, unmountB := fw.mountSecond(t, 0)
	defer unmountB()
	dirA := filepath.Join(fw.GetMountPoint(), "hardlink_remote_rename_over")
	dirB := filepath.Join(mountB, "hardlink_remote_rename_over")
	require.NoError(t, os.Mkdir(dirA, 0755))
	a, b := filepath.Join(dirA, "a"), filepath.Join(dirA, "b")
	require.NoError(t, os.WriteFile(filepath.Join(dirA, "src"), []byte("source"), 0644))
	writeAfterLosingOneName(t, a, b, true, func() {
		require.Eventually(t, func() bool {
			_, errA := os.Stat(filepath.Join(dirB, "a"))
			_, errSrc := os.Stat(filepath.Join(dirB, "src"))
			return errA == nil && errSrc == nil
		}, 10*time.Second, 50*time.Millisecond)
		require.NoError(t, os.Rename(filepath.Join(dirB, "src"), filepath.Join(dirB, "a")))
		require.Eventually(t, func() bool {
			got, err := os.ReadFile(a)
			return err == nil && string(got) == "source"
		}, 10*time.Second, 50*time.Millisecond)
	})
	got, err := os.ReadFile(a)
	require.NoError(t, err)
	require.Equal(t, "source", string(got))
}

// With -writebackCache, close() only queues the flush. Removing that name of a
// hard link straight after must not drop the queued flush: the file lives on.
func TestQueuedFlushReachesSurvivingLinkAfterUnlink(t *testing.T) {
	config := writebackConfig()
	fw := NewFuseTestFramework(t, config)
	defer fw.Cleanup()
	require.NoError(t, fw.Setup(config))
	dir := filepath.Join(fw.GetMountPoint(), "hardlink_queued_flush")
	require.NoError(t, os.Mkdir(dir, 0755))
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i*7 + 1)
	}
	for round := 0; round < 5; round++ {
		a, b := filepath.Join(dir, fmt.Sprintf("a%d", round)), filepath.Join(dir, fmt.Sprintf("b%d", round))
		require.NoError(t, os.WriteFile(a, []byte("before"), 0644))
		require.NoError(t, os.Link(a, b))
		require.NoError(t, os.WriteFile(a, payload, 0644))
		require.NoError(t, os.Remove(a))
		waitForFileContent(t, b, payload, 10*time.Second)
	}
}

// Another mount removing one name of a hard link lowers the link count every
// remaining name reports here, as a local unlink does.
func TestSurvivingLinkCountFollowsRemoteUnlink(t *testing.T) {
	config := DefaultTestConfig()
	fw := NewFuseTestFramework(t, config)
	defer fw.Cleanup()
	require.NoError(t, fw.Setup(config))
	mountB, unmountB := fw.mountSecond(t, 0)
	defer unmountB()
	dirA := filepath.Join(fw.GetMountPoint(), "hardlink_remote_count")
	dirB := filepath.Join(mountB, "hardlink_remote_count")
	require.NoError(t, os.Mkdir(dirA, 0755))
	a, b := filepath.Join(dirA, "a"), filepath.Join(dirA, "b")
	require.NoError(t, os.WriteFile(a, []byte("shared"), 0644))
	require.NoError(t, os.Link(a, b))
	var st unix.Stat_t
	require.NoError(t, unix.Stat(b, &st))
	require.EqualValues(t, 2, st.Nlink)
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dirB, "a"))
		return err == nil
	}, 10*time.Second, 50*time.Millisecond)
	require.NoError(t, os.Remove(filepath.Join(dirB, "a")))
	require.Eventually(t, func() bool {
		var st unix.Stat_t
		return unix.Stat(b, &st) == nil && st.Nlink == 1
	}, 10*time.Second, 100*time.Millisecond, "surviving link still reports the removed name in its link count")
}

