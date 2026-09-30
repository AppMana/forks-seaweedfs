package example

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// With -s3.shareCopyChunks a whole-object CopyObject within an unversioned
// bucket gives the destination the source's chunks: no bytes move, and the
// chunks live until the last object using them is deleted. The Harbor blob
// commit (multipart upload to _uploads/<id>/data, CopyObject to
// blobs/sha256/<digest>/data, delete the upload) is the workload it is for.

func TestS3CopySharesChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	cluster, err := startMiniCluster(t, "-s3.shareCopyChunks=true")
	require.NoError(t, err)
	defer cluster.Stop()
	checker := newSharedCopyChecker(t, cluster)
	checker.waitForOwnerRing()

	t.Run("CopySharesTheSourceChunks", func(t *testing.T) {
		c := checker.with(t)
		bucket := createTestBucket(t, cluster, "share-")
		data := randomBytes(t, 20<<20)
		c.put(bucket, "src", data)
		c.copy(bucket, "src", bucket, "dst")

		srcFids := c.fids(bucket, "src")
		require.NotEmpty(t, srcFids)
		require.Equal(t, srcFids, c.fids(bucket, "dst"), "destination was given new chunks: the copy moved bytes")
		require.Equal(t, data, c.get(bucket, "dst"))
		require.Equal(t, data, c.get(bucket, "src"))
	})

	t.Run("HarborBlobCommit", func(t *testing.T) {
		c := checker.with(t)
		bucket := createTestBucket(t, cluster, "harbor-")
		const blobs = 8
		type blob struct {
			data  []byte
			key   string
			fids  []string
			upKey string
		}
		var committed []blob
		for i := 0; i < blobs; i++ {
			parts := [][]byte{randomBytes(t, 5<<20), randomBytes(t, 5<<20), randomBytes(t, 1<<20+int64(i)*4099)}
			data := bytes.Join(parts, nil)
			sum := sha256.Sum256(data)
			upKey := fmt.Sprintf("docker/registry/v2/repositories/lib/app/_uploads/%s/data", randomString(16))
			key := fmt.Sprintf("docker/registry/v2/blobs/sha256/%s/%s/data", hex.EncodeToString(sum[:1]), hex.EncodeToString(sum[:]))
			c.multipartPut(bucket, upKey, parts)
			upFids := c.fids(bucket, upKey)
			c.copy(bucket, upKey, bucket, key)
			require.Equal(t, upFids, c.fids(bucket, key), "blob %d: commit copied bytes", i)
			c.delete(bucket, upKey)
			committed = append(committed, blob{data: data, key: key, fids: upFids, upKey: upKey})
		}
		c.waitForDeletions(bucket)

		for i, b := range committed {
			c.requireMissing(bucket, b.upKey)
			require.True(t, c.allChunksPresent(b.fids), "blob %d: deleting the upload freed the committed blob's chunks", i)
			got := c.get(bucket, b.key)
			require.Equal(t, len(b.data), len(got), "blob %d size", i)
			require.True(t, bytes.Equal(b.data, got), "blob %d: committed blob differs from the upload", i)
			for _, r := range [][2]int64{{0, 0}, {5<<20 - 7, 5<<20 + 7}, {10 << 20, int64(len(b.data)) - 1}, {int64(len(b.data)) - 1, int64(len(b.data)) - 1}} {
				require.Equal(t, b.data[r[0]:r[1]+1], c.getRange(bucket, b.key, r[0], r[1]), "blob %d range %d-%d", i, r[0], r[1])
			}
		}
	})

	t.Run("DeletingEveryLinkFreesTheChunks", func(t *testing.T) {
		c := checker.with(t)
		bucket := createTestBucket(t, cluster, "free-")
		c.put(bucket, "a", randomBytes(t, 12<<20))
		c.copy(bucket, "a", bucket, "b")
		c.copy(bucket, "b", bucket, "c")
		fids := c.fids(bucket, "a")
		group := c.sharedGroup(bucket, "a")
		require.NotEmpty(t, group, "source was not marked as sharing its chunks")
		require.Equal(t, 3, c.groupReferences(group))

		c.delete(bucket, "b")
		c.delete(bucket, "a")
		c.waitForDeletions(bucket)
		require.True(t, c.allChunksPresent(fids), "chunks freed while c still uses them")
		require.Equal(t, 1, c.groupReferences(group))

		c.delete(bucket, "c")
		require.Eventually(t, func() bool { return c.noChunkPresent(fids) }, 30*time.Second, 200*time.Millisecond, "chunks of the last link were never freed")
		require.Equal(t, 0, c.groupReferences(group), "group references outlived every member")
	})

	t.Run("OverwritingTheSourceKeepsTheDestination", func(t *testing.T) {
		c := checker.with(t)
		bucket := createTestBucket(t, cluster, "over-")
		data := randomBytes(t, 9<<20)
		c.put(bucket, "src", data)
		c.copy(bucket, "src", bucket, "dst")
		fids := c.fids(bucket, "src")
		c.put(bucket, "src", randomBytes(t, 3<<20))
		c.waitForDeletions(bucket)
		require.True(t, c.allChunksPresent(fids))
		require.Equal(t, data, c.get(bucket, "dst"))
	})

	t.Run("CopiesRacingOverwritesAndDeletesOfTheSource", func(t *testing.T) {
		c := checker.with(t)
		bucket := createTestBucket(t, cluster, "race-")
		versions := make([][]byte, 6)
		for i := range versions {
			versions[i] = randomBytes(t, 9<<20)
		}
		c.put(bucket, "src", versions[0])

		var wg sync.WaitGroup
		var writerDone atomic.Bool
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer writerDone.Store(true)
			for _, data := range versions[1:] {
				if _, err := cluster.s3Client.PutObject(&s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("src"), Body: bytes.NewReader(data)}); err != nil {
					t.Errorf("overwrite: %v", err)
				}
				if _, err := cluster.s3Client.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("src")}); err != nil {
					t.Errorf("delete: %v", err)
				}
			}
		}()
		var mu sync.Mutex
		var copied []string
		for w := 0; w < 6; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for n := 0; !writerDone.Load(); n++ {
					key := fmt.Sprintf("dst-%d-%d", w, n)
					// a copy of a source deleted under it fails; one that succeeds
					// must hold a whole version
					if _, err := cluster.s3Client.CopyObject(&s3.CopyObjectInput{
						Bucket: aws.String(bucket), Key: aws.String(key), CopySource: aws.String(bucket + "/src"),
					}); err == nil {
						mu.Lock()
						copied = append(copied, key)
						mu.Unlock()
					}
				}
			}(w)
		}
		wg.Wait()
		c.waitForDeletions(bucket)
		require.NotEmpty(t, copied)
		for _, key := range copied {
			got := c.get(bucket, key)
			found := false
			for _, data := range versions {
				if bytes.Equal(got, data) {
					found = true
					break
				}
			}
			require.True(t, found, "%s holds no version of the source (%d bytes)", key, len(got))
		}
		t.Logf("%d copies succeeded while the source was overwritten and deleted %d times", len(copied), len(versions)-1)
	})

	t.Run("DeletingTheBucketDropsItsGroups", func(t *testing.T) {
		c := checker.with(t)
		bucket := createTestBucket(t, cluster, "drop-")
		c.put(bucket, "src", randomBytes(t, 9<<20))
		c.copy(bucket, "src", bucket, "dst")
		group := c.sharedGroup(bucket, "src")
		require.Equal(t, 2, c.groupReferences(group))
		_, err := cluster.s3Client.DeleteBucket(&s3.DeleteBucketInput{Bucket: aws.String(bucket)})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return c.groupReferences(group) == 0 }, 30*time.Second, 200*time.Millisecond,
			"references of a deleted bucket's objects outlived it")
	})

	t.Run("ReplacedMetadataStaysWithItsObject", func(t *testing.T) {
		c := checker.with(t)
		bucket := createTestBucket(t, cluster, "meta-")
		_, err := cluster.s3Client.PutObject(&s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String("src"), Body: bytes.NewReader(randomBytes(t, 9<<20)),
			ContentType: aws.String("application/x-src"), Metadata: map[string]*string{"Side": aws.String("source")},
		})
		require.NoError(t, err)
		_, err = cluster.s3Client.CopyObject(&s3.CopyObjectInput{
			Bucket: aws.String(bucket), Key: aws.String("dst"), CopySource: aws.String(bucket + "/src"),
			MetadataDirective: aws.String("REPLACE"), ContentType: aws.String("application/x-dst"),
			Metadata: map[string]*string{"Side": aws.String("destination")},
		})
		require.NoError(t, err)
		require.Equal(t, c.fids(bucket, "src"), c.fids(bucket, "dst"))
		for key, want := range map[string]string{"src": "source", "dst": "destination"} {
			head, err := cluster.s3Client.HeadObject(&s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			require.NoError(t, err)
			require.Equal(t, want, aws.StringValue(head.Metadata["Side"]), key)
			require.Equal(t, "application/x-"+key, aws.StringValue(head.ContentType), key)
		}
	})

	t.Run("CrossBucketCopyCopiesBytes", func(t *testing.T) {
		c := checker.with(t)
		src := createTestBucket(t, cluster, "xsrc-")
		dst := createTestBucket(t, cluster, "xdst-")
		data := randomBytes(t, 9<<20)
		c.put(src, "obj", data)
		// a member of a group: its marker must not travel with a byte copy
		c.copy(src, "obj", src, "link")
		require.NotEmpty(t, c.sharedGroup(src, "obj"))
		c.copy(src, "obj", dst, "obj")
		c.requireDisjoint(c.fids(src, "obj"), c.fids(dst, "obj"))
		require.Empty(t, c.sharedGroup(dst, "obj"), "a byte copy joined the source's group")
		require.Equal(t, data, c.get(dst, "obj"))
	})

	t.Run("VersionedBucketCopyCopiesBytes", func(t *testing.T) {
		c := checker.with(t)
		bucket := createTestBucket(t, cluster, "ver-")
		_, err := cluster.s3Client.PutBucketVersioning(&s3.PutBucketVersioningInput{
			Bucket:                  aws.String(bucket),
			VersioningConfiguration: &s3.VersioningConfiguration{Status: aws.String("Enabled")},
		})
		require.NoError(t, err)
		data := randomBytes(t, 9<<20)
		c.put(bucket, "src", data)
		c.copy(bucket, "src", bucket, "dst")
		require.Equal(t, data, c.get(bucket, "dst"))
		srcFids := c.fidsUnder(fmt.Sprintf("/buckets/%s/src.versions", bucket))
		require.NotEmpty(t, srcFids)
		c.requireDisjoint(srcFids, c.fidsUnder(fmt.Sprintf("/buckets/%s/dst.versions", bucket)))
	})

	t.Run("EncryptedCopyCopiesBytes", func(t *testing.T) {
		c := checker.with(t)
		bucket := createTestBucket(t, cluster, "ssec-")
		key, keyMD5 := generateSSECKey()
		data := randomBytes(t, 9<<20)
		_, err := putObjectSSEC(cluster.s3Client, &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String("src"), Body: bytes.NewReader(data),
			SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(key), SSECustomerKeyMD5: aws.String(keyMD5),
		})
		require.NoError(t, err)
		copyReq, _ := cluster.s3Client.CopyObjectRequest(&s3.CopyObjectInput{
			Bucket: aws.String(bucket), Key: aws.String("dst"), CopySource: aws.String(bucket + "/src"),
			CopySourceSSECustomerAlgorithm: aws.String("AES256"), CopySourceSSECustomerKey: aws.String(key), CopySourceSSECustomerKeyMD5: aws.String(keyMD5),
			SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(key), SSECustomerKeyMD5: aws.String(keyMD5),
		})
		copyReq.Handlers.Validate.Clear()
		require.NoError(t, copyReq.Send())
		require.Empty(t, c.sharedGroup(bucket, "src"), "an encrypted source was marked")
		require.Empty(t, c.sharedGroup(bucket, "dst"))
		c.requireDisjoint(c.fids(bucket, "src"), c.fids(bucket, "dst"))
		getReq, out := cluster.s3Client.GetObjectRequest(&s3.GetObjectInput{
			Bucket: aws.String(bucket), Key: aws.String("dst"),
			SSECustomerAlgorithm: aws.String("AES256"), SSECustomerKey: aws.String(key), SSECustomerKeyMD5: aws.String(keyMD5),
		})
		getReq.Handlers.Validate.Clear()
		require.NoError(t, getReq.Send())
		got, err := io.ReadAll(out.Body)
		out.Body.Close()
		require.NoError(t, err)
		require.Equal(t, data, got)
	})

	t.Run("SmallInlineObjectCopies", func(t *testing.T) {
		c := checker.with(t)
		bucket := createTestBucket(t, cluster, "tiny-")
		c.put(bucket, "src", []byte("tiny"))
		c.copy(bucket, "src", bucket, "dst")
		require.Equal(t, []byte("tiny"), c.get(bucket, "dst"))
		c.delete(bucket, "src")
		require.Equal(t, []byte("tiny"), c.get(bucket, "dst"))
	})
}

