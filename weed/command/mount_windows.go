package command

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"google.golang.org/grpc/reflection"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/mount_pb"
	"github.com/seaweedfs/seaweedfs/weed/security"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"github.com/seaweedfs/seaweedfs/weed/util/grace"
	"github.com/seaweedfs/seaweedfs/weed/util/version"
)

func runMount(cmd *Command, args []string) bool {

	if *mountOptions.debug {
		go http.ListenAndServe(fmt.Sprintf(":%d", *mountOptions.debugPort), nil)
	}

	*mountCpuProfile = util.ResolvePath(*mountCpuProfile)
	*mountMemProfile = util.ResolvePath(*mountMemProfile)
	grace.SetupProfiling(*mountCpuProfile, *mountMemProfile)
	if *mountReadRetryTime < time.Second {
		*mountReadRetryTime = time.Second
	}
	util.RetryWaitTime = *mountReadRetryTime

	umask, umaskErr := strconv.ParseUint(*mountOptions.umaskString, 8, 64)
	if umaskErr != nil {
		fmt.Printf("can not parse umask %s", *mountOptions.umaskString)
		return false
	}

	if len(args) > 0 {
		return false
	}

	return RunMountWindows(&mountOptions, os.FileMode(umask))
}

// checkWinFspInstalled verifies the WinFsp FSD is installed before
// cgofuse tries (and panics) to load winfsp-x64.dll.
func checkWinFspInstalled() error {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\WinFsp`, registry.QUERY_VALUE)
	if err != nil {
		key, err = registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WinFsp`, registry.QUERY_VALUE)
	}
	if err != nil {
		return fmt.Errorf("WinFsp is not installed (registry key SOFTWARE\\WOW6432Node\\WinFsp not found): install the WinFsp MSI from https://winfsp.dev/rel/ first: %w", err)
	}
	defer key.Close()
	installDir, _, err := key.GetStringValue("InstallDir")
	if err != nil {
		return fmt.Errorf("WinFsp registry key exists but InstallDir is unreadable: %w", err)
	}
	dll := filepath.Join(installDir, "bin", "winfsp-x64.dll")
	if _, err := os.Stat(dll); err != nil {
		return fmt.Errorf("WinFsp DLL %s not found: %w", dll, err)
	}
	return nil
}

// prepareWinFspMountPoint makes dir usable as a WinFsp directory mount
// point: the parent must exist and dir itself must NOT exist (WinFsp
// creates a reparse point there). An empty leftover directory (e.g.
// pre-created by kubelet) or a dangling reparse point from a killed
// prior mount is removed.
func prepareWinFspMountPoint(dir string, dirAutoCreate bool) error {
	parent := filepath.Dir(dir)
	if dirAutoCreate {
		if err := os.MkdirAll(parent, 0777); err != nil {
			return fmt.Errorf("create parent directory %s: %w", parent, err)
		}
	}
	if _, err := os.Stat(parent); err != nil {
		return fmt.Errorf("mount point parent %s not accessible: %w", parent, err)
	}

	fi, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		// A dangling reparse point from a killed mount can fail Lstat in
		// odd ways; try removing it regardless.
		if rmErr := os.Remove(dir); rmErr != nil {
			return fmt.Errorf("mount point %s not statable (%v) and not removable: %w", dir, err, rmErr)
		}
		return nil
	}

	if isReparsePoint(fi) {
		// leftover WinFsp mount point or symlink: remove the link only
		if err := os.Remove(dir); err != nil {
			return fmt.Errorf("remove stale reparse point %s: %w", dir, err)
		}
		return nil
	}

	if !fi.IsDir() {
		return fmt.Errorf("mount point %s exists and is not a directory", dir)
	}
	// plain directory: only remove if empty (os.Remove == RemoveDirectory,
	// which never recurses)
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("mount point %s exists and is not an empty directory: %w", dir, err)
	}
	return nil
}

func isReparsePoint(fi os.FileInfo) bool {
	if sys, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return sys.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return fi.Mode()&os.ModeSymlink != 0
}

