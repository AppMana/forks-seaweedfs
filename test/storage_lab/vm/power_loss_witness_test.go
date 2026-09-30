package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestVolatileWitnessRejectsNoLossAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "witness")
	run := func(script string) error {
		// Never run the guest sysctl preparation on the test host.
		script = strings.ReplaceAll(script, "'/mnt/volume/.power-loss-witness'", strconv.Quote(path))
		return exec.Command("python3", "-c", script).Run()
	}
	write := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(bytes.Repeat([]byte("D"), 4096))
	if err := run(dirtyVolatileWitness); err != nil {
		t.Fatal(err)
	}
	if err := run(checkVolatileWitness); err == nil {
		t.Fatal("no-loss restart accepted as power loss")
	}
	for _, data := range [][]byte{nil, []byte("D"), bytes.Repeat([]byte("X"), 4096)} {
		write(data)
		if err := run(checkVolatileWitness); err == nil {
			t.Fatal("missing/corrupt durable control accepted")
		}
	}
	write(bytes.Repeat([]byte("D"), 4096))
	if err := run(checkVolatileWitness); err != nil {
		t.Fatal(err)
	}
}
