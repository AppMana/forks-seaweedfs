package shell

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/filer/empty_folder_cleanup"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3_constants"
)

func init() {
	Commands = append(Commands, &commandS3BucketEmptyFolders{})
}

type commandS3BucketEmptyFolders struct {
}

func (c *commandS3BucketEmptyFolders) Name() string {
	return "s3.bucket.emptyfolders"
}

func (c *commandS3BucketEmptyFolders) Help() string {
	return `view or change whether a bucket keeps empty folders

	Example:
		# View the policy of a bucket
		s3.bucket.emptyfolders -name <bucket_name>

		# Remove empty implicit folders, as S3 does after the last object goes
		s3.bucket.emptyfolders -name <bucket_name> -remove

		# Keep empty folders; required for buckets that back mounts or volumes
		s3.bucket.emptyfolders -name <bucket_name> -keep

	The filer's empty folder cleaner only removes folders from buckets that
	explicitly opt in (-remove). Buckets created through the S3 API opt in at
	creation; buckets created for mounts and CSI volumes keep their folders,
	and so does any bucket without the attribute.
`
}

func (c *commandS3BucketEmptyFolders) HasTag(CommandTag) bool {
	return false
}

// emptyFolderPolicy is what a bucket entry says about its empty folders.
type emptyFolderPolicy string

const (
	emptyFolderPolicyKeep   emptyFolderPolicy = "keep"
	emptyFolderPolicyRemove emptyFolderPolicy = "remove"
)

// bucketEmptyFolderPolicy reads the policy off a bucket entry, mirroring the
// cleaner: only an explicit false removes folders.
func bucketEmptyFolderPolicy(entry *filer_pb.Entry) (policy emptyFolderPolicy, attribute string) {
	value, found := entry.GetExtended()[s3_constants.ExtAllowEmptyFolders]
	if !found {
		return emptyFolderPolicyKeep, "(unset)"
	}
	text := strings.TrimSpace(string(value))
	if strings.EqualFold(text, "false") {
		return emptyFolderPolicyRemove, text
	}
	return emptyFolderPolicyKeep, text
}

func (c *commandS3BucketEmptyFolders) Do(args []string, commandEnv *CommandEnv, writer io.Writer) (err error) {

	bucketCommand := flag.NewFlagSet(c.Name(), flag.ContinueOnError)
	bucketName := bucketCommand.String("name", "", "bucket name")
	keep := bucketCommand.Bool("keep", false, "keep empty folders")
	remove := bucketCommand.Bool("remove", false, "remove empty implicit folders once their last entry is deleted")
	if err = bucketCommand.Parse(args); err != nil {
		return nil
	}

	if *bucketName == "" {
		return fmt.Errorf("empty bucket name")
	}
	if *keep && *remove {
		return fmt.Errorf("cannot use both -keep and -remove flags together")
	}

	return commandEnv.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {

		resp, err := client.GetFilerConfiguration(context.Background(), &filer_pb.GetFilerConfigurationRequest{})
		if err != nil {
			return fmt.Errorf("get filer configuration: %w", err)
		}
		filerBucketsPath := resp.DirBuckets

		lookupResp, err := client.LookupDirectoryEntry(context.Background(), &filer_pb.LookupDirectoryEntryRequest{
			Directory: filerBucketsPath,
			Name:      *bucketName,
		})
		if err != nil {
			return fmt.Errorf("lookup bucket %s: %w", *bucketName, err)
		}
		entry := lookupResp.Entry
		if entry == nil {
			return fmt.Errorf("bucket %s not found", *bucketName)
		}

		if !*keep && !*remove {
			policy, attribute := bucketEmptyFolderPolicy(entry)
			fmt.Fprintf(writer, "Bucket: %s\nEmpty folders: %s (%s=%s)\n", *bucketName, policy, s3_constants.ExtAllowEmptyFolders, attribute)
			return nil
		}

		empty_folder_cleanup.SetBucketAllowEmptyFolders(entry, *keep)
		if _, err := client.UpdateEntry(context.Background(), &filer_pb.UpdateEntryRequest{
			Directory: filerBucketsPath,
			Entry:     entry,
		}); err != nil {
			return fmt.Errorf("failed to update bucket: %w", err)
		}
		policy, _ := bucketEmptyFolderPolicy(entry)
		fmt.Fprintf(writer, "Bucket %s empty folders: %s\n", *bucketName, policy)
		return nil
	})
}
