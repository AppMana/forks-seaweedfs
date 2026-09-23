package winfsp

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Exercise OS cleanup, not an application Unmount path. Each child first runs
// the real lifecycle oracle, then waits at an explicit pre-unmount checkpoint.
func TestMountManagerProcessCrash(t *testing.T) {
	if os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_MANAGER_LAB") != "1" {
		t.Skip("requires a disposable elevated Windows VM with WinFsp")
	}
	root := t.TempDir()
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	token := fmt.Sprintf("%x", nonce)
	for name, content := range map[string]string{".crash-owner": token, "unrelated-data.txt": crashSiblingContent} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	point := filepath.Join(root, "reused-mount")
	for cycle := 0; cycle < 8; cycle++ {
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ready := filepath.Join(root, fmt.Sprintf("ready-%d", cycle))
			logPath := filepath.Join(root, fmt.Sprintf("child-%d.log", cycle))
			log, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestMountManagerDirectoryLifecycle$", "-test.v", "-test.timeout=25s", "-mount-manager-check-cleanup", "-mount-manager-crash-child")
			cmd.Env = append(os.Environ(), "SEAWEEDFS_MOUNT_CRASH_ROOT="+root, "SEAWEEDFS_MOUNT_CRASH_READY="+ready, "SEAWEEDFS_MOUNT_CRASH_TOKEN="+token)
			cmd.Stdout, cmd.Stderr = log, log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			waited := false
			defer func() {
				if !waited {
					_ = cmd.Process.Kill()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("terminated child did not exit within cleanup deadline")
					}
				}
				data, err := os.ReadFile(logPath)
				t.Logf("child cycle=%d log error=%v\n%s", cycle, err, data)
			}()
			var guid string
			for guid == "" {
				data, err := os.ReadFile(ready)
				if err == nil && len(data) > 0 {
					guid = string(data)
					if len(guid) != 49 || !strings.HasPrefix(guid, `\\?\Volume{`) || !strings.HasSuffix(guid, `}\`) {
						t.Fatalf("invalid child volume identity: %q", guid)
					}
					break
				}
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					waited = true
					t.Fatalf("child exited before crash checkpoint: %v", err)
				case <-ctx.Done():
					t.Fatal("child did not reach crash checkpoint")
				case <-time.After(10 * time.Millisecond):
				}
			}
			paths, err := mountManagerVolumePaths(guid)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, path := range paths {
				found = found || strings.EqualFold(filepath.Clean(path), filepath.Clean(point))
			}
			if !found {
				t.Fatalf("pre-crash mapping absent: %q", paths)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatalf("terminate owned child: %v", err)
			}
			select {
			case err := <-done:
				waited = true
				if err == nil {
					t.Error("child unexpectedly exited successfully rather than being terminated")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("terminated child did not exit within deadline")
			}
			if _, err := os.Lstat(point); !os.IsNotExist(err) {
				t.Errorf("junction remains after child death: %v", err)
			}
			paths, err = mountManagerVolumePaths(guid)
			if err != nil && err != windows.ERROR_FILE_NOT_FOUND && err != windows.ERROR_PATH_NOT_FOUND {
				t.Errorf("query volume after child death: %v", err)
			}
			for _, path := range paths {
				if strings.EqualFold(filepath.Clean(path), filepath.Clean(point)) {
					t.Errorf("stale mapping after child death: %q", path)
				}
			}
			data, err := os.ReadFile(filepath.Join(root, "unrelated-data.txt"))
			if err != nil || string(data) != "unrelated data must survive mount cleanup" {
				t.Errorf("sibling changed after child death: %v", err)
			}
			if t.Failed() {
				t.FailNow()
			}
			t.Logf("cycle=%d: crash cleanup and sibling preservation succeeded", cycle)
		}()
	}
}
