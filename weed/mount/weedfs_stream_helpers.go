package mount

import (
	"context"
	"errors"
	"syscall"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// streamCreateEntry routes a CreateEntryRequest through the streaming mux
// if available, falling back to a unary gRPC call on transport errors.
func (wfs *WFS) streamCreateEntry(ctx context.Context, req *filer_pb.CreateEntryRequest) (*filer_pb.CreateEntryResponse, error) {
	// One deadline across the stream attempt and the fallback.
	ctx, cancel := filerReplyContext(ctx)
	defer cancel()
	if wfs.streamMutate != nil && wfs.streamMutate.IsAvailable() {
		resp, err := wfs.streamMutate.CreateEntry(ctx, req)
		if err == nil || !errors.Is(err, ErrStreamTransport) {
			return resp, asFilerTimeout("create", err) // success or application error — don't retry
		}
		glog.V(1).Infof("streamCreateEntry %s/%s: stream failed, falling back to unary: %v", req.Directory, req.Entry.Name, err)
	}
	return awaitFilerReply(ctx, "create", func() (*filer_pb.CreateEntryResponse, error) {
		var resp *filer_pb.CreateEntryResponse
		err := wfs.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
			var err error
			resp, err = filer_pb.CreateEntryWithResponse(context.Background(), client, req)
			return err
		})
		return resp, err
	})
}

// streamUpdateEntry routes an UpdateEntryRequest through the streaming mux
// if available, falling back to a unary gRPC call on transport errors.
func (wfs *WFS) streamUpdateEntry(ctx context.Context, req *filer_pb.UpdateEntryRequest) (*filer_pb.UpdateEntryResponse, error) {
	ctx, cancel := filerReplyContext(ctx)
	defer cancel()
	if wfs.streamMutate != nil && wfs.streamMutate.IsAvailable() {
		resp, err := wfs.streamMutate.UpdateEntry(ctx, req)
		if err == nil || !errors.Is(err, ErrStreamTransport) {
			return resp, asFilerTimeout("update", err)
		}
		glog.V(1).Infof("streamUpdateEntry %s/%s: stream failed, falling back to unary: %v", req.Directory, req.Entry.Name, err)
	}
	return awaitFilerReply(ctx, "update", func() (*filer_pb.UpdateEntryResponse, error) {
		var resp *filer_pb.UpdateEntryResponse
		err := wfs.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
			var err error
			resp, err = client.UpdateEntry(context.Background(), req)
			return err
		})
		return resp, err
	})
}

// streamDeleteEntry routes a DeleteEntryRequest through the streaming mux
// if available, falling back to a unary gRPC call on transport errors.
func (wfs *WFS) streamDeleteEntry(ctx context.Context, req *filer_pb.DeleteEntryRequest) (*filer_pb.DeleteEntryResponse, error) {
	ctx, cancel := filerReplyContext(ctx)
	defer cancel()
	if wfs.streamMutate != nil && wfs.streamMutate.IsAvailable() {
		resp, err := wfs.streamMutate.DeleteEntry(ctx, req)
		if err == nil || !errors.Is(err, ErrStreamTransport) {
			return resp, asFilerTimeout("delete", err)
		}
		glog.V(1).Infof("streamDeleteEntry %s/%s: stream failed, falling back to unary: %v", req.Directory, req.Name, err)
	}
	return awaitFilerReply(ctx, "delete", func() (*filer_pb.DeleteEntryResponse, error) {
		return wfs.unaryDeleteEntry(req)
	})
}

func (wfs *WFS) unaryDeleteEntry(req *filer_pb.DeleteEntryRequest) (*filer_pb.DeleteEntryResponse, error) {
	var resp *filer_pb.DeleteEntryResponse
	err := wfs.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		var err error
		resp, err = client.DeleteEntry(context.Background(), req)
		if err != nil {
			return err
		}
		// A refused delete arrives in the response, as the stream branch above.
		if resp.Error != "" {
			return &streamMutateError{msg: resp.Error, errno: syscall.EIO}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}
