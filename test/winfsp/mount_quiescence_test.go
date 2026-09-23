package winfsp

import (
	"testing"
	"time"
)

// Both calls must leave the DLL before its import hook can be restored.
func waitForLabMountQuiescence(unmount func() bool, mountDone <-chan bool, limit time.Duration) bool {
	unmountDone := make(chan bool, 1)
	go func() { unmountDone <- unmount() }()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for mountDone != nil || unmountDone != nil {
		select {
		case <-mountDone:
			mountDone = nil
		case <-unmountDone:
			unmountDone = nil
		case <-timer.C:
			return false
		}
	}
	return true
}

func TestLabMountQuiescence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mount, unmount bool
	}{
		{"both exit", true, true},
		{"mount still running", false, true},
		{"unmount still running", true, false},
		{"both still running", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mountDone := make(chan bool, 1)
			if tc.mount {
				mountDone <- false
			}
			release := make(chan struct{})
			defer close(release)
			unmount := func() bool {
				if !tc.unmount {
					<-release
				}
				return false
			}
			if got := waitForLabMountQuiescence(unmount, mountDone, 20*time.Millisecond); got != (tc.mount && tc.unmount) {
				t.Fatalf("quiescent=%v", got)
			}
		})
	}
}
