package filer

import (
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb"
)

// A peer filer's change reaches this filer's store only when the aggregator
// replays it, and its event carries the peer's stamp, which can be well below
// the time of a listing read before the replay. The listing's snapshot must
// stay at or below what every peer has been replayed through, or a mount
// fences that event out of the listing it built.
func TestListingSnapshotStaysAtOrBelowWhatPeersAreReplayedThrough(t *testing.T) {
	self, peer := pb.ServerAddress("filer-a:8888"), pb.ServerAddress("filer-b:8888")
	f := &Filer{}
	ma := newTestAggregator()
	ma.self = self
	ma.peerChans = map[pb.ServerAddress]chan struct{}{self: make(chan struct{}), peer: make(chan struct{})}
	f.MetaAggregator = ma

	now := time.Now().UnixNano()
	replayedThrough := now - int64(300*time.Millisecond)
	ma.initPeerWatermark(self)
	ma.initPeerWatermark(peer)
	ma.advancePeerWatermark(self, now)
	ma.advancePeerWatermark(peer, replayedThrough)

	if snapshot := f.ListingSnapshotTsNs(now); snapshot > replayedThrough {
		t.Fatalf("snapshot %d is past %d, what the peer's changes are replayed through", snapshot, replayedThrough)
	}
}

// Without peers a listing is complete through every change this filer
// stamped and finished, and nothing later can be stamped at or below it.
func TestListingSnapshotFencesLaterStamps(t *testing.T) {
	f := &Filer{}
	now := time.Now().UnixNano() + int64(time.Hour)
	snapshot := f.ListingSnapshotTsNs(now)
	if stamp := f.metaLogInflight.stamp(); stamp <= snapshot {
		t.Fatalf("a change stamped after the listing at %d lands at or below its snapshot %d", stamp, snapshot)
	}
}
