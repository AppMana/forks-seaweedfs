package s3api

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/iam_pb"
)

const testWatchInterval = 20 * time.Millisecond

func identityConfigJson(entries ...string) string {
	body := ""
	for i, entry := range entries {
		if i > 0 {
			body += ","
		}
		body += entry
	}
	return fmt.Sprintf(`{"identities":[%s]}`, body)
}

func identityEntry(name, accessKey, secretKey string) string {
	return fmt.Sprintf(`{"name":"%s","credentials":[{"accessKey":"%s","secretKey":"%s"}],"actions":["Read","Write"]}`,
		name, accessKey, secretKey)
}

// newStaticFileIam builds an IdentityAccessManagement backed only by a static
// config file, the same way NewIdentityAccessManagementWithStore does for the
// -s3.config flag, but without a credential manager or filer.
func newStaticFileIam(t *testing.T, configFile string) *IdentityAccessManagement {
	t.Helper()
	iam := &IdentityAccessManagement{
		hashes:       make(map[string]*sync.Pool),
		hashCounters: make(map[string]*int32),
		stopChan:     make(chan struct{}),
	}
	if err := iam.loadS3ApiConfigurationFromFile(configFile); err != nil {
		t.Fatalf("failed to load initial config file: %v", err)
	}
	t.Cleanup(iam.Shutdown)
	return iam
}

func writeConfigFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

func waitForCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(testWatchInterval)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func hasAccessKey(iam *IdentityAccessManagement, accessKey, secretKey string) bool {
	_, cred, found := iam.LookupByAccessKey(accessKey)
	return found && cred != nil && cred.SecretKey == secretKey
}

func TestStaticConfigWatcherLoadsAddedIdentity(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))

	iam := newStaticFileIam(t, configFile)
	iam.startStaticConfigWatcher(configFile, testWatchInterval)

	if !hasAccessKey(iam, "AKALICE", "SKALICE") {
		t.Fatalf("expected alice to be loaded at startup")
	}
	if _, _, found := iam.LookupByAccessKey("AKBOB"); found {
		t.Fatalf("did not expect bob before the config file changed")
	}

	writeConfigFile(t, configFile, identityConfigJson(
		identityEntry("alice", "AKALICE", "SKALICE"),
		identityEntry("bob", "AKBOB", "SKBOB"),
	))

	waitForCondition(t, "bob to be picked up from the changed config file", func() bool {
		return hasAccessKey(iam, "AKBOB", "SKBOB")
	})
	if !hasAccessKey(iam, "AKALICE", "SKALICE") {
		t.Fatalf("expected alice to survive the reload")
	}
}

func TestStaticConfigWatcherRemovesDeletedIdentity(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(
		identityEntry("alice", "AKALICE", "SKALICE"),
		identityEntry("bob", "AKBOB", "SKBOB"),
	))

	iam := newStaticFileIam(t, configFile)
	iam.startStaticConfigWatcher(configFile, testWatchInterval)

	if !hasAccessKey(iam, "AKBOB", "SKBOB") {
		t.Fatalf("expected bob to be loaded at startup")
	}

	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))

	waitForCondition(t, "bob to be removed after deletion from the config file", func() bool {
		_, _, found := iam.LookupByAccessKey("AKBOB")
		return !found
	})
	if iam.lookupByIdentityName("bob") != nil {
		t.Fatalf("expected bob identity to be gone from the identity list")
	}
	if !hasAccessKey(iam, "AKALICE", "SKALICE") {
		t.Fatalf("expected alice to survive the reload")
	}
}

func TestStaticConfigWatcherRotatesCredentials(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKOLD")))

	iam := newStaticFileIam(t, configFile)
	iam.startStaticConfigWatcher(configFile, testWatchInterval)

	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKNEW")))

	waitForCondition(t, "alice secret key to be rotated", func() bool {
		return hasAccessKey(iam, "AKALICE", "SKNEW")
	})
}

func TestStaticConfigWatcherKeepsPreviousConfigOnMalformedFile(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))

	iam := newStaticFileIam(t, configFile)
	iam.startStaticConfigWatcher(configFile, testWatchInterval)

	// A truncated write, as seen when a config file is being replaced in place.
	writeConfigFile(t, configFile, `{"identities":[{"name":"alice","cred`)

	time.Sleep(20 * testWatchInterval)

	if !hasAccessKey(iam, "AKALICE", "SKALICE") {
		t.Fatalf("expected the previous good identity set to survive a malformed config file")
	}

	// A later good write must still be applied.
	writeConfigFile(t, configFile, identityConfigJson(
		identityEntry("alice", "AKALICE", "SKALICE"),
		identityEntry("bob", "AKBOB", "SKBOB"),
	))
	waitForCondition(t, "recovery after a malformed config file", func() bool {
		return hasAccessKey(iam, "AKBOB", "SKBOB")
	})
}

