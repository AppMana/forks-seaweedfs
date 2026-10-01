package framework

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/test/volume_server/matrix"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
)

func TestMaintenanceReferencedNeedleRepair(t *testing.T) {
	if testing.Short() {
		t.Skip("requires isolated real master/filer/volume processes")
	}
	source := StartSingleVolumeClusterWithFiler(t, matrix.P1())
	target := StartSingleVolumeCluster(t, matrix.P1())
	client := NewHTTPClient()
	payload := bytes.Repeat([]byte("preserved original immutable Harbor-style object"), 4096)
	path := "/buckets/repair/item"
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, err := writer.CreateFormFile("file", "item")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	response, err := client.Post("http://"+source.FilerAddress()+path+"?collection=repair", writer.FormDataContentType(), &upload)
	if err != nil {
		t.Fatal(err)
	}
	out := ReadAllAndClose(t, response)
	if response.StatusCode != 201 {
		t.Fatalf("seed %d: %s", response.StatusCode, out)
	}
	response, err = client.Get("http://" + source.FilerAddress() + path + "?metadata=true")
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Chunks []struct {
			FileID string `json:"file_id"`
		} `json:"chunks"`
	}
	err = json.NewDecoder(response.Body).Decode(&metadata)
	response.Body.Close()
	if err != nil || len(metadata.Chunks) != 1 {
		t.Fatalf("metadata: %v %+v", err, metadata)
	}
	fid := metadata.Chunks[0].FileID
	parsed, err := needle.ParseFileIdFromString(fid)
	if err != nil {
		t.Fatal(err)
	}
	// The requested source record must not be the last index row: production
	// CopyFile responses batch many records, unlike a single-record fixture.
	sourceOther := NewFileID(uint32(parsed.VolumeId), uint64(parsed.Key)+2000, 789)
	response = UploadBytes(t, client, source.VolumeAdminURL(), sourceOther, bytes.Repeat([]byte("unrelated source record"), 128))
	ReadAllAndClose(t, response)
	if response.StatusCode != 201 {
		t.Fatalf("source index suffix seed: %d", response.StatusCode)
	}
	conn, tc := DialVolumeServer(t, target.VolumeGRPCAddress())
	defer conn.Close()
	AllocateVolume(t, tc, uint32(parsed.VolumeId), "repair")
	intactFID := NewFileID(uint32(parsed.VolumeId), uint64(parsed.Key)+1000, 456)
	intact := []byte("independent target-only record")
	response = UploadBytes(t, client, target.VolumeAdminURL(), intactFID, intact)
	ReadAllAndClose(t, response)
	if response.StatusCode != 201 {
		t.Fatal(response.StatusCode)
	}
	verify := func(fileID string, data []byte, code int) {
		t.Helper()
		resp := ReadBytes(t, client, target.VolumeAdminURL(), fileID)
		body := ReadAllAndClose(t, resp)
		if resp.StatusCode != code || (code == 200 && !bytes.Equal(body, data)) {
			t.Fatalf("read %s: status=%d bytes=%d", fileID, resp.StatusCode, len(body))
		}
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	run := func(digest string, apply bool, wantSuccess bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, source.weedBinary, "-config_dir="+source.configDir, "shell", "-master="+source.MasterAddress(), "-filer="+source.FilerServerAddress())
		cmd.Dir = source.baseDir
		input := fmt.Sprintf("lock\nvolume.repair.needle -path %s -fid %s -sha256 %s -source %s -target %s", path, fid, digest, source.VolumeServerAddress(), target.VolumeServerAddress())
		if apply {
			input += " -apply"
		}
		cmd.Stdin = strings.NewReader(input + "\nunlock\n")
		out, err := cmd.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("apply=%v success=%v: %v\n%s", apply, wantSuccess, err, out)
		}
	}
	run(strings.Repeat("0", 64), true, false)
	verify(fid, nil, 404)
	verify(intactFID, intact, 200)
	run(hash, false, true)
	verify(fid, nil, 404)
	verify(intactFID, intact, 200)
	run(hash, true, true)
	verify(fid, payload, 200)
	verify(intactFID, intact, 200)
	run(hash, true, false)
	verify(fid, payload, 200)
	verify(intactFID, intact, 200)
	target.RestartVolumeServer()
	verify(fid, payload, 200)
	verify(intactFID, intact, 200)
}
