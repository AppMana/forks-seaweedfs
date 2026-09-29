package needle

import (
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util/buffer_pool"
)

// memBackend is an in-memory BackendStorageFile that records every WriteAt.
// It can fail the write that would take the file past failAfter bytes.
type memBackend struct {
	data      []byte
	writes    int
	failAfter int64 // <=0: never fail
	truncated []int64
	keep      bool // retain written bytes (false: only track the size)
	size      int64
}

func (m *memBackend) ReadAt(p []byte, off int64) (int, error) {
	return copy(p, m.data[off:]), nil
}

func (m *memBackend) WriteAt(p []byte, off int64) (int, error) {
	m.writes++
	if m.failAfter > 0 && off+int64(len(p)) > m.failAfter {
		n := int(m.failAfter - off)
		if n < 0 {
			n = 0
		}
		m.put(p[:n], off)
		return n, errors.New("injected write failure")
	}
	m.put(p, off)
	return len(p), nil
}

func (m *memBackend) put(p []byte, off int64) {
	if end := off + int64(len(p)); end > m.size {
		m.size = end
	}
	if !m.keep {
		return
	}
	if end := off + int64(len(p)); end > int64(len(m.data)) {
		m.data = append(m.data, make([]byte, end-int64(len(m.data)))...)
	}
	copy(m.data[off:], p)
}

func (m *memBackend) Truncate(off int64) error {
	m.truncated = append(m.truncated, off)
	m.size = off
	if m.keep && off < int64(len(m.data)) {
		m.data = m.data[:off]
	}
	return nil
}
func (m *memBackend) Close() error                       { return nil }
func (m *memBackend) GetStat() (int64, time.Time, error) { return m.size, time.Time{}, nil }
func (m *memBackend) Name() string                       { return "mem" }
func (m *memBackend) Sync() error                        { return nil }

func appendTestNeedles() map[string]func() *Needle {
	payload := make([]byte, 100_003)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	base := func() *Needle {
		return &Needle{Cookie: types.Cookie(0x1234abcd), Id: types.NeedleId(0x77), Data: append([]byte(nil), payload...), Checksum: 0xdeadbeef, AppendAtNs: 1790667986000000000}
	}
	return map[string]func() *Needle{
		"plain": base,
		"all fields": func() *Needle {
			n := base()
			n.SetHasName()
			n.Name = []byte("Newtonsoft.Json.pdb")
			n.SetHasMime()
			n.Mime = []byte("application/octet-stream")
			n.SetHasLastModifiedDate()
			n.LastModified = 1790667986
			n.SetHasTtl()
			n.Ttl, _ = ReadTTL("3d")
			n.SetHasPairs()
			n.Pairs = []byte(`{"owner":"harbor"}`)
			n.PairsSize = uint16(len(n.Pairs))
			return n
		},
		"tombstone": func() *Needle {
			n := base()
			n.Data = nil
			return n
		},
		"one byte": func() *Needle {
			n := base()
			n.Data = []byte{9}
			return n
		},
	}
}

// TestAppendWritesLegacyRecord pins the on-disk record, and Append's returns,
// to the reference buffered encoder for every version.
func TestAppendWritesLegacyRecord(t *testing.T) {
	for _, version := range []Version{Version1, Version2, Version3} {
		for name, mk := range appendTestNeedles() {
			t.Run(fmt.Sprintf("%s/%s", versionString(version), name), func(t *testing.T) {
				const start = 8 * 1000
				ref := mk()
				want := new(bytes.Buffer)
				wantSize, wantActual, err := ref.LegacyPrepareWriteBuffer(version, want)
				if err != nil {
					t.Fatal(err)
				}

				backend := &memBackend{keep: true, data: make([]byte, start), size: start}
				n := mk()
				offset, size, actual, err := n.Append(backend, version)
				if err != nil {
					t.Fatalf("append: %v", err)
				}
				if offset != start || size != wantSize || actual != wantActual {
					t.Fatalf("returns offset=%d size=%d actual=%d, want %d/%d/%d", offset, size, actual, start, wantSize, wantActual)
				}
				if got := backend.data[start:]; !bytes.Equal(got, want.Bytes()) {
					t.Fatalf("record differs from the buffered encoding: got %d bytes, want %d", len(got), want.Len())
				}
				if backend.size != start+int64(want.Len()) {
					t.Fatalf("file size %d, want %d", backend.size, start+int64(want.Len()))
				}
			})
		}
	}
}

// TestAppendTruncatesOnFailedWrite covers every part of the record: a write
// failing anywhere must leave the file at its original length.
func TestAppendTruncatesOnFailedWrite(t *testing.T) {
	const start = 4096
	for _, version := range []Version{Version1, Version2, Version3} {
		record := new(bytes.Buffer)
		if _, _, err := appendTestNeedles()["all fields"]().LegacyPrepareWriteBuffer(version, record); err != nil {
			t.Fatal(err)
		}
		// in the header, in the payload, and in the last bytes of the footer
		for _, cut := range []int64{1, types.NeedleHeaderSize + 2, types.NeedleHeaderSize + 50_000, int64(record.Len()) - 1} {
			t.Run(fmt.Sprintf("%s/cut%d", versionString(version), cut), func(t *testing.T) {
				backend := &memBackend{keep: true, data: make([]byte, start), size: start, failAfter: start + cut}
				if _, _, _, err := appendTestNeedles()["all fields"]().Append(backend, version); err == nil {
					t.Fatal("want an error")
				}
				if len(backend.truncated) != 1 || backend.truncated[0] != start || backend.size != start {
					t.Fatalf("truncated to %v, size %d; want one truncation to %d", backend.truncated, backend.size, start)
				}
			})
		}
	}
}

// TestAppendDoesNotCopyPayload: Append copied the whole needle body into a
// pooled buffer before writing it, on the primary and on every replica. The
// pool keeps that capacity until two collections pass, and after a collection
// the next append allocates it again.
func TestAppendDoesNotCopyPayload(t *testing.T) {
	const size = 8 << 20
	const appends = 4
	payload := make([]byte, size)
	backend := &memBackend{}

	var total uint64
	for i := 0; i < appends; i++ {
		n := &Needle{Cookie: 1, Id: types.NeedleId(i + 1), Data: payload, Checksum: 1, AppendAtNs: 1}
		// two collections empty sync.Pool, as between writes on a busy server
		runtime.GC()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if _, _, _, err := n.Append(backend, GetCurrentVersion()); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		total += after.TotalAlloc - before.TotalAlloc
	}
	perAppend := total / appends
	if perAppend >= size/8 {
		t.Fatalf("allocated %d bytes per %d-byte append; the payload must be written in place", perAppend, size)
	}
	t.Logf("allocated %d bytes per %d-byte append", perAppend, size)

	n := &Needle{Cookie: 1, Id: 99, Data: payload, Checksum: 1, AppendAtNs: 1}
	if _, _, _, err := n.Append(backend, GetCurrentVersion()); err != nil {
		t.Fatal(err)
	}
	pooled := buffer_pool.SyncPoolGetBuffer()
	defer buffer_pool.SyncPoolPutBuffer(pooled)
	if pooled.Cap() >= size {
		t.Fatalf("pooled write buffer retained %d bytes of capacity after an append", pooled.Cap())
	}
}
