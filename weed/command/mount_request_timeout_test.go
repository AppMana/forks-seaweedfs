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

// An O_CREAT open holds the parent directory's lock across LOOKUP and CREATE,
// and a revalidating GETATTR can precede them; three namespace requests each
// answered at the deadline must still finish inside hung_task's 120 s.
func TestFuseMetadataRequestTimeoutDefault(t *testing.T) {
	request, _, err := fuseRequestTimeouts(&mountOptions)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := fuseMetadataRequestTimeout(&mountOptions, request)
	if err != nil {
		t.Fatal(err)
	}
	if metadata != 25*time.Second {
		t.Fatalf("default metadata request timeout %v, want 25s", metadata)
	}
	if 3*metadata >= 120*time.Second {
		t.Fatalf("three chained namespace requests (%v) reach the hung-task window", 3*metadata)
	}
}

func TestFuseMetadataRequestTimeoutMustNotExceedRequestTimeout(t *testing.T) {
	for _, tc := range []struct {
		metadata, request time.Duration
		ok                bool
	}{
		{25 * time.Second, 60 * time.Second, true},
		{60 * time.Second, 60 * time.Second, true},
		{61 * time.Second, 60 * time.Second, false},
		{0, 60 * time.Second, true},
		{-time.Second, 60 * time.Second, false},
		{25 * time.Second, 0, true},
	} {
		options := MountOptions{fuseMetadataRequestTimeout: &tc.metadata}
		_, err := fuseMetadataRequestTimeout(&options, tc.request)
		if (err == nil) != tc.ok {
			t.Errorf("metadata %v, request %v: err %v, want ok=%v", tc.metadata, tc.request, err, tc.ok)
		}
	}
}
