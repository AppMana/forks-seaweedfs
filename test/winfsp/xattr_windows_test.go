package winfsp

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ntdll         = windows.NewLazySystemDLL("ntdll.dll")
	ntSetEaFile   = ntdll.NewProc("NtSetEaFile")
	ntQueryEaFile = ntdll.NewProc("NtQueryEaFile")
)

const (
	fileReadEa  = 0x0008
	fileWriteEa = 0x0010

	statusNoEasOnFile = 0xC0000052
	statusNoMoreEas   = 0x80000012
)

// fullEaInformation encodes one FILE_FULL_EA_INFORMATION record. A value of
// zero length removes the attribute.
func fullEaInformation(name string, value []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(0))
	b.WriteByte(0)
	b.WriteByte(byte(len(name)))
	binary.Write(&b, binary.LittleEndian, uint16(len(value)))
	b.WriteString(name)
	b.WriteByte(0)
	b.Write(value)
	return b.Bytes()
}

func openForEa(t *testing.T, path string) windows.Handle {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, fileReadEa|fileWriteEa|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("open %s for extended attributes: %v", path, err)
	}
	return h
}

func setEa(h windows.Handle, name string, value []byte) error {
	buf := fullEaInformation(name, value)
	var iosb windows.IO_STATUS_BLOCK
	status, _, _ := ntSetEaFile.Call(uintptr(h), uintptr(unsafe.Pointer(&iosb)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if status != 0 {
		return windows.NTStatus(status)
	}
	return nil
}

// queryEas returns every extended attribute on the file, by name.
func queryEas(h windows.Handle) (map[string][]byte, error) {
	buf := make([]byte, 64<<10)
	var iosb windows.IO_STATUS_BLOCK
	status, _, _ := ntQueryEaFile.Call(uintptr(h), uintptr(unsafe.Pointer(&iosb)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0, 0, 0, 1)
	eas := map[string][]byte{}
	if status == statusNoEasOnFile || status == statusNoMoreEas {
		return eas, nil
	}
	if status != 0 {
		return nil, windows.NTStatus(status)
	}
	for off := 0; off < len(buf); {
		next := binary.LittleEndian.Uint32(buf[off:])
		nameLen := int(buf[off+5])
		valueLen := int(binary.LittleEndian.Uint16(buf[off+6:]))
		name := string(buf[off+8 : off+8+nameLen])
		value := buf[off+8+nameLen+1 : off+8+nameLen+1+valueLen]
		eas[name] = append([]byte(nil), value...)
		if next == 0 {
			break
		}
		off += int(next)
	}
	return eas, nil
}

// filerExtended reads the entry's Extended map through the filer, which is
// where Linux mounts read extended attributes from.
func filerExtended(addr, relative string) (map[string][]byte, error) {
	fullPath, err := scopedFilerPath(*filerRoot, relative)
	if err != nil {
		return nil, err
	}
	endpoint := &url.URL{Scheme: "http", Host: addr, Path: fullPath, RawQuery: "metadata=true"}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(endpoint.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", endpoint, resp.Status)
	}
	var entry struct {
		Extended map[string]string `json:"Extended"`
	}
	if err := json.Unmarshal(body, &entry); err != nil {
		return nil, fmt.Errorf("decode %s: %w", endpoint, err)
	}
	extended := map[string][]byte{}
	for k, v := range entry.Extended {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("extended %s: %w", k, err)
		}
		extended[k] = decoded
	}
	return extended, nil
}

// Windows extended attributes are the mount's xattrs: a value set through
// NtSetEaFile is read back after reopening, is stored on the filer entry
// where Linux mounts read it, and is removed by a zero-length set. Windows
// upper-cases EA names, so the test uses an upper-case name.
func TestWindowsExtendedAttributesRoundTrip(t *testing.T) {
	root := testRoot(t)
	path := filepath.Join(root, "ea-file")
	if err := writeAndSync(path, []byte("payload beside an extended attribute")); err != nil {
		t.Fatal(err)
	}
	const name = "SEAWEEDFS.TEST"
	value := []byte("value\x00with binary \xff bytes")

	h := openForEa(t, path)
	if err := setEa(h, name, value); err != nil {
		windows.CloseHandle(h)
		t.Fatalf("NtSetEaFile: %v", err)
	}
	windows.CloseHandle(h)

	h = openForEa(t, path)
	eas, err := queryEas(h)
	windows.CloseHandle(h)
	if err != nil {
		t.Fatalf("NtQueryEaFile: %v", err)
	}
	if got, ok := eas[name]; !ok || !bytes.Equal(got, value) {
		t.Fatalf("after reopen: %s = %q (present %v), want %q; all: %q", name, got, ok, value, eas)
	}

	if *filerAddr != "" {
		extended, err := filerExtended(*filerAddr, filepath.ToSlash(filepath.Join(filepath.Base(root), "ea-file")))
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := extended["xattr-"+name]; !ok || !bytes.Equal(got, value) {
			t.Fatalf("filer entry xattr-%s = %q (present %v), want %q", name, got, ok, value)
		}
	}

	h = openForEa(t, path)
	err = setEa(h, name, nil)
	if err == nil {
		eas, err = queryEas(h)
	}
	windows.CloseHandle(h)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := eas[name]; ok {
		t.Fatalf("%s still present after a zero-length set: %q", name, eas)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "payload beside an extended attribute" {
		t.Fatalf("extended attributes damaged data: %q %v", data, err)
	}
}
