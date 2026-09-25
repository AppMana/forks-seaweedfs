package main

import (
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

func mapReadOnly(f *os.File, size int) ([]byte, func(), error) {
	h, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READONLY, 0, uint32(size), nil)
	if err != nil {
		return nil, func() {}, err
	}
	p, err := windows.MapViewOfFile(h, windows.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		windows.CloseHandle(h)
		return nil, func() {}, err
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(p)), size), func() { windows.UnmapViewOfFile(p); windows.CloseHandle(h) }, nil
}
