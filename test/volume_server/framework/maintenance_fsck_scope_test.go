package framework

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/test/volume_server/matrix"
)

// Bucket metadata may legitimately reference chunks in another collection.
// The isolated fixture exercises the actual CLI, including its dangerous
// missing-volume purge option, and requires intact original bytes to survive.
func TestMaintenanceFsckCrossCollectionReferences(t *testing.T) {
	if testing.Short() {
		t.Skip("starts isolated master, volume and filer binaries")
	}
	c := StartSingleVolumeClusterWithFiler(t, matrix.P1())
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	payload := bytes.Repeat([]byte("original-cross-collection-data"), 4096)
	for _, name := range []string{"local", "foreign"} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		collection := "scope-test"
		if name == "foreign" {
			collection = "outside-scope"
		}
		resp, err := client.Post("http://"+c.FilerAddress()+"/buckets/scope-test/"+name+"?collection="+collection, writer.FormDataContentType(), &body)
		if err != nil {
			t.Fatal(err)
		}
		out, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil || resp.StatusCode >= 300 {
			t.Fatalf("upload: %v %d %s", readErr, resp.StatusCode, out)
		}
	}
	for _, apply := range []bool{false, true} {
		input := "lock\nvolume.fsck -collection scope-test -findMissingChunksInFiler -cutoffTimeAgo=0"
		if apply {
			input += " -reallyDeleteFilerEntries"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, c.weedBinary, "-config_dir="+c.configDir, "shell", "-master="+c.MasterAddress(), "-filer="+c.FilerServerAddress())
		cmd.Dir = c.baseDir
		cmd.Stdin = strings.NewReader(input + "\nunlock\n")
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil || strings.Contains(string(out), "volume not found") {
			t.Errorf("apply=%v: valid foreign collection reference rejected: %v\n%s", apply, err, out)
		}
		for _, name := range []string{"local", "foreign"} {
			resp, err := client.Get("http://" + c.FilerAddress() + "/buckets/scope-test/" + name)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil || resp.StatusCode != 200 || !bytes.Equal(body, payload) {
				t.Errorf("apply=%v original %s lost: status=%d read=%v bytes=%d", apply, name, resp.StatusCode, readErr, len(body))
			}
		}
	}
	// Negative control: do not pass by merely ignoring foreign references.
	// Remove only this disposable fixture's physical chunk, leaving metadata.
	resp, err := client.Get("http://" + c.FilerAddress() + "/buckets/scope-test/foreign?metadata=true")
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Chunks []struct {
			FileID string `json:"file_id"`
		} `json:"chunks"`
	}
	err = json.NewDecoder(resp.Body).Decode(&metadata)
	resp.Body.Close()
	if err != nil || len(metadata.Chunks) != 1 {
		t.Fatalf("fixture metadata: %v %+v", err, metadata)
	}
	req, err := http.NewRequest(http.MethodDelete, "http://"+c.VolumeAdminAddress()+"/"+metadata.Chunks[0].FileID, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("delete fixture chunk: %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.weedBinary, "-config_dir="+c.configDir, "shell", "-master="+c.MasterAddress(), "-filer="+c.FilerServerAddress())
	cmd.Dir = c.baseDir
	cmd.Stdin = strings.NewReader("lock\nvolume.fsck -collection scope-test -findMissingChunksInFiler -cutoffTimeAgo=0\nunlock\n")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "/buckets/scope-test/foreign") || strings.Contains(string(out), "volume not found") {
		t.Fatalf("actual missing foreign chunk must be reported, not a missing volume: %v\n%s", err, out)
	}
}
