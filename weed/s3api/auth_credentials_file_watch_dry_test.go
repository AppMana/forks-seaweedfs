package s3api

import (
	"path/filepath"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/iam_pb"
)

// The whole point of routing SIGHUP and the file watcher through one function
// is that they cannot diverge. These tests pin the properties that would
// silently break if someone re-introduced a second reload path.

// Identity removal is the fork's addition -- upstream's merge is purely
// additive. Because removal lives inside the shared reload path rather than
// inside the watcher, an operator sending SIGHUP gets it too. If removal were
// implemented in the watcher goroutine instead, this test would fail while the
// watcher tests still passed.
func TestSighupReloadPathAlsoRemovesIdentities(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(
		identityEntry("alice", "AKALICE", "SKALICE"),
		identityEntry("bob", "AKBOB", "SKBOB"),
	))

	iam := newStaticFileIam(t, configFile)
	if !hasAccessKey(iam, "AKBOB", "SKBOB") {
		t.Fatal("precondition: bob should be loaded")
	}

	// Drop bob, then reload the way the SIGHUP handler does -- no watcher.
	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))
	if err := iam.ReloadStaticConfigFile(configFile); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if hasAccessKey(iam, "AKBOB", "SKBOB") {
		t.Fatal("bob was deleted from the config file but still authenticates after a SIGHUP-style reload")
	}
	if !hasAccessKey(iam, "AKALICE", "SKALICE") {
		t.Fatal("alice must survive the reload")
	}
	if iam.IsStaticIdentity("bob") {
		t.Fatal("bob should no longer be tracked as a static identity")
	}
}

// A config-file reload owns only the names that file put there. Identities that
// became static some other way (environment variables) share the same
// staticIdentityNames map, and removing them on reload would silently revoke
// credentials the file never knew about. This is why fileIdentityNames is
// tracked separately.
func TestFileReloadDoesNotRemoveNonFileStaticIdentities(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))

	iam := newStaticFileIam(t, configFile)

	// Simulate an identity made static by something other than this file.
	iam.m.Lock()
	iam.staticIdentityNames["env-user"] = true
	iam.m.Unlock()

	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE2")))
	if err := iam.ReloadStaticConfigFile(configFile); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if !iam.IsStaticIdentity("env-user") {
		t.Fatal("a non-file static identity was released by a config-file reload; only names the file owns may be dropped")
	}
	if !hasAccessKey(iam, "AKALICE", "SKALICE2") {
		t.Fatal("alice's rotated secret should be live")
	}
}

// interval <= 0 must disable the watcher outright so -s3.config.reloadInterval=0
// is a real off switch. SIGHUP must keep working.
func TestStaticConfigWatcherDisabledWithZeroInterval(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))

	iam := newStaticFileIam(t, configFile)
	iam.startStaticConfigWatcher(configFile, 0)

	writeConfigFile(t, configFile, identityConfigJson(
		identityEntry("alice", "AKALICE", "SKALICE"),
		identityEntry("bob", "AKBOB", "SKBOB"),
	))

	// Give a (hypothetical) watcher goroutine ample opportunity to fire.
	for i := 0; i < 25; i++ {
		if hasAccessKey(iam, "AKBOB", "SKBOB") {
			t.Fatal("watcher reloaded the file despite interval=0; the flag must be a real off switch")
		}
		sleepWatchTick()
	}

	// SIGHUP-equivalent reload still works with the watcher disabled.
	if err := iam.ReloadStaticConfigFile(configFile); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !hasAccessKey(iam, "AKBOB", "SKBOB") {
		t.Fatal("explicit reload must still work when the watcher is disabled")
	}
}

// An empty config path must not start a watcher either (weed server / weed mini
// pass the default interval through even when no -config was given).
func TestStaticConfigWatcherIgnoresEmptyPath(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))
	iam := newStaticFileIam(t, configFile)

	// Must not panic or spawn anything that touches "".
	iam.startStaticConfigWatcher("", DefaultStaticConfigReloadInterval)
	sleepWatchTick()
}

// A malformed file must never wipe live identities, on the SIGHUP path too --
// not just via the watcher's error branch.
func TestSighupReloadKeepsIdentitiesOnMalformedFile(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))
	iam := newStaticFileIam(t, configFile)

	writeConfigFile(t, configFile, `{"identities":[{"name":"alice",`) // truncated
	if err := iam.ReloadStaticConfigFile(configFile); err == nil {
		t.Fatal("expected a malformed config file to produce an error")
	}
	if !hasAccessKey(iam, "AKALICE", "SKALICE") {
		t.Fatal("a malformed config file wiped the live identities")
	}
}

// Service accounts and dynamic identities must survive a reload that removes a
// different file identity, i.e. removal must be precisely scoped.
func TestFileReloadRemovalIsScopedToTheRemovedName(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "seaweedfs_s3_config")
	writeConfigFile(t, configFile, identityConfigJson(
		identityEntry("alice", "AKALICE", "SKALICE"),
		identityEntry("bob", "AKBOB", "SKBOB"),
	))
	iam := newStaticFileIam(t, configFile)

	if err := iam.UpsertIdentity(&iam_pb.Identity{
		Name:        "dynamic-user",
		Actions:     []string{"Read"},
		Credentials: []*iam_pb.Credential{{AccessKey: "AKDYN", SecretKey: "SKDYN"}},
	}); err != nil {
		t.Fatalf("add dynamic identity: %v", err)
	}

	writeConfigFile(t, configFile, identityConfigJson(identityEntry("alice", "AKALICE", "SKALICE")))
	if err := iam.ReloadStaticConfigFile(configFile); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if hasAccessKey(iam, "AKBOB", "SKBOB") {
		t.Fatal("bob should have been removed")
	}
	if !hasAccessKey(iam, "AKALICE", "SKALICE") {
		t.Fatal("alice should be untouched")
	}
	if !hasAccessKey(iam, "AKDYN", "SKDYN") {
		t.Fatal("the dynamic identity must not be collateral damage of a file removal")
	}
}
