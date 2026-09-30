package copying_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/require"
)

// Exercise multiple full-sized parts through the running S3 endpoint. Aborting
// an in-progress replacement must not destroy the previously committed object.
func TestMultipartCompleteAndAbortPreservesObject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := getS3Client(t)
	bucket := getNewBucketName()
	createBucket(t, client, bucket)
	defer deleteBucket(t, client, bucket)
	key := "multipart-preservation"
	payload := generateRandomData(40 * 1024 * 1024)
	want := sha256.Sum256(payload)
	begin := func() string {
		upload, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(bucket), Key: aws.String(key),
		})
		require.NoError(t, err)
		require.NotEmpty(t, aws.ToString(upload.UploadId))
		return *upload.UploadId
	}
	uploadID := begin()
	parts := make([]types.CompletedPart, 0, 5)
	for i := 0; i < 5; i++ {
		part, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(payload[i*8*1024*1024 : (i+1)*8*1024*1024]),
		})
		require.NoError(t, err)
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: part.ETag})
	}
	_, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	require.NoError(t, err)
	verify := func() {
		object, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		require.NoError(t, err)
		defer object.Body.Close()
		hash := sha256.New()
		size, err := io.Copy(hash, object.Body)
		require.NoError(t, err)
		require.Equal(t, int64(len(payload)), size)
		require.Equal(t, want[:], hash.Sum(nil))
	}
	verify()
	uploadID = begin()
	_, err = client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
		PartNumber: aws.Int32(1), Body: bytes.NewReader([]byte("abandoned replacement")),
	})
	require.NoError(t, err)
	_, err = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	require.NoError(t, err)
	_, err = client.ListParts(ctx, &s3.ListPartsInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	var apiError smithy.APIError
	require.True(t, errors.As(err, &apiError), "expected NoSuchUpload, got %v", err)
	require.Equal(t, "NoSuchUpload", apiError.ErrorCode())
	verify()
}
