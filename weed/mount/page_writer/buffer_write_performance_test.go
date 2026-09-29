package page_writer

import "testing"

// Keep this workload unchanged when comparing pre/post error-path fixes.
// Repeated partial overwrites avoid upload/network latency and exercise the
// same interface dispatch and interval bookkeeping as ordinary buffered I/O.
func BenchmarkBufferedWrite(b *testing.B) {
	for _, memory := range []bool{true, false} {
		name := "swap"
		if memory {
			name = "memory"
		}
		b.Run(name, func(b *testing.B) {
			up := NewUploadPipeline(nil, 1<<20, nil, 2, b.TempDir(), nil)
			defer up.Shutdown()
			data := make([]byte, 4096)
			if _, err := up.SaveDataAt(data, 64, memory, 1); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if n, err := up.SaveDataAt(data, 64, memory, int64(i+2)); n != len(data) || err != nil {
					b.Fatalf("write: %d, %v", n, err)
				}
			}
			b.StopTimer()
		})
	}
}