// TestS3CopySharedChunksPerformance bounds what sharing costs and saves: a
// shared copy takes metadata time regardless of size, and reading or listing
// a shared object costs what a plain object does.
func TestS3CopySharedChunksPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	cluster, err := startMiniCluster(t, "-s3.shareCopyChunks=true", "-master.volumeSizeLimitMB=64")
	require.NoError(t, err)
	defer cluster.Stop()
	c := newSharedCopyChecker(t, cluster)
	c.waitForOwnerRing()
	bucket := createTestBucket(t, cluster, "perf-")

	small := randomBytes(t, 8<<20)
	large := randomBytes(t, 256<<20)
	c.put(bucket, "small", small)
	start := time.Now()
	c.put(bucket, "large", large)
	putLarge := time.Since(start)

	copyTime := func(src, dst string) time.Duration {
		best := time.Duration(1 << 62)
		for i := 0; i < 5; i++ {
			start := time.Now()
			c.copy(bucket, src, bucket, fmt.Sprintf("%s-%d", dst, i))
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}
	copySmall := copyTime("small", "small-copy")
	copyLarge := copyTime("large", "large-copy")
	t.Logf("PUT 256 MiB %v; shared copy 8 MiB %v, 256 MiB %v", putLarge, copySmall, copyLarge)
	require.Equal(t, c.fids(bucket, "large"), c.fids(bucket, "large-copy-0"))
	// the copy writes metadata only: 32x the bytes may not cost a byte copy
	require.Less(t, copyLarge, putLarge/8, "copying 256 MiB took a sizeable part of writing it")
	require.Less(t, copyLarge, 4*copySmall+50*time.Millisecond, "copy time grows with object size")

	c.put(bucket, "plain", small)
	medianOf := func(d []time.Duration) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[len(d)/2]
	}
	median := func(fn func()) time.Duration {
		var d []time.Duration
		for i := 0; i < 41; i++ {
			start := time.Now()
			fn()
			d = append(d, time.Since(start))
		}
		return medianOf(d)
	}
	// pairs times a and b alternately, so drift in the machine's load falls
	// on both alike
	pairs := func(a, b func()) (time.Duration, time.Duration) {
		var da, db []time.Duration
		for i := 0; i < 41; i++ {
			start := time.Now()
			a()
			da = append(da, time.Since(start))
			start = time.Now()
			b()
			db = append(db, time.Since(start))
		}
		return medianOf(da), medianOf(db)
	}
	head := func(key string) func() {
		return func() {
			_, err := cluster.s3Client.HeadObject(&s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			require.NoError(t, err)
		}
	}
	get := func(key string) func() { return func() { c.get(bucket, key) } }
	headPlain, headShared := pairs(head("plain"), head("small-copy-0"))
	getPlain, getShared := pairs(get("plain"), get("small-copy-0"))
	var listed int64
	listTime := median(func() {
		out, err := cluster.s3Client.ListObjectsV2(&s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
		require.NoError(t, err)
		listed = aws.Int64Value(out.KeyCount)
	})
	t.Logf("HEAD plain %v shared %v; GET 8 MiB plain %v shared %v; LIST %d objects %v", headPlain, headShared, getPlain, getShared, listed, listTime)
	require.EqualValues(t, 13, listed, "the listing shows something besides the objects")
	require.Less(t, headShared, 2*headPlain+2*time.Millisecond, "HEAD of a shared object is slower than of a plain one")
	require.Less(t, getShared, 2*getPlain+5*time.Millisecond, "GET of a shared object is slower than of a plain one")
}

type sharedCopyChecker struct {
	t       *testing.T
	cluster *TestCluster
	filer   filer_pb.SeaweedFilerClient
	http    *http.Client
}

func newSharedCopyChecker(t *testing.T, cluster *TestCluster) *sharedCopyChecker {
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", cluster.filerPort+10000), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return &sharedCopyChecker{t: t, cluster: cluster, filer: filer_pb.NewSeaweedFilerClient(conn), http: &http.Client{Timeout: 10 * time.Second}}
}

// with is c reporting to t, for use in a subtest.
func (c *sharedCopyChecker) with(t *testing.T) *sharedCopyChecker {
	copied := *c
	copied.t = t
	return &copied
}

// waitForOwnerRing waits until a copy shares its source's chunks: the gateway
// links a copy only once it knows the filer that owns the source's writes, and
// learns that from the master shortly after it starts.
func (c *sharedCopyChecker) waitForOwnerRing() {
	bucket := createTestBucket(c.t, c.cluster, "ring-")
	c.put(bucket, "src", randomBytes(c.t, 1<<20))
	n := 0
	require.Eventually(c.t, func() bool {
		n++
		dst := fmt.Sprintf("dst%d", n)
		c.copy(bucket, "src", bucket, dst)
		return c.sharedGroup(bucket, dst) != ""
	}, 30*time.Second, 100*time.Millisecond, "copies never shared chunks")
}

func randomBytes(t *testing.T, n int64) []byte {
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func (c *sharedCopyChecker) put(bucket, key string, data []byte) {
	_, err := c.cluster.s3Client.PutObject(&s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)})
	require.NoError(c.t, err, "put %s/%s", bucket, key)
}

