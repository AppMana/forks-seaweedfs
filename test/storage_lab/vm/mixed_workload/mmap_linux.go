package main

import (
	"os"
	"syscall"
)

func mapReadOnly(f *os.File, size int) ([]byte, func(), error) {
	b, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	return b, func() { _ = syscall.Munmap(b) }, err
}
