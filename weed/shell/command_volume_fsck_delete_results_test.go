package shell

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
)

func TestFsckPurgeRejectsMissingDeleteResults(t *testing.T) {
	c := &commandVolumeFsck{
		writer: io.Discard,
		volumeServers: func(uint32) ([]pb.ServerAddress, bool) {
			return []pb.ServerAddress{"replica-a:8080", "replica-b:8080"}, true
		},
		deleteFileIds: func(pb.ServerAddress, []string) []*volume_server_pb.DeleteResult {
			return nil
		},
	}
	if err := c.purgeFileIdsForOneVolume(7, []string{"7,abc"}); err == nil {
		t.Fatal("missing delete results from every replica must make purge incomplete")
	}
}

func TestFsckPurgeValidatesEveryReplicaDeleteResult(t *testing.T) {
	fileIds := []string{"7,aaa", "7,bbb"}
	complete := func() []*volume_server_pb.DeleteResult {
		return []*volume_server_pb.DeleteResult{
			{FileId: fileIds[0], Status: http.StatusAccepted},
			{FileId: fileIds[1], Status: http.StatusNotModified},
		}
	}
	tests := []struct {
		name        string
		servers     []pb.ServerAddress
		delete      func(pb.ServerAddress, []string) []*volume_server_pb.DeleteResult
		wantErr     string
		wantSuccess bool
	}{
		{
			name:    "no replicas",
			servers: nil,
			delete:  func(pb.ServerAddress, []string) []*volume_server_pb.DeleteResult { return complete() },
			wantErr: "no volume servers found",
		},
		{
			name:    "partial response",
			servers: []pb.ServerAddress{"replica-a:8080"},
			delete: func(pb.ServerAddress, []string) []*volume_server_pb.DeleteResult {
				return complete()[:1]
			},
			wantErr: "returned 0 of 1 results",
		},
		{
			name:    "duplicate response",
			servers: []pb.ServerAddress{"replica-a:8080"},
			delete: func(pb.ServerAddress, []string) []*volume_server_pb.DeleteResult {
				return []*volume_server_pb.DeleteResult{
					{FileId: fileIds[0], Status: http.StatusAccepted},
					{FileId: fileIds[0], Status: http.StatusAccepted},
					{FileId: fileIds[1], Status: http.StatusAccepted},
				}
			},
			wantErr: "too many results",
		},
		{
			name:    "unknown response",
			servers: []pb.ServerAddress{"replica-a:8080"},
			delete: func(pb.ServerAddress, []string) []*volume_server_pb.DeleteResult {
				return append(complete(), &volume_server_pb.DeleteResult{FileId: "7,ccc", Status: http.StatusAccepted})
			},
			wantErr: "unexpected file",
		},
		{
			name:    "nil response element",
			servers: []pb.ServerAddress{"replica-a:8080"},
			delete: func(pb.ServerAddress, []string) []*volume_server_pb.DeleteResult {
				return []*volume_server_pb.DeleteResult{nil, {FileId: fileIds[1], Status: http.StatusAccepted}}
			},
			wantErr: "nil result",
		},
		{
			name:    "failure status without error text",
			servers: []pb.ServerAddress{"replica-a:8080"},
			delete: func(pb.ServerAddress, []string) []*volume_server_pb.DeleteResult {
				results := complete()
				results[0].Status = http.StatusInternalServerError
				return results
			},
			wantErr: "returned status 500",
		},
		{
			name:    "per-file error",
			servers: []pb.ServerAddress{"replica-a:8080"},
			delete: func(pb.ServerAddress, []string) []*volume_server_pb.DeleteResult {
				results := complete()
				results[0].Error = "disk refused delete"
				return results
			},
			wantErr: "disk refused delete",
		},
		{
			name:    "one replica silently misses response",
			servers: []pb.ServerAddress{"replica-a:8080", "replica-b:8080"},
			delete: func(server pb.ServerAddress, _ []string) []*volume_server_pb.DeleteResult {
				if server == "replica-b:8080" {
					return nil
				}
				return complete()
			},
			wantErr: "replica-b:8080 returned 0 of 1 results",
		},
		{
			name:        "all replicas acknowledge every file",
			servers:     []pb.ServerAddress{"replica-a:8080", "replica-b:8080"},
			delete:      func(pb.ServerAddress, []string) []*volume_server_pb.DeleteResult { return complete() },
			wantSuccess: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			c := &commandVolumeFsck{
				writer: &output,
				volumeServers: func(uint32) ([]pb.ServerAddress, bool) {
					return tt.servers, true
				},
				deleteFileIds: tt.delete,
			}
			err := c.purgeFileIdsForOneVolume(7, fileIds)
			if tt.wantSuccess {
				if err != nil {
					t.Fatalf("purge failed: %v\n%s", err, output.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("purge error = %v, want substring %q\n%s", err, tt.wantErr, output.String())
			}
		})
	}
}