func (c *sharedCopyChecker) multipartPut(bucket, key string, parts [][]byte) {
	up, err := c.cluster.s3Client.CreateMultipartUpload(&s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(c.t, err)
	var completed []*s3.CompletedPart
	for i, part := range parts {
		out, err := c.cluster.s3Client.UploadPart(&s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: up.UploadId,
			PartNumber: aws.Int64(int64(i + 1)), Body: bytes.NewReader(part),
		})
		require.NoError(c.t, err)
		completed = append(completed, &s3.CompletedPart{ETag: out.ETag, PartNumber: aws.Int64(int64(i + 1))})
	}
	_, err = c.cluster.s3Client.CompleteMultipartUpload(&s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: up.UploadId,
		MultipartUpload: &s3.CompletedMultipartUpload{Parts: completed},
	})
	require.NoError(c.t, err)
}

func (c *sharedCopyChecker) copy(srcBucket, srcKey, dstBucket, dstKey string) {
	_, err := c.cluster.s3Client.CopyObject(&s3.CopyObjectInput{
		Bucket: aws.String(dstBucket), Key: aws.String(dstKey), CopySource: aws.String(srcBucket + "/" + srcKey),
	})
	require.NoError(c.t, err, "copy %s/%s to %s/%s", srcBucket, srcKey, dstBucket, dstKey)
}

