package mount

import (
	"os"
	"testing"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/seaweedfs/go-fuse/v2/fuse"
)

func TestToGoOpenFlagsAccessModes(t *testing.T) {
	cases := []struct {
		in   int
		want uint32
	}{
		{cgofuse.O_RDONLY, uint32(os.O_RDONLY)},
		{cgofuse.O_WRONLY, uint32(os.O_WRONLY)},
		{cgofuse.O_RDWR, uint32(os.O_RDWR)},
		{cgofuse.O_WRONLY | cgofuse.O_CREAT, uint32(os.O_WRONLY | os.O_CREATE)},
		{cgofuse.O_RDWR | cgofuse.O_CREAT | cgofuse.O_EXCL, uint32(os.O_RDWR | os.O_CREATE | os.O_EXCL)},
		{cgofuse.O_WRONLY | cgofuse.O_TRUNC, uint32(os.O_WRONLY | os.O_TRUNC)},
		{cgofuse.O_WRONLY | cgofuse.O_APPEND, uint32(os.O_WRONLY | os.O_APPEND)},
	}
	for _, c := range cases {
		if got := toGoOpenFlags(c.in); got != c.want {
			t.Errorf("toGoOpenFlags(%#x) = %#x, want %#x", c.in, got, c.want)
		}
	}
}

func TestToGoOpenFlagsAnyWriteMask(t *testing.T) {
	// go-fuse's O_ANYWRITE gate must see write intent for every
	// write-capable cgofuse flag combination.
	writey := []int{
		cgofuse.O_WRONLY,
		cgofuse.O_RDWR,
		cgofuse.O_WRONLY | cgofuse.O_CREAT,
		cgofuse.O_RDWR | cgofuse.O_TRUNC,
		cgofuse.O_WRONLY | cgofuse.O_APPEND,
	}
	for _, f := range writey {
		if toGoOpenFlags(f)&fuse.O_ANYWRITE == 0 {
			t.Errorf("flags %#x lost write intent through translation", f)
		}
	}
	if toGoOpenFlags(cgofuse.O_RDONLY)&fuse.O_ANYWRITE != 0 {
		t.Errorf("O_RDONLY translated with write intent")
	}
}
