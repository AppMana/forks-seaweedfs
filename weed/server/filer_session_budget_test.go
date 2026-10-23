package weed_server

import "testing"

func TestFilerSessionReaderBudget(t *testing.T) {
	for _, tc := range []struct{ whole, session, want int64 }{{512, 64, 64}, {512, 0, 128}, {512, 1024, 512}, {0, 0, 64}} {
		if got := filerSessionReaderBudget(tc.whole, tc.session); got != tc.want<<20 {
			t.Fatalf("%+v: got %d", tc, got)
		}
	}
}