func (c *sharedCopyChecker) delete(bucket, key string) {
	_, err := c.cluster.s3Client.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(c.t, err, "delete %s/%s", bucket, key)
}

func (c *sharedCopyChecker) get(bucket, key string) []byte {
	out, err := c.cluster.s3Client.GetObject(&s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.NoError(c.t, err, "get %s/%s", bucket, key)
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	require.NoError(c.t, err)
	return b
}

func (c *sharedCopyChecker) getRange(bucket, key string, first, last int64) []byte {
	out, err := c.cluster.s3Client.GetObject(&s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Range: aws.String(fmt.Sprintf("bytes=%d-%d", first, last))})
	require.NoError(c.t, err)
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	require.NoError(c.t, err)
	return b
}

func (c *sharedCopyChecker) requireMissing(bucket, key string) {
	_, err := c.cluster.s3Client.HeadObject(&s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	require.Error(c.t, err, "%s/%s still exists", bucket, key)
}

func (c *sharedCopyChecker) entry(dir, name string) *filer_pb.Entry {
	resp, err := c.filer.LookupDirectoryEntry(context.Background(), &filer_pb.LookupDirectoryEntryRequest{Directory: dir, Name: name})
	require.NoError(c.t, err, "lookup %s/%s", dir, name)
	return resp.Entry
}

func chunkFids(entries ...*filer_pb.Entry) []string {
	var fids []string
	for _, entry := range entries {
		for _, chunk := range entry.GetChunks() {
			fids = append(fids, chunk.GetFileIdString())
		}
	}
	sort.Strings(fids)
	return fids
}

func (c *sharedCopyChecker) fidsAt(dir, name string) []string {
	return chunkFids(c.entry(dir, name))
}

// fidsUnder collects the chunks of every entry in dir.
func (c *sharedCopyChecker) fidsUnder(dir string) []string {
	return chunkFids(c.list(dir)...)
}

func (c *sharedCopyChecker) list(dir string) []*filer_pb.Entry {
	stream, err := c.filer.ListEntries(context.Background(), &filer_pb.ListEntriesRequest{Directory: dir, Limit: 1000})
	require.NoError(c.t, err)
	var entries []*filer_pb.Entry
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return entries
		}
		require.NoError(c.t, err, "list %s", dir)
		entries = append(entries, resp.Entry)
	}
}

