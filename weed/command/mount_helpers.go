package command

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3_constants"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func ensureBucketAllowEmptyFolders(ctx context.Context, filerClient filer_pb.FilerClient, mountRoot, bucketRootPath string) error {
	bucketPath, isBucketRootMount := bucketPathForMountRoot(mountRoot, bucketRootPath)
	if !isBucketRootMount {
		return nil
	}

	entry, err := filer_pb.GetEntry(ctx, filerClient, util.FullPath(bucketPath))
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
