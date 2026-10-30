package s3api

import (
	"bytes"
	"crypto/sha256"
	"os"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// DefaultStaticConfigReloadInterval is how often the -s3.config file is polled
// for changes when -s3.config.reloadInterval is not set.
const DefaultStaticConfigReloadInterval = 10 * time.Second

// ReloadStaticConfigFile reloads static identities, preserving identities
// from the filer and environment. Parse failures leave the active config intact.
func (iam *IdentityAccessManagement) ReloadStaticConfigFile(fileName string) error {
	if err := iam.loadS3ApiConfigurationFromFile(fileName); err != nil {
		return err
	}

	iam.m.RLock()
	count := len(iam.identities)
	iam.m.RUnlock()
	glog.V(0).Infof("loaded %d identities from config file %s", count, fileName)
	return nil
}

// startStaticConfigWatcher hashes content because projected Kubernetes
// Secrets replace symlinks rather than writing the watched inode.
// A nonpositive interval disables polling.
func (iam *IdentityAccessManagement) startStaticConfigWatcher(fileName string, interval time.Duration) {
	if fileName == "" || interval <= 0 {
		return
	}

	// The file can change between the initial load and watcher startup.
	// Reload on the first tick rather than assuming this content is active.
	var lastHash []byte

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-iam.stopChan:
				return
			case <-ticker.C:
				currentHash, err := hashFileContent(fileName)
				if err != nil {
					// The path can be briefly absent while kubelet swaps the
					// ..data symlink. Keep the live identities and retry.
					glog.V(1).Infof("fail to read config file %s: %v", fileName, err)
					continue
				}
				if bytes.Equal(currentHash, lastHash) {
					continue
				}
				// Record the new content even if the load fails: a malformed
				// file must not be retried on every single tick, and the next
				// real edit will hash differently and be picked up.
				lastHash = currentHash
				glog.V(0).Infof("config file %s changed, reloading identities", fileName)
				if err := iam.ReloadStaticConfigFile(fileName); err != nil {
					glog.Errorf("fail to reload config file %s, keeping the previously loaded identities: %v", fileName, err)
				}
			}
		}
	}()
}

func hashFileContent(fileName string) ([]byte, error) {
	content, err := os.ReadFile(fileName)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(content)
	return sum[:], nil
}
