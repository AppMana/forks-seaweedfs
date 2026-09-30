package copying_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Explicit intensive lane for run_image.py: one long-lived server and bucket,
// fixed live data, real multipart/copy/abort traffic and repeated vacuum. Never
// use the ordinary suite's collection-deletion cleanup to hide volume growth.
func TestS3QualificationSoak(t *testing.T) {
	raw := os.Getenv("SEAWEEDFS_SOAK_SECONDS")
	if raw == "" {
		t.Skip("explicit isolated-image soak lane required")
	}
	seconds, err := strconv.Atoi(raw)
	require.NoError(t, err)
	require.GreaterOrEqual(t, seconds, 1)
	require.LessOrEqual(t, seconds, 86400)
	require.Equal(t, "1", os.Getenv("SEAWEEDFS_ISOLATED_IMAGE"))
	require.Equal(t, "http://127.0.0.1:8333", defaultConfig.Endpoint)
	require.Equal(t, "http://127.0.0.1:9333", defaultConfig.MasterEndpoint)
	client := getS3Client(t)
	client = s3.New(client.Options(), func(options *s3.Options) {
		options.HTTPClient = &http.Client{Timeout: 30 * time.Second}
		options.RetryMaxAttempts = 1
	})
	conn, err := grpc.NewClient("127.0.0.1:19333", grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer conn.Close()
	master := master_pb.NewSeaweedClient(conn)
	bucket := getNewBucketName()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second+9*time.Minute)
	defer cancel()
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	// The owned container is removed by the runner; no privileged collection
	// cleanup or server restart occurs during this test, including on failure.
	put := func(key string, data []byte) {
		_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)})
		require.NoError(t, err)
	}
	verify := func(key string, expected []byte) {
		got, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		require.NoError(t, err)
		h := sha256.New()
		n, readErr := io.Copy(h, got.Body)
		closeErr := got.Body.Close()
		require.NoError(t, readErr)
		require.NoError(t, closeErr)
		require.EqualValues(t, len(expected), n)
		want := sha256.Sum256(expected)
		require.Equal(t, want[:], h.Sum(nil), key)
	}
	sentinel := generateRandomData(1 << 20)
	put("immutable", sentinel)
	started := time.Now()
	cycles := 0
	for time.Since(started) < time.Duration(seconds)*time.Second || cycles < 2 {
		payload := generateRandomData(10 << 20)
		put("source", payload)
		_, err = client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("copy"), CopySource: aws.String(bucket + "/source")})
		require.NoError(t, err)
		verify("copy", payload)
		begin, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("multipart")})
		require.NoError(t, err)
		parts := make([]types.CompletedPart, 0, 2)
		for i := 0; i < 2; i++ {
			part, err := client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("multipart"), UploadId: begin.UploadId,
				PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(payload[i*5<<20 : (i+1)*5<<20])})
			require.NoError(t, err)
			parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: part.ETag})
		}
		_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("multipart"), UploadId: begin.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
		require.NoError(t, err)
		aborted, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("multipart")})
		require.NoError(t, err)
		_, err = client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("multipart"), UploadId: aborted.UploadId,
			PartNumber: aws.Int32(1), Body: bytes.NewReader(payload[:5<<20])})
		require.NoError(t, err)
		_, err = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("multipart"), UploadId: aborted.UploadId})
		require.NoError(t, err)
		_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("copy")})
		require.NoError(t, err)
		// Let asynchronous chunk deletion run, but never retry payload checks.
		time.Sleep(time.Second)
		// The stock master schedules one vacuum per server and polls quota at
		// ten-second intervals. A multi-volume pass legitimately exceeds one
		// minute even with small data; do not mistake that for a hung volume.
		vacuumCtx, stop := context.WithTimeout(ctx, 4*time.Minute)
		_, err = master.VacuumVolume(vacuumCtx, &master_pb.VacuumVolumeRequest{Collection: bucket, GarbageThreshold: 0.1})
		stop()
		require.NoError(t, err)
		verify("immutable", sentinel)
		verify("source", payload)
		verify("multipart", payload)
		listing, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		require.NoError(t, err)
		require.EqualValues(t, 3, aws.ToInt32(listing.KeyCount))
		var volumeBytes int64
		err = filepath.WalkDir("/data", func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".dat") {
				return nil
			}
			info, err := entry.Info()
			if err == nil {
				volumeBytes += info.Size()
			}
			return err
		})
		require.NoError(t, err)
		require.Greater(t, volumeBytes, int64(0), "no real volume data observed")
		require.Less(t, volumeBytes, int64(384<<20), "fixed 21 MiB live payload must not consume the 512 MiB lab data filesystem")
		cycles++
		t.Logf("SOAK_CYCLE cycles=%d elapsed_seconds=%.1f live_bytes=%d volume_dat_bytes=%d", cycles, time.Since(started).Seconds(), 21<<20, volumeBytes)
	}
	fmt.Printf("SOAK_COMPLETE duration_seconds=%d cycles=%d\n", seconds, cycles)
}
