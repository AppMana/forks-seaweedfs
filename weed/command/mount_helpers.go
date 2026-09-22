package command

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/filer/empty_folder_cleanup"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3_constants"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// ensureMountRoot creates the mount root on the filer when it does not exist
// yet and leaves an existing one untouched. A plain CreateEntry rewrites an
// existing entry, which reset the bucket's mode, owner and extended
// attributes on every mount start. A bucket created here is a volume, so it
// keeps its empty directories.
func ensureMountRoot(ctx context.Context, filerClient filer_pb.FilerClient, mountRoot, bucketRootPath string) error {
	entry, _, _, err := filer_pb.GetEntry(ctx, filerClient, util.FullPath(mountRoot))
	if err != nil && err != filer_pb.ErrNotFound {
		return err
	}
	if entry != nil {
		return nil
	}
	parent, name := util.FullPath(mountRoot).DirAndName()
	bucketPath, isBucketRootMount := bucketPathForMountRoot(mountRoot, bucketRootPath)
	return filer_pb.Mkdir(ctx, filerClient, parent, name, func(entry *filer_pb.Entry) {
		if isBucketRootMount && bucketPath == path.Clean(mountRoot) {
			empty_folder_cleanup.SetBucketAllowEmptyFolders(entry, true)
		}
	})
}

func ensureBucketAllowEmptyFolders(ctx context.Context, filerClient filer_pb.FilerClient, mountRoot, bucketRootPath string) error {
	bucketPath, isBucketRootMount := bucketPathForMountRoot(mountRoot, bucketRootPath)
	if !isBucketRootMount {
		return nil
	}

	entry, _, _, err := filer_pb.GetEntry(ctx, filerClient, util.FullPath(bucketPath))
	if err != nil {
		return err
	}
	if entry == nil {
		return fmt.Errorf("bucket %s not found", bucketPath)
	}

	if entry.Extended == nil {
		entry.Extended = make(map[string][]byte)
	}
	if strings.EqualFold(strings.TrimSpace(string(entry.Extended[s3_constants.ExtAllowEmptyFolders])), "true") {
		return nil
	}

	entry.Extended[s3_constants.ExtAllowEmptyFolders] = []byte("true")

	bucketFullPath := util.FullPath(bucketPath)
	parent, _ := bucketFullPath.DirAndName()
	if err := filerClient.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		return filer_pb.UpdateEntry(ctx, client, &filer_pb.UpdateEntryRequest{
			Directory: parent,
			Entry:     entry,
		})
	}); err != nil {
		return err
	}

	glog.V(3).Infof("RunMount: set bucket %s %s=true", bucketPath, s3_constants.ExtAllowEmptyFolders)
	return nil
}

func bucketPathForMountRoot(mountRoot, bucketRootPath string) (string, bool) {
	cleanPath := path.Clean("/" + strings.TrimPrefix(mountRoot, "/"))
	cleanBucketRoot := path.Clean("/" + strings.TrimPrefix(bucketRootPath, "/"))
	if cleanBucketRoot == "/" {
		return "", false
	}
	prefix := cleanBucketRoot + "/"
	if !strings.HasPrefix(cleanPath, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(cleanPath, prefix)

	bucketParts := strings.Split(rest, "/")
	if len(bucketParts) != 1 || bucketParts[0] == "" {
		return "", false
	}
	return cleanBucketRoot + "/" + bucketParts[0], true
}

func peerStringOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
