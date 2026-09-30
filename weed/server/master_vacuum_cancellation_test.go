package weed_server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/topology"
)

func TestCanceledVolumeVacuumDoesNotReturnSuccess(t *testing.T) {
	ms := &MasterServer{
		Topo:   topology.NewTopology("vacuum", nil, 32<<20, 5, false),
		option: &MasterOption{MaxParallelVacuumPerServer: 1},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/vol/vacuum", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	ms.volumeVacuumHandler(w, r)
	if w.Code != http.StatusRequestTimeout {
		t.Fatalf("canceled vacuum returned HTTP %d: %s", w.Code, w.Body.String())
	}
}
