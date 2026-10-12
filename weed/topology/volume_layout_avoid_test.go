package topology

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

// Server 10.0.0.1 holds four empty volumes and 10.0.0.2 four nearly full
// ones, so a pick weighted by remaining space lands on 10.0.0.1 nine times in
// ten: the shape of a stalled volume server, whose volumes stop filling while
// the healthy ones take every write.
const avoidLayout = `
{
  "dc1":{
    "rack1":{
      "stalled":{
        "ip":"10.0.0.1",
        "volumes":[
          {"id":1, "size":0, "replication":"000"},
          {"id":2, "size":0, "replication":"000"},
          {"id":3, "size":0, "replication":"000"},
          {"id":4, "size":0, "replication":"000"}
        ],
        "limit":10
      },
      "healthy":{
        "ip":"10.0.0.2",
        "volumes":[
          {"id":5, "size":9000, "replication":"000"},
          {"id":6, "size":9000, "replication":"000"},
          {"id":7, "size":9000, "replication":"000"},
          {"id":8, "size":9000, "replication":"000"}
        ],
        "limit":10
      }
    }
  }
}
`

func serverUrl(t *testing.T, topo *Topology, id string) string {
	t.Helper()
	for _, dc := range topo.Children() {
		for _, rack := range dc.Children() {
			for _, node := range rack.Children() {
				if string(node.Id()) == id {
					return node.(*DataNode).Url()
				}
			}
		}
	}
	t.Fatalf("no data node %s", id)
	return ""
}

func setupPickTestWithReplication(t *testing.T, layout, replication string) (*Topology, *VolumeLayout) {
	t.Helper()
	topo := setupWithLimit(t, layout, 10000)
	rp, _ := super_block.NewReplicaPlacementFromString(replication)
	return topo, topo.GetVolumeLayout("", rp, needle.EMPTY_TTL, types.HardDriveType)
}

// A writer that saw a volume server stall asks the master to avoid it. While
// any writable volume avoids it, no pick may land on it: a writer that has to
// find such a volume by drawing assignments at random went back to the
// stalled server once its eight draws all landed there, and a mount close()
// that met that twice ran past the 60 s FUSE request deadline.
func TestPickForWriteAvoidsTheServersAWriterNames(t *testing.T) {
	for _, option := range []*VolumeGrowOption{{}, {DataCenter: "dc1"}} {
		topo, vl := setupPickTest(t, avoidLayout, 10000)
		option.AvoidServers = []string{serverUrl(t, topo, "stalled")}
		for i := 0; i < 2000; i++ {
			vid, _, locations, shouldGrow, err := vl.PickForWrite(1, option)
			if err != nil || shouldGrow {
				t.Fatalf("pick %d with %+v: %v, shouldGrow=%v", i, option, err, shouldGrow)
			}
			if vid < 5 {
				t.Fatalf("pick %d with dataCenter %q landed on volume %d of the avoided server", i, option.DataCenter, vid)
			}
			if locations.Length() != 1 || locations.Head().Url() != serverUrl(t, topo, "healthy") {
				t.Fatalf("pick %d: locations %v", i, locations.list)
			}
		}
	}
}

// A replicated volume is avoided when any of its holders is.
func TestPickForWriteAvoidsAVolumeAnyOfWhoseHoldersIsNamed(t *testing.T) {
	topo, vl := setupPickTestWithReplication(t, `
{
  "dc1":{
    "rack1":{
      "a":{"ip":"10.0.0.1", "volumes":[{"id":1, "size":0, "replication":"001"}, {"id":2, "size":9000, "replication":"001"}], "limit":10},
      "b":{"ip":"10.0.0.2", "volumes":[{"id":1, "size":0, "replication":"001"}], "limit":10},
      "c":{"ip":"10.0.0.3", "volumes":[{"id":2, "size":9000, "replication":"001"}], "limit":10}
    }
  }
}
`, "001")
	option := &VolumeGrowOption{AvoidServers: []string{serverUrl(t, topo, "b")}}
	for i := 0; i < 500; i++ {
		vid, _, _, _, err := vl.PickForWrite(1, option)
		if err != nil {
			t.Fatal(err)
		}
		if vid != needle.VolumeId(2) {
			t.Fatalf("pick %d landed on volume %d, whose replica is on the avoided server", i, vid)
		}
	}
}

// When every writable volume involves an avoided server the pick still
// answers, from all of them: avoiding is a preference, the writer decides
// whether to go ahead, and the master must neither fail the assignment nor
// grow volumes over it.
func TestPickForWriteFallsBackWhenEveryVolumeIsAvoided(t *testing.T) {
	topo, vl := setupPickTest(t, avoidLayout, 10000)
	option := &VolumeGrowOption{AvoidServers: []string{serverUrl(t, topo, "stalled"), serverUrl(t, topo, "healthy")}}
	seen := map[needle.VolumeId]bool{}
	for i := 0; i < 2000; i++ {
		vid, _, _, shouldGrow, err := vl.PickForWrite(1, option)
		if err != nil || shouldGrow {
			t.Fatalf("pick %d: %v, shouldGrow=%v", i, err, shouldGrow)
		}
		seen[vid] = true
	}
	if !seen[1] || !seen[5] {
		t.Fatalf("picks %v did not range over both servers' volumes", seen)
	}
}

func TestVolumeGrowOptionStringOmitsAvoidServers(t *testing.T) {
	option := &VolumeGrowOption{Collection: "c"}
	before := option.String()
	option.AvoidServers = []string{"10.0.0.1:8080"}
	if after := option.String(); after != before {
		t.Fatalf("String() = %s with avoided servers, %s without: it keys growth logs and warnings", after, before)
	}
}

// Avoiding narrows the volumes a constrained pick chooses from, never to
// none: when only an avoided server serves the requested data center, the
// pick falls back to it rather than reporting no writable volume, which
// would grow volumes and hold the assignment for growth.
func TestConstrainedPickFallsBackWhenOnlyAvoidedServersMatch(t *testing.T) {
	topo, vl := setupPickTest(t, `
{
  "dc1":{"rack1":{"stalled":{"ip":"10.0.0.1", "volumes":[{"id":1, "size":0, "replication":"000"}], "limit":10}}},
  "dc2":{"rack1":{"healthy":{"ip":"10.0.0.2", "volumes":[{"id":2, "size":0, "replication":"000"}], "limit":10}}}
}
`, 10000)
	option := &VolumeGrowOption{DataCenter: "dc1", AvoidServers: []string{serverUrl(t, topo, "stalled")}}
	vid, _, _, shouldGrow, err := vl.PickForWrite(1, option)
	if err != nil || shouldGrow || vid != 1 {
		t.Fatalf("pick in dc1 = volume %d, %v, shouldGrow=%v; want volume 1", vid, err, shouldGrow)
	}
}
