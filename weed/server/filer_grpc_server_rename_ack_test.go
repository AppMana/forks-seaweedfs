package weed_server

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"github.com/seaweedfs/seaweedfs/weed/util/log_buffer"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type renameAckStream struct {
	grpc.ServerStream
	onSend func(*filer_pb.StreamRenameEntryResponse) error
}

func renameLoggedEvents(t *testing.T, server *FilerServer) []*filer_pb.SubscribeMetadataResponse {
	t.Helper()
	if server.filer.LocalMetaLogBuffer.GetOffset() == 0 {
		return nil
	}
	buffer, _, pooled, err := server.filer.LocalMetaLogBuffer.ReadFromBuffer(log_buffer.NewMessagePosition(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if buffer == nil {
		return nil
	}
	if pooled {
		defer server.filer.LocalMetaLogBuffer.ReleaseMemory(buffer)
	}
	var events []*filer_pb.SubscribeMetadataResponse
	data := buffer.Bytes()
	for len(data) > 0 {
		if len(data) < 4 {
			t.Fatal("truncated log header")
		}
		n := int(util.BytesToUint32(data[:4]))
		if n > len(data)-4 {
			t.Fatal("truncated log entry")
		}
		entry := &filer_pb.LogEntry{}
		if err := proto.Unmarshal(data[4:4+n], entry); err != nil {
			t.Fatal(err)
		}
		event := &filer_pb.SubscribeMetadataResponse{}
		if err := proto.Unmarshal(entry.Data, event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
		data = data[4+n:]
	}
	return events
}

type renameMutateStream struct {
	grpc.ServerStream
	responses []*filer_pb.StreamMutateEntryResponse
}

func (s *renameMutateStream) Context() context.Context                          { return context.Background() }
func (s *renameMutateStream) Recv() (*filer_pb.StreamMutateEntryRequest, error) { return nil, io.EOF }
func (s *renameMutateStream) Send(r *filer_pb.StreamMutateEntryResponse) error {
	s.responses = append(s.responses, proto.Clone(r).(*filer_pb.StreamMutateEntryResponse))
	return nil
}

func TestStreamRenameRepliesMatchCommittedLog(t *testing.T) {
	for _, transport := range []string{"direct", "multiplexed", "disconnected"} {
		for _, shape := range []string{"file", "overwrite", "directory"} {
			t.Run(transport+"/"+shape, func(t *testing.T) {
				store := newRenameTestStore()
				store.entries["/src"] = newFileEntry("/src", 101)
				store.entries["/src"].Content = []byte("intact source")
				wantCount := 1
				if shape == "overwrite" {
					store.entries["/dst"] = newFileEntry("/dst", 202)
					wantCount = 2
				}
				if shape == "directory" {
					store.entries["/src"] = newDirectoryEntry("/src", 101)
					for _, name := range []string{"a", "b"} {
						store.entries["/src/"+name] = newFileEntry("/src/"+name, 303)
						store.entries["/src/"+name].Content = []byte(name)
					}
					wantCount = 3
				}
				server := &FilerServer{filer: newRenameTestFiler(t, store), entryLockTable: util.NewLockTable[util.FullPath]()}
				server.filer.Signature = 77
				req := &filer_pb.StreamRenameEntryRequest{OldDirectory: "/", OldName: "src", NewDirectory: "/", NewName: "dst", Signatures: []int32{88}}
				var replies []*filer_pb.StreamRenameEntryResponse
				if transport == "multiplexed" {
					wire := &renameMutateStream{}
					if err := server.handleStreamMutateRename(&syncStream{stream: wire}, 42, req); err != nil {
						t.Fatal(err)
					}
					if len(wire.responses) != wantCount+1 {
						t.Fatalf("responses=%d want=%d", len(wire.responses), wantCount+1)
					}
					for i, r := range wire.responses {
						if r.RequestId != 42 || r.Error != "" || r.IsLast != (i == wantCount) {
							t.Fatalf("invalid completion frame: %v", r)
						}
						if !r.IsLast {
							replies = append(replies, r.GetRenameResponse())
						}
					}
				} else {
					stream := &renameAckStream{onSend: func(r *filer_pb.StreamRenameEntryResponse) error {
						if transport == "disconnected" {
							return io.ErrClosedPipe
						}
						replies = append(replies, proto.Clone(r).(*filer_pb.StreamRenameEntryResponse))
						return nil
					}}
					err := server.StreamRenameEntry(req, stream)
					if transport == "disconnected" {
						if !errors.Is(err, io.ErrClosedPipe) {
							t.Fatalf("send error=%v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
				logged := renameLoggedEvents(t, server)
				if len(logged) != wantCount {
					t.Fatalf("logged=%d want=%d", len(logged), wantCount)
				}
				if transport != "disconnected" {
					if len(replies) != len(logged) {
						t.Fatalf("replies=%d log=%d", len(replies), len(logged))
					}
					for i, r := range replies {
						got := &filer_pb.SubscribeMetadataResponse{Directory: r.Directory, EventNotification: r.EventNotification, TsNs: r.TsNs}
						if !proto.Equal(got, logged[i]) {
							t.Fatalf("reply %d differs from actual logged event: %v vs %v", i, got, logged[i])
						}
						if r.TsNs == 0 || (i > 0 && r.TsNs <= replies[i-1].TsNs) {
							t.Fatalf("non-monotonic reply timestamp: %v", r)
						}
						if len(r.EventNotification.Signatures) != 2 || r.EventNotification.Signatures[0] != 88 || r.EventNotification.Signatures[1] != 77 {
							t.Fatalf("lost signatures: %v", r)
						}
					}
				}
				if shape == "overwrite" && (logged[0].EventNotification.OldEntry.Name != "dst" || logged[0].EventNotification.NewEntry != nil) {
					t.Fatal("overwrite must publish target deletion before rename")
				}
				if _, err := store.FindEntry(context.Background(), "/src"); err != filer_pb.ErrNotFound {
					t.Fatalf("source not vacated: %v", err)
				}
				dst, err := store.FindEntry(context.Background(), "/dst")
				if err != nil || dst.Inode != 101 {
					t.Fatalf("destination lost: %v %v", dst, err)
				}
				if shape != "directory" && string(dst.Content) != "intact source" {
					t.Fatal("source data changed")
				}
				if shape == "directory" {
					for _, name := range []string{"a", "b"} {
						e, err := store.FindEntry(context.Background(), util.FullPath("/dst/"+name))
						if err != nil || string(e.Content) != name {
							t.Fatalf("child %s lost: %v", name, err)
						}
					}
				}
			})
		}
	}
}

func (s *renameAckStream) Context() context.Context { return context.Background() }
func (s *renameAckStream) Send(resp *filer_pb.StreamRenameEntryResponse) error {
	return s.onSend(resp)
}

// A coarse Windows clock (or a backward clock step) can leave wall time behind
// the filer's monotonic event clock. Exercise the real server response against
// the real mount cache, not a hand-authored replacement rename event.
func TestStreamRenameAckSurvivesClockBehindMetadataFence(t *testing.T) {
	store := newRenameTestStore()
	server := &FilerServer{filer: newRenameTestFiler(t, store), entryLockTable: util.NewLockTable[util.FullPath]()}
	fence := server.filer.LocalDeliveredThroughTsNs(time.Now().Add(time.Hour).UnixNano())
	mapper, err := meta_cache.NewUidGidMapper("", "")
	if err != nil {
		t.Fatal(err)
	}
	cache := meta_cache.NewMetaCache(filepath.Join(t.TempDir(), "cache"), mapper, "/", false,
		func(util.FullPath) {}, func(util.FullPath) bool { return true },
		func(meta_cache.EntryInvalidation) {}, func(util.FullPath) {})
	defer cache.Shutdown()
	for iteration := 0; iteration < 3; iteration++ {
		source := newFileEntry("/config.lock", uint64(100+iteration))
		source.Content = []byte{byte(iteration + 1)}
		if err := store.InsertEntry(context.Background(), source); err != nil {
			t.Fatal(err)
		}
		// A successful lookup may already have fenced this source above wall time.
		if err := cache.InsertEntry(context.Background(), source, fence); err != nil {
			t.Fatal(err)
		}
		responses := 0
		stream := &renameAckStream{onSend: func(resp *filer_pb.StreamRenameEntryResponse) error {
			responses++
			if resp.TsNs <= fence {
				t.Errorf("rename acknowledgment timestamp %d does not exceed prior metadata fence %d", resp.TsNs, fence)
			}
			return cache.ApplyMetadataResponse(context.Background(), &filer_pb.SubscribeMetadataResponse{
				Directory: resp.Directory, EventNotification: resp.EventNotification, TsNs: resp.TsNs,
			}, meta_cache.LocalMetadataResponseApplyOptions)
		}}
		if err := server.StreamRenameEntry(&filer_pb.StreamRenameEntryRequest{OldDirectory: "/", OldName: "config.lock", NewDirectory: "/", NewName: "config"}, stream); err != nil {
			t.Fatal(err)
		}
		if responses == 0 {
			t.Fatal("successful rename delivered no acknowledgment")
		}
		if _, _, err := cache.FindEntry(context.Background(), "/config.lock"); err != filer_pb.ErrNotFound {
			t.Fatalf("iteration %d: successful rename left config.lock cached: %v", iteration, err)
		}
		entry, _, err := cache.FindEntry(context.Background(), "/config")
		if err != nil || entry == nil || len(entry.Content) != 1 || entry.Content[0] != byte(iteration+1) {
			t.Fatalf("iteration %d: destination content lost: %+v, %v", iteration, entry, err)
		}
		if _, err := store.FindEntry(context.Background(), "/config.lock"); err != filer_pb.ErrNotFound {
			t.Fatalf("source persisted after rename: %v", err)
		}
	}
}

func TestStreamRenameDoesNotAcknowledgeFailedCommit(t *testing.T) {
	store := newRenameTestStore()
	store.entries["/config.lock"] = newFileEntry("/config.lock", 100)
	store.commitErr = errors.New("injected commit failure")
	server := &FilerServer{filer: newRenameTestFiler(t, store), entryLockTable: util.NewLockTable[util.FullPath]()}
	responses := 0
	stream := &renameAckStream{onSend: func(*filer_pb.StreamRenameEntryResponse) error { responses++; return nil }}
	if err := server.StreamRenameEntry(&filer_pb.StreamRenameEntryRequest{OldDirectory: "/", OldName: "config.lock", NewDirectory: "/", NewName: "config"}, stream); err == nil {
		t.Fatal("expected commit failure")
	}
	if responses != 0 {
		t.Fatalf("failed commit published %d successful namespace changes", responses)
	}
	if events := renameLoggedEvents(t, server); len(events) != 0 {
		t.Fatalf("failed commit published %d log events", len(events))
	}
}

// Internal log files deliberately do not recursively generate metadata events,
// but their caller still needs a namespace acknowledgment after a successful move.
func TestStreamRenameAcknowledgesUnloggedPath(t *testing.T) {
	store := newRenameTestStore()
	dir := filer.SystemLogDir
	store.entries[dir] = newDirectoryEntry(dir, 100)
	store.entries[dir+"/src"] = newFileEntry(dir+"/src", 101)
	server := &FilerServer{filer: newRenameTestFiler(t, store), entryLockTable: util.NewLockTable[util.FullPath]()}
	var replies []*filer_pb.StreamRenameEntryResponse
	stream := &renameAckStream{onSend: func(r *filer_pb.StreamRenameEntryResponse) error { replies = append(replies, r); return nil }}
	if err := server.StreamRenameEntry(&filer_pb.StreamRenameEntryRequest{OldDirectory: dir, OldName: "src", NewDirectory: dir, NewName: "dst"}, stream); err != nil {
		t.Fatal(err)
	}
	if len(replies) != 1 {
		t.Fatalf("successful unlogged rename delivered %d acknowledgments, want 1", len(replies))
	}
	r := replies[0]
	if r.TsNs != 0 || r.Directory != dir || r.EventNotification.GetOldEntry().GetName() != "src" || r.EventNotification.GetNewEntry().GetName() != "dst" {
		t.Fatalf("invalid unversioned acknowledgment: %v", r)
	}
	if len(renameLoggedEvents(t, server)) != 0 {
		t.Fatal("internal log rename recursively generated metadata events")
	}
}