func (c *sharedCopyChecker) fids(bucket, key string) []string {
	dir, name := util.NewFullPath("/buckets/"+bucket, key).DirAndName()
	return c.fidsAt(dir, name)
}

// sharedGroup is the group the object's marker names, or "".
func (c *sharedCopyChecker) sharedGroup(bucket, key string) string {
	dir, name := util.NewFullPath("/buckets/"+bucket, key).DirAndName()
	ref, ok := filer.ParseSharedChunksRef(c.entry(dir, name).GetExtended())
	if !ok {
		return ""
	}
	return ref.Group
}

func (c *sharedCopyChecker) groupReferences(group string) int {
	return len(c.list(filer.SharedChunksRefDir + "/" + group))
}

func (c *sharedCopyChecker) chunkPresent(fid string) bool {
	resp, err := c.http.Get(fmt.Sprintf("http://127.0.0.1:%d/%s", c.cluster.volumePort, fid))
	require.NoError(c.t, err)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		return true
	case http.StatusNotFound:
		return false
	}
	c.t.Fatalf("chunk %s: unexpected status %d", fid, resp.StatusCode)
	return false
}

func (c *sharedCopyChecker) allChunksPresent(fids []string) bool {
	for _, fid := range fids {
		if !c.chunkPresent(fid) {
			return false
		}
	}
	return true
}

func (c *sharedCopyChecker) noChunkPresent(fids []string) bool {
	for _, fid := range fids {
		if c.chunkPresent(fid) {
			return false
		}
	}
	return true
}

func (c *sharedCopyChecker) requireDisjoint(a, b []string) {
	seen := map[string]bool{}
	for _, fid := range a {
		seen[fid] = true
	}
	for _, fid := range b {
		require.False(c.t, seen[fid], "chunk %s is shared", fid)
	}
}

// waitForDeletions deletes a canary object and waits for its chunks to be
// freed: the filer frees chunks in the order their deletes were queued, so
// every delete queued before it has been carried out too.
func (c *sharedCopyChecker) waitForDeletions(bucket string) {
	key := "canary-" + randomString(8)
	c.put(bucket, key, randomBytes(c.t, 1<<20))
	fids := c.fids(bucket, key)
	c.delete(bucket, key)
	require.Eventually(c.t, func() bool { return c.noChunkPresent(fids) }, 30*time.Second, 100*time.Millisecond, "canary chunks were never freed")
}
