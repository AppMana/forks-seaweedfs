//go:build linux || darwin || freebsd

package command

import (
	"testing"
	"time"
)

// A mount must answer every FUSE request well inside the kernel's hung-task
// window (120 s), and answer it itself before the kernel's request timeout
// aborts the whole connection.
func TestFuseRequestTimeoutDefaults(t *testing.T) {
	request, kernel, err := fuseRequestTimeouts(&mountOptions)
	if err != nil {
		t.Fatal(err)
	}
	if request != 60*time.Second || kernel != 90*time.Second {
		t.Fatalf("default request timeouts: weed %v, kernel %v; want 60s, 90s", request, kernel)
	}
}

func TestFuseRequestTimeoutMustExpireBeforeKernelTimeout(t *testing.T) {
	for _, tc := range []struct {
		request, kernel time.Duration
		ok              bool
	}{
		{60 * time.Second, 90 * time.Second, true},
		{90 * time.Second, 90 * time.Second, false},
		{0, 90 * time.Second, false},
		{60 * time.Second, 0, true},
		{0, 0, true},
	} {
		options := MountOptions{fuseRequestTimeout: &tc.request, fuseKernelRequestTimeout: &tc.kernel}
		_, _, err := fuseRequestTimeouts(&options)
		if (err == nil) != tc.ok {
			t.Errorf("weed %v, kernel %v: err %v, want ok=%v", tc.request, tc.kernel, err, tc.ok)
		}
	}
}
