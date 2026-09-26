package mount

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	cgofuse "github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

// Called once from Init, after cgofuse loaded the selected WinFsp DLL. Reuse
// that exact module; do not search for or load another DLL. Only the synthetic
// mount root needs a local owner. Real entries retain their stored identities.
func winfspMountIdentity() (uint32, uint32, error) {
	name := map[string]string{"amd64": "winfsp-x64.dll", "386": "winfsp-x86.dll", "arm64": "winfsp-a64.dll"}[runtime.GOARCH]
	if name == "" {
		return 0, 0, fmt.Errorf("unsupported WinFsp architecture %s", runtime.GOARCH)
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, 0, err
	}
	var module windows.Handle
	if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, p, &module); err != nil {
		return 0, 0, err
	}
	proc, err := windows.GetProcAddress(module, "FspPosixMapSidToUid")
	if err != nil {
		return 0, 0, err
	}
	token := windows.GetCurrentProcessToken()
	// TokenOwner, not TokenUser: SYSTEM's default owner is Administrators.
	// Use that local owner for the synthetic root only. This does not repair
	// the separate default-create descriptor translation inside WinFsp.
	var size uint32
	if err := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &size); err != windows.ERROR_INSUFFICIENT_BUFFER {
		return 0, 0, fmt.Errorf("query token owner size: %v", err)
	}
	if size < uint32(unsafe.Sizeof(uintptr(0))) {
		return 0, 0, fmt.Errorf("short token owner information")
	}
	ownerInfo := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &ownerInfo[0], size, &size); err != nil {
		return 0, 0, err
	}
	owner := *(**windows.SID)(unsafe.Pointer(&ownerInfo[0]))
	group, err := token.GetTokenPrimaryGroup()
	if err != nil {
		return 0, 0, err
	}
	mapSID := func(sid *windows.SID) (uint32, error) {
		var id uint32
		status, _, _ := syscall.SyscallN(proc, uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&id)))
		if int32(status) < 0 {
			return 0, fmt.Errorf("FspPosixMapSidToUid: NTSTATUS %#x", uint32(status))
		}
		return id, nil
	}
	uid, err := mapSID(owner)
	runtime.KeepAlive(ownerInfo)
	if err != nil {
		return 0, 0, err
	}
	gid, err := mapSID(group.PrimaryGroup)
	runtime.KeepAlive(group)
	if err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

func (a *winfspFS) Init() {
	if !a.basicPermissions {
		return
	}
	uid, gid, err := winfspMountIdentity()
	if err != nil {
		a.identityError = err
		glog.Errorf("WinFsp mount identity: %v", err)
		return
	}
	a.wfs.option.MountUid = uid
	a.wfs.option.MountGid = gid
}

// WinFsp assumes create callbacks record its request-context identity and
// calls Chown only when the requested descriptor differs from that identity.
// This is a native thread-local lookup only on creation, never on data I/O.
func (a *winfspFS) creationHeader(ino uint64) fuse.InHeader {
	h := a.header(ino)
	if !a.basicPermissions {
		return h
	}
	uid, gid, pid := cgofuse.Getcontext()
	if uid != ^uint32(0) {
		h.Uid = uid
	}
	if gid != ^uint32(0) {
		h.Gid = gid
	}
	if pid >= 0 {
		h.Pid = uint32(pid)
	}
	return h
}
