package fuse_test

import (
	"bytes"
	"crypto/rand"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A mount whose filer and volume servers stop answering, and whose cache
// directory is deleted underneath it, must still answer every request within
// -fuse.requestTimeout. A FUSE request the mount never answers holds the
// calling task in uninterruptible sleep, and on nodes with hung_task_panic
// that panics the node. The kernel request timeout is disabled here so the
// mount's own deadline is what is tested.
func TestUnresponsiveBackendAnswersWithinRequestTimeout(t *testing.T) {
	const requestTimeout = 3 * time.Second
	// A failed readahead READ is followed by a synchronous READ of the same
	// page, so a read() can wait on two requests in turn, each within the
	// timeout. Every wait wakes the task, which is what the hung-task
	// watchdog measures.
	const bound = 2*requestTimeout + time.Second

	config := DefaultTestConfig()
	cacheDir := filepath.Join(t.TempDir(), "cache")
	config.MountOptions = []string{
		"-cacheDir=" + cacheDir,
		"-fuse.requestTimeout=" + requestTimeout.String(),
		"-fuse.kernelRequestTimeout=0",
	}
	framework := NewFuseTestFramework(t, config)
	defer framework.Cleanup()
	require.NoError(t, framework.Setup(config))
	mnt := framework.GetMountPoint()

	payload := make([]byte, 8<<20)
	rand.Read(payload)
	// Upload through the filer, not the mount, so the read below cannot be
	// served from the kernel page cache.
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "written")
	require.NoError(t, err)
	_, err = part.Write(payload)
	require.NoError(t, err)
	require.NoError(t, form.Close())
	resp, err := http.Post("http://"+framework.GetFilerAddr()+"/written", form.FormDataContentType(), &body)
	require.NoError(t, err)
	resp.Body.Close()
	require.Less(t, resp.StatusCode, 300, "filer upload status")
	require.Eventually(t, func() bool {
		info, err := os.Stat(filepath.Join(mnt, "written"))
		return err == nil && info.Size() == int64(len(payload))
	}, 10*time.Second, 100*time.Millisecond)
	held, err := os.OpenFile(filepath.Join(mnt, "held"), os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)

	mini := framework.miniProcess.cmd.Process
	require.NoError(t, mini.Signal(syscall.SIGSTOP))
	defer mini.Signal(syscall.SIGCONT)
	require.NoError(t, os.RemoveAll(cacheDir))

	within := func(name string, op func() error) {
		t.Helper()
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- op() }()
		select {
		case err := <-done:
			t.Logf("%s returned after %v: %v", name, time.Since(start).Round(time.Millisecond), err)
		case <-time.After(bound):
			t.Errorf("%s did not return within %v", name, bound)
		}
	}

	within("lookup of a new name", func() error {
		_, err := os.Stat(filepath.Join(mnt, "absent"))
		return err
	})
	within("mkdir", func() error {
		return os.Mkdir(filepath.Join(mnt, "dir"), 0o755)
	})
	within("read", func() error {
		f, err := os.Open(filepath.Join(mnt, "written"))
		if err != nil {
			return err
		}
		defer f.Close()
		// One page: the kernel splits a larger read() into more READ
		// requests and waits for them in turn.
		got := make([]byte, 4096)
		n, err := f.Read(got)
		if err == nil && !bytes.Equal(got[:n], payload[:n]) {
			t.Errorf("read returned wrong data")
		}
		return err
	})
	within("write and close", func() error {
		if _, err := held.Write(payload); err != nil {
			held.Close()
			return err
		}
		return held.Close()
	})
	within("fsync of a new file", func() error {
		f, err := os.Create(filepath.Join(mnt, "synced"))
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(payload); err != nil {
			return err
		}
		return f.Sync()
	})
	within("exit of a SIGKILLed writer", func() error {
		cmd := exec.Command("sh", "-c", `exec 3>"$1"; head -c 8388608 /dev/urandom >&3; echo ready; exec sleep 600`, "sh", filepath.Join(mnt, "killed"))
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		ready := make([]byte, 6)
		stdout.Read(ready)
		cmd.Process.Kill()
		return cmd.Wait()
	})
}