// TestStaticConfigWatcherKubernetesSymlinkSwap reproduces how kubelet updates a
// projected Secret volume: the config file is a symlink into a `..data` symlink
// that is atomically replaced by a rename, so the original file inode is never
// written to.
func TestStaticConfigWatcherKubernetesSymlinkSwap(t *testing.T) {
	mountDir := t.TempDir()
	configFile := filepath.Join(mountDir, "seaweedfs_s3_config")

	projectSecret := func(timestampDir string, content string) {
		t.Helper()
		dataDir := filepath.Join(mountDir, timestampDir)
		if err := os.Mkdir(dataDir, 0755); err != nil {
			t.Fatalf("failed to create %s: %v", dataDir, err)
		}
		writeConfigFile(t, filepath.Join(dataDir, "seaweedfs_s3_config"), content)
		tempLink := filepath.Join(mountDir, "..data_tmp")
		if err := os.Symlink(timestampDir, tempLink); err != nil {
			t.Fatalf("failed to create temp data symlink: %v", err)
		}
		if err := os.Rename(tempLink, filepath.Join(mountDir, "..data")); err != nil {
			t.Fatalf("failed to swap data symlink: %v", err)
		}
	}

	projectSecret("..2026_07_31_00_00_00.1111", identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))
	if err := os.Symlink(filepath.Join("..data", "seaweedfs_s3_config"), configFile); err != nil {
		t.Fatalf("failed to create config symlink: %v", err)
	}

	iam := newStaticFileIam(t, configFile)
	iam.startStaticConfigWatcher(configFile, testWatchInterval)

	if !hasAccessKey(iam, "AKALICE", "SKALICE") {
		t.Fatalf("expected alice to be loaded at startup")
	}

	// kubelet writes a brand new timestamped directory and swaps ..data at it.
	projectSecret("..2026_07_31_00_00_01.2222", identityConfigJson(
		identityEntry("alice", "AKALICE", "SKALICE"),
		identityEntry("bob", "AKBOB", "SKBOB"),
	))

	waitForCondition(t, "bob to be picked up after a kubelet symlink swap", func() bool {
		return hasAccessKey(iam, "AKBOB", "SKBOB")
	})
}

// TestStaticConfigWatcherReloadWithConcurrentLookups reloads the config file
// while requests are being authenticated, to be run with -race.
func TestStaticConfigWatcherReloadWithConcurrentLookups(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SK0")))

	iam := newStaticFileIam(t, configFile)
	iam.startStaticConfigWatcher(configFile, testWatchInterval)

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, _, found := iam.LookupByAccessKey("AKALICE"); !found {
						t.Errorf("alice must stay resolvable across reloads")
						return
					}
					iam.GetStaticIdentities()
				}
			}
		}()
	}

	for i := 1; i <= 20; i++ {
		writeConfigFile(t, configFile, identityConfigJson(
			identityEntry("alice", "AKALICE", fmt.Sprintf("SK%d", i)),
			identityEntry(fmt.Sprintf("user%d", i), fmt.Sprintf("AK%d", i), "SK"),
		))
		time.Sleep(2 * testWatchInterval)
	}

	close(stop)
	readers.Wait()

	waitForCondition(t, "the last config file revision to be applied", func() bool {
		return hasAccessKey(iam, "AKALICE", "SK20")
	})
}

// TestStaticConfigReloadKeepsDynamicIdentities makes sure reloading the static
// config file does not drop identities that came from the filer.
func TestStaticConfigReloadKeepsDynamicIdentities(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))

	iam := newStaticFileIam(t, configFile)

	if err := iam.UpsertIdentity(&iam_pb.Identity{
		Name:        "dynamic-user",
		Actions:     []string{"Read"},
		Credentials: []*iam_pb.Credential{{AccessKey: "AKDYN", SecretKey: "SKDYN"}},
	}); err != nil {
		t.Fatalf("failed to add dynamic identity: %v", err)
	}
	if !hasAccessKey(iam, "AKDYN", "SKDYN") {
		t.Fatalf("expected the dynamic identity to be loaded")
	}

	writeConfigFile(t, configFile, identityConfigJson(
		identityEntry("alice", "AKALICE", "SKALICE"),
		identityEntry("bob", "AKBOB", "SKBOB"),
	))
	if err := iam.ReloadStaticConfigFile(configFile); err != nil {
		t.Fatalf("failed to reload config file: %v", err)
	}

	if !hasAccessKey(iam, "AKDYN", "SKDYN") {
		t.Fatalf("expected the dynamic identity to survive a static config file reload")
	}
	if !hasAccessKey(iam, "AKBOB", "SKBOB") {
		t.Fatalf("expected bob to be added by the static config file reload")
	}
	if iam.IsStaticIdentity("dynamic-user") {
		t.Fatalf("did not expect the dynamic identity to become static")
	}
}

// sleepWatchTick pauses for one watcher poll interval, for tests that need to
// give a (possibly absent) watcher goroutine a chance to run.
func sleepWatchTick() {
	time.Sleep(testWatchInterval)
}