func RunMountWindows(option *MountOptions, umask os.FileMode) bool {

	// basic checks
	chunkSizeLimitMB := *mountOptions.chunkSizeLimitMB
	if chunkSizeLimitMB <= 0 {
		fmt.Printf("Please specify a reasonable buffer size.\n")
		return false
	}

	if err := checkWinFspInstalled(); err != nil {
		glog.Errorf("%v", err)
		return false
	}

	// try to connect to filer
	filerAddresses := pb.ServerAddresses(*option.filer).ToAddresses()
	util.LoadSecurityConfiguration()
	grpcDialOption := security.LoadClientTLS(util.GetViper(), "grpc.client")
	var cipher bool
	var bucketRootPath string
	var err error
	for i := 0; i < 10; i++ {
		err = pb.WithOneOfGrpcFilerClients(false, filerAddresses, grpcDialOption, func(client filer_pb.SeaweedFilerClient) error {
			resp, err := client.GetFilerConfiguration(context.Background(), &filer_pb.GetFilerConfigurationRequest{})
			if err != nil {
				return fmt.Errorf("get filer grpc address %v configuration: %w", filerAddresses, err)
			}
			cipher = resp.Cipher
			bucketRootPath = resp.DirBuckets
			return nil
		})
		if err != nil {
			glog.V(0).Infof("failed to talk to filer %v: %v", filerAddresses, err)
			glog.V(0).Infof("wait for %d seconds ...", i+1)
			time.Sleep(time.Duration(i+1) * time.Second)
		}
	}
	if err != nil {
		glog.Errorf("failed to talk to filer %v: %v", filerAddresses, err)
		return true
	}
	if bucketRootPath == "" {
		bucketRootPath = "/buckets"
	}

	filerMountRootPath := *option.filerMountRootPath

	// prepare mount point (WinFsp requires it to NOT exist)
	dir := util.ResolvePath(*option.dir)
	if dir == "" {
		fmt.Printf("Please specify the mount directory via \"-dir\"")
		return false
	}
	isDriveMountPoint := len(dir) == 2 && dir[1] == ':'
	if !isDriveMountPoint {
		if err := prepareWinFspMountPoint(dir, *option.dirAutoCreate); err != nil {
			glog.Errorf("prepare mount point: %v", err)
			return false
		}
	}

	// start on local unix socket (AF_UNIX is supported on Windows Server
	// 2019+; keep paths short for the 108-byte sockaddr_un limit)
	if *option.localSocket == "" {
		mountDirHash := util.HashToInt32([]byte(dir))
		if mountDirHash < 0 {
			mountDirHash = -mountDirHash
		}
		*option.localSocket = filepath.Join(os.TempDir(), fmt.Sprintf("seaweedfs-mount-%d.sock", mountDirHash))
	}
	if err := os.Remove(*option.localSocket); err != nil && !os.IsNotExist(err) {
		glog.Fatalf("Failed to remove %s, error: %s", *option.localSocket, err.Error())
	}
	montSocketListener, err := net.Listen("unix", *option.localSocket)
	if err != nil {
		glog.Fatalf("Failed to listen on %s: %v", *option.localSocket, err)
	}

	// uid/gid have no Windows equivalents; files are owned by the mount
	// identity unless -map.uid/-map.gid translate filer-side ids.
	uid, gid := uint32(0), uint32(0)
	mountMode := os.ModeDir | os.FileMode(0777)&^umask

	uidGidMapper, err := meta_cache.NewUidGidMapper(*option.uidMap, *option.gidMap)
	if err != nil {
		fmt.Printf("failed to parse %s %s: %v\n", *option.uidMap, *option.gidMap, err)
		return false
	}

	// find mount point
	mountRoot := filerMountRootPath
	if mountRoot != "/" && strings.HasSuffix(mountRoot, "/") {
		mountRoot = mountRoot[0 : len(mountRoot)-1]
	}

	cacheDirForRead := util.ResolvePath(*option.cacheDirForRead)
	cacheDirForWrite := util.ResolvePath(*option.cacheDirForWrite)
	if cacheDirForWrite == "" {
		cacheDirForWrite = cacheDirForRead
	}

	// The host is created after the filesystem, so the notifier closure
	// captures a pointer assigned below (before StartBackgroundTasks
	// subscribes to filer events, so there is no race).
	var winFspHost *mount.WinFspHost
	onRemoteMetadataEvent := func(resp *filer_pb.SubscribeMetadataResponse) {
		if winFspHost != nil {
			notifyWinFspFromEvent(winFspHost, mountRoot, resp)
		}
	}

	seaweedFileSystem := mount.NewSeaweedFileSystem(&mount.Option{
		OnRemoteMetadataEvent:       onRemoteMetadataEvent,
		MountDirectory:              dir,
		FilerAddresses:              filerAddresses,
		GrpcDialOption:              grpcDialOption,
		FilerSigningKey:             security.SigningKey(util.GetViper().GetString("jwt.filer_signing.key")),
		FilerSigningExpiresAfterSec: util.GetViper().GetInt("jwt.filer_signing.expires_after_seconds"),
		FilerMountRootPath:          mountRoot,
		Collection:                  *option.collection,
		Replication:                 *option.replication,
		TtlSec:                      int32(*option.ttlSec),
		DiskType:                    types.ToDiskType(*option.diskType),
		ChunkSizeLimit:              int64(chunkSizeLimitMB) * 1024 * 1024,
		ConcurrentWriters:           *option.concurrentWriters,
		ConcurrentReaders:           *option.concurrentReaders,
		CacheDirForRead:             cacheDirForRead,
		CacheSizeMBForRead:          *option.cacheSizeMBForRead,
		CacheDirForWrite:            cacheDirForWrite,
		WriteBufferSizeMB:           *option.writeBufferSizeMB,
		CacheMetaTTlSec:             *option.cacheMetaTtlSec,
		DataCenter:                  *option.dataCenter,
		Quota:                       int64(*option.collectionQuota) * 1024 * 1024,
		MountUid:                    uid,
		MountGid:                    gid,
		MountMode:                   mountMode,
		MountCtime:                  time.Now(),
		MountMtime:                  time.Now(),
		Umask:                       umask,
		VolumeServerAccess:          *mountOptions.volumeServerAccess,
		Cipher:                      cipher,
		UidGidMapper:                uidGidMapper,
		IncludeSystemEntries:        *option.includeSystemEntries,
		DisableXAttr:                *option.disableXAttr,
		IsMacOs:                     false,
		MetadataFlushSeconds:        *option.metadataFlushSeconds,
		// RDMA acceleration options
		RdmaEnabled:           *option.rdmaEnabled,
		RdmaSidecarAddr:       *option.rdmaSidecarAddr,
		RdmaFallback:          *option.rdmaFallback,
		RdmaReadOnly:          *option.rdmaReadOnly,
		RdmaMaxConcurrent:     *option.rdmaMaxConcurrent,
		RdmaTimeoutMs:         *option.rdmaTimeoutMs,
		DirIdleEvictSec:       *option.dirIdleEvictSec,
		EnableDistributedLock: option.distributedLock != nil && *option.distributedLock,
		WritebackCache:        option.writebackCache != nil && *option.writebackCache,
		PosixDirNlink:         option.posixDirNlink != nil && *option.posixDirNlink,
		// Peer chunk sharing
		PeerEnabled:    option.peerEnabled != nil && *option.peerEnabled,
		PeerListen:     peerStringOrEmpty(option.peerListen),
		PeerAdvertise:  peerStringOrEmpty(option.peerAdvertise),
		PeerDataCenter: peerStringOrEmpty(option.peerDataCenter),
		PeerRack:       peerStringOrEmpty(option.peerRack),
	})

	// create mount root
	mountRootPath := util.FullPath(mountRoot)
	mountRootParent, mountDir := mountRootPath.DirAndName()
	if err = filer_pb.Mkdir(context.Background(), seaweedFileSystem, mountRootParent, mountDir, nil); err != nil {
		fmt.Printf("failed to create dir %s on filer %s: %v\n", mountRoot, filerAddresses, err)
		return false
	}
	if err := ensureBucketAllowEmptyFolders(context.Background(), seaweedFileSystem, mountRoot, bucketRootPath); err != nil {
		fmt.Printf("failed to set bucket auto-remove-empty-folders policy for %s: %v\n", mountRoot, err)
		return false
	}

	host := mount.NewWinFspHost(seaweedFileSystem)
	winFspHost = host
	// Both CTRL_C_EVENT and CTRL_BREAK_EVENT are delivered as
	// os.Interrupt by the Go runtime; the CSI mount supervisor stops this
	// process with a console ctrl event for a clean unmount.
	grace.OnInterrupt(func() {
		if !host.Unmount() {
			glog.Errorf("failed to unmount %s", dir)
		}
	})

	seaweedFileSystem.Init(nil)

	grpcS := pb.NewGrpcServer()
	mount_pb.RegisterSeaweedMountServer(grpcS, seaweedFileSystem)
	reflection.Register(grpcS)
	go grpcS.Serve(montSocketListener)

	err = seaweedFileSystem.StartBackgroundTasks()
	if err != nil {
		fmt.Printf("failed to start background tasks: %v\n", err)
		return false
	}

	glog.V(0).Infof("mounting %s%s to %v via WinFsp", *option.filer, mountRoot, dir)
	glog.V(0).Infof("This is SeaweedFS version %s %s %s", version.Version(), runtime.GOOS, runtime.GOARCH)

	var extraOptions []string
	for _, opt := range option.extraOptions {
		extraOptions = append(extraOptions, "-o", opt)
	}
	// -winfspOptions=k=v[,k=v...] appends raw WinFsp options; WinFsp's
	// option parser lets these later -o values override the host
	// defaults (FileInfoTimeout etc.), enabling live A/B tuning.
	if *option.winfspOptions != "" {
		for _, opt := range strings.Split(*option.winfspOptions, ",") {
			if opt = strings.TrimSpace(opt); opt != "" {
				extraOptions = append(extraOptions, "-o", opt)
			}
		}
	}
	if *option.readOnly {
		// WinFsp cannot enforce read-only at the volume level for FUSE
		// filesystems in all paths; the WFS handlers reject writes when
		// the kernel does not. Surface intent via the FUSE option.
		extraOptions = append(extraOptions, "-o", "ro")
	}

	// Register the directory mount point with the Windows mount manager
	// (WinFsp "\\.\C:\dir" mount point syntax). Without this the volume
	// is invisible to the mount manager and GetFinalPathNameByHandle
	// fails with ERROR_UNRECOGNIZED_VOLUME (1005), which breaks any
	// consumer that resolves paths through the mount, most notably
	// containerd's CRI parseMount when a pod consumes a CSI volume
	// staged on this mount. Requires Administrator/SYSTEM, which the
	// mount supervisor always has. WEED_WINFSP_NO_MOUNTMGR=1 opts out
	// (non-elevated interactive use).
	// Note: kubelet may hand out rooted but drive-less paths
	// (\var\lib\kubelet\...); filepath.Abs resolves them against the
	// current drive so the mount point satisfies WinFsp's
	// FspPathIsMountmgrMountPoint syntax (\\.\C:\dir).
	mountPoint := dir
	cleanupSymlink := false
	if prefix := os.Getenv("WEED_WINFSP_VOLUME_PREFIX"); prefix != "" {
		// Experimental: serve the filesystem as a WinFsp NETWORK file
		// system (UNC \\prefix\share) instead of a local volume, and
		// expose it at -dir through a directory symlink to the UNC path.
		// Rationale: HCS cannot attach the container filters (bindflt/
		// wcifs) to local WinFsp volumes ("Do not attach the filter to
		// the volume at this time", winfsp/winfsp#498), so pods cannot
		// consume them; UNC-backed mounts take the network path in HCS,
		// which works (same mechanism as SMB CSI drivers).
		//
		// Each FUSE instance needs a unique prefix; derive the share
		// name from the mount directory. WinFsp network file systems
		// cannot use directory mount points (STATUS 0xc00000ca), so the
		// FUSE mount point is an auto-assigned drive letter ('*') and
		// the -dir symlink is created here once the share is live.
		// The share name must be deterministic for a given mount
		// directory regardless of how the path is spelled: kubelet
		// passes drive-less rooted paths (\var\lib\kubelet\...) while
		// recovery paths may carry the drive letter. Containers resolve
		// their volume mappings to the UNC path once at creation, so a
		// re-mount must reproduce the exact same share name.
		hashSrc := dir
		if abs, err := filepath.Abs(dir); err == nil {
			hashSrc = abs
		}
		h := util.HashToInt32([]byte(strings.ToLower(hashSrc)))
		if h < 0 {
			h = -h
		}
		share := fmt.Sprintf("%s\\%08x", strings.TrimRight(prefix, "\\"), h)
		// Must be the standalone --VolumePrefix= argv form: the
		// "-o VolumePrefix=" form fails FspFileSystemCreate with
		// STATUS_INVALID_PARAMETER through the cgofuse option path.
		extraOptions = append(extraOptions, "--VolumePrefix="+share)
		mountPoint = "*"
		uncPath := `\` + share
		cleanupSymlink = true
		glog.V(0).Infof("winfsp network volume %s, symlink at %s", uncPath, dir)
		go func() {
			// Wait for the share to come up so os.Symlink creates a
			// DIRECTORY symlink (Go picks the flag from the live target).
			for i := 0; i < 600; i++ {
				if _, err := os.Stat(uncPath); err == nil {
					if err := os.Symlink(uncPath, dir); err != nil {
						glog.Errorf("create staging symlink %s -> %s: %v", dir, uncPath, err)
					}
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			glog.Errorf("share %s never became available; staging symlink not created", uncPath)
		}()
	} else if os.Getenv("WEED_WINFSP_NO_MOUNTMGR") != "1" && !strings.HasPrefix(dir, `\\`) {
		if abs, err := filepath.Abs(dir); err == nil && len(abs) >= 2 && abs[1] == ':' {
			mountPoint = `\\.\` + abs
		}
	}
	mountErr := host.Mount(mountPoint, *option.volumeLabel, extraOptions)
	if cleanupSymlink {
		// host.Mount blocks until unmount; remove the staging symlink so
		// a clean unmount leaves nothing behind (mirrors WinFsp removing
		// its own directory mount points).
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			glog.Warningf("remove staging symlink %s: %v", dir, err)
		}
	}
	if mountErr != nil {
		glog.Errorf("mount: %v", mountErr)
		return false
	}
	return true
}

// winfspNotification is one kernel cache invalidation derived from a
// filer metadata event.
type winfspNotification struct {
	path   string
	action uint32
}

// notifyWinFspFromEvent translates a filer metadata event into WinFsp
// kernel cache invalidations.
func notifyWinFspFromEvent(host *mount.WinFspHost, mountRoot string, resp *filer_pb.SubscribeMetadataResponse) {
	for _, n := range winfspNotifications(mountRoot, resp) {
		host.Notify(n.path, n.action)
	}
}

// winfspNotifications computes the invalidations for an event. mountRoot
// is the filer-side mount root; notification paths are '/'-separated and
// volume-relative.
func winfspNotifications(mountRoot string, resp *filer_pb.SubscribeMetadataResponse) (out []winfspNotification) {
	en := resp.EventNotification
	if en == nil {
		return nil
	}
	toVolumePath := func(dir, name string) (string, bool) {
		full := string(util.NewFullPath(dir, name))
		if mountRoot != "/" {
			if !strings.HasPrefix(full, mountRoot) {
				return "", false
			}
			full = full[len(mountRoot):]
		}
		if !strings.HasPrefix(full, "/") {
			full = "/" + full
		}
		return full, true
	}
	oldEntry, newEntry := en.OldEntry, en.NewEntry
	switch {
	case oldEntry != nil && newEntry == nil: // delete
		if p, ok := toVolumePath(resp.Directory, oldEntry.Name); ok {
			action := uint32(mount.NotifyUnlink)
			if oldEntry.IsDirectory {
				action = mount.NotifyRmdir
			}
			out = append(out, winfspNotification{p, action})
		}
	case newEntry != nil && oldEntry == nil: // create
		dir := resp.Directory
		if en.NewParentPath != "" {
			dir = en.NewParentPath
		}
		if p, ok := toVolumePath(dir, newEntry.Name); ok {
			action := uint32(mount.NotifyCreate)
			if newEntry.IsDirectory {
				action = mount.NotifyMkdir
			}
			out = append(out, winfspNotification{p, action})
		}
	case newEntry != nil && oldEntry != nil: // update or rename
		newDir := resp.Directory
		if en.NewParentPath != "" {
			newDir = en.NewParentPath
		}
		oldPath, okOld := toVolumePath(resp.Directory, oldEntry.Name)
		newPath, okNew := toVolumePath(newDir, newEntry.Name)
		if okOld && okNew && oldPath != newPath { // rename
			action := uint32(mount.NotifyUnlink)
			if oldEntry.IsDirectory {
				action = mount.NotifyRmdir
			}
			out = append(out, winfspNotification{oldPath, action})
			action = uint32(mount.NotifyCreate)
			if newEntry.IsDirectory {
				action = mount.NotifyMkdir
			}
			out = append(out, winfspNotification{newPath, action})
		} else if okNew {
			out = append(out, winfspNotification{newPath, uint32(mount.NotifyTruncate | mount.NotifyUtime)})
		}
	}
	return out
}
