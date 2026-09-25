package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

// Portable instrumentation: copy this file, not application changes, into
// vanilla upstream when comparing startup costs. Include a full checkpoint
// and nonempty tails on both sides of it.
func BenchmarkLevelDBRecoveryOpen(b *testing.B) {
	for _, count := range []int{10000, 16384, 65536} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			dir := b.TempDir()
			indexPath, dbPath := filepath.Join(dir, "1.idx"), filepath.Join(dir, "1.ldb")
			index, err := os.Create(indexPath)
			if err != nil {
				b.Fatal(err)
			}
			m, err := NewLevelDbNeedleMap(dbPath, index, nil, 0, needle.GetCurrentVersion())
			if err != nil {
				b.Fatal(err)
			}
			for i := 1; i <= count; i++ {
				if err := m.Put(types.NeedleId(i), types.ToOffset(int64(i)*1024), types.Size(512)); err != nil {
					b.Fatal(err)
				}
			}
			m.Close()
			// Exercise upstream's fastest legal startup case, not just the old
			// already-slow replay path. LOG timestamp has no effect on new code.
			future := time.Now().Add(time.Hour)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := os.Chtimes(filepath.Join(dbPath, "LOG"), future, future); err != nil {
					b.Fatal(err)
				}
				index, err = os.OpenFile(indexPath, os.O_RDWR, 0600)
				if err != nil {
					b.Fatal(err)
				}
				m, err = NewLevelDbNeedleMap(dbPath, index, nil, 0, needle.GetCurrentVersion())
				if err != nil {
					b.Fatal(err)
				}
				m.Close()
			}
		})
	}
}
