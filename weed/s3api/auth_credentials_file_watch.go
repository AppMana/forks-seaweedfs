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

// ReloadStaticConfigFile re-reads the -s3.config file and applies it.
//
// This is the single reload entry point: both the SIGHUP handler and the file
// watcher call it, so the two can never drift apart. All of the actual work
// lives in loadS3ApiConfigurationFromFile, which since upstream's
// fromStaticFile change (PR #10096) already overwrites the file's own static
// identities, re-marks them, and republishes them to the credential manager.
//
// Identities added, removed or rotated in the file all take effect; identities
// that came from the filer or from environment variables are left untouched.
// On any error the previously loaded identities are kept, because
// loadS3ApiConfigurationFromFile validates and parses before it mutates
// anything.
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

// startStaticConfigWatcher polls the -s3.config file and reloads identities
// whenever its content changes, so credentials can be added, rotated or revoked
// without restarting the server and without an operator having to deliver a
// SIGHUP by hand.
//
// The file's *content* is hashed rather than watching the file for write
// events. Kubernetes projects Secrets and ConfigMaps as a symlink farm: the
// mounted path is a symlink to ..data/<key>, and ..data is itself a symlink
// that kubelet swaps atomically with a rename. The inode the process opened at
// startup is therefore never written to, so an inotify watch on the file never
// fires. This was verified against a live pod; do not "optimize" this into an
// fsnotify watch on the path.
//
// An interval of 0 disables the watcher entirely (SIGHUP still works).
func (iam *IdentityAccessManagement) startStaticConfigWatcher(fileName string, interval time.Duration) {
	if fileName == "" || interval <= 0 {
		return
	}

	lastHash, err := hashFileContent(fileName)
	if err != nil {
		// Not fatal: the initial load already succeeded or the process would
		// not be here. A nil hash simply means the first tick reloads once.
		glog.Warningf("fail to hash config file %s: %v", fileName, err)
	}

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
