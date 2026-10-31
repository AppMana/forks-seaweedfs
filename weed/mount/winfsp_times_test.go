package mount

import "testing"

func TestWinfspTimeApplies(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sec, nsec int64
		want      bool
	}{
		{"UTIME_OMIT", 0, (1 << 30) - 2, false},
		{"UTIME_OMIT with seconds", 1700000000, (1 << 30) - 2, false},
		{"zero", 0, 0, false},
		{"Windows epoch 1601-01-01", -11644473600, 0, false},
		{"Windows epoch plus a day", -11644387200, 0, false},
		{"a real time", 1700000000, 5, true},
		{"one nanosecond after the Unix epoch", 0, 1, true},
		{"a date before 1970", -86400, 0, true},
		{"just past the Windows epoch cutoff", -11644387199, 0, true},
	} {
		if got := winfspTimeApplies(tc.sec, tc.nsec); got != tc.want {
			t.Errorf("%s: winfspTimeApplies(%d, %d) = %v, want %v", tc.name, tc.sec, tc.nsec, got, tc.want)
		}
	}
}
