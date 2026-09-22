package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// QGA exec output has a smaller ceiling than the lab RPC. Transfer bounded
// chunks and verify the complete artifact against the guest's SHA-256.
func readWindowsArtifact(path string, exec func(string) ([]byte, error)) ([]byte, error) {
	quoted := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	metadata, err := exec("$p=" + quoted + "; $f=Get-Item -LiteralPath $p -ErrorAction Stop; Write-Output $f.Length; Write-Output (Get-FileHash -LiteralPath $p -Algorithm SHA256 -ErrorAction Stop).Hash")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(metadata))
	if len(fields) != 2 {
		return nil, fmt.Errorf("invalid artifact metadata: %q", metadata)
	}
	size, err := strconv.Atoi(fields[0])
	if err != nil || size <= 0 || size > 128<<20 {
		return nil, fmt.Errorf("artifact size outside 1..128MiB: %q", fields[0])
	}
	data := make([]byte, 0, size)
	for len(data) < size {
		count := min(1<<20, size-len(data))
		command := fmt.Sprintf("$f=[IO.File]::OpenRead(%s); try { $null=$f.Seek(%d,[IO.SeekOrigin]::Begin); $b=New-Object byte[] %d; $n=0; while($n -lt $b.Length){ $r=$f.Read($b,$n,$b.Length-$n); if($r -eq 0){throw 'unexpected EOF'}; $n+=$r }; [Convert]::ToBase64String($b) } finally { $f.Dispose() }", quoted, len(data), count)
		encoded, err := exec(command)
		if err != nil {
			return nil, err
		}
		chunk, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
		if err != nil || len(chunk) != count {
			return nil, fmt.Errorf("artifact chunk at %d: length %d want %d: %v", len(data), len(chunk), count, err)
		}
		data = append(data, chunk...)
	}
	if !strings.EqualFold(sha(data), fields[1]) {
		return nil, fmt.Errorf("artifact SHA-256 mismatch")
	}
	return data, nil
}

func TestWindowsArtifactMultiChunk(t *testing.T) {
	want := bytes.Repeat([]byte("x"), (2<<20)+17)
	calls, offset := 0, 0
	got, err := readWindowsArtifact(`C:\lab\it's a trace.etl`, func(command string) ([]byte, error) {
		calls++
		if !strings.Contains(command, "it''s a trace.etl") {
			t.Fatal("path not PowerShell-escaped")
		}
		if calls == 1 {
			return []byte(fmt.Sprintf("%d %s", len(want), sha(want))), nil
		}
		count := min(1<<20, len(want)-offset)
		if !strings.Contains(command, fmt.Sprintf("Seek(%d,", offset)) || !strings.Contains(command, fmt.Sprintf("byte[] %d;", count)) {
			t.Fatalf("incorrect chunk command: %s", command)
		}
		encoded := base64.StdEncoding.EncodeToString(want[offset : offset+count])
		offset += count
		return []byte(encoded), nil
	})
	if err != nil || !bytes.Equal(got, want) || calls != 4 {
		t.Fatalf("multi-chunk calls=%d: %v", calls, err)
	}
}

func TestWindowsArtifactRejectsIncompleteEvidence(t *testing.T) {
	for _, mode := range []string{"ok", "short", "corrupt", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			want := []byte("retained ETL bytes")
			calls := 0
			got, err := readWindowsArtifact(`C:\lab\trace.etl`, func(command string) ([]byte, error) {
				calls++
				if calls == 1 {
					if mode == "oversized" {
						return []byte("999999999 abc"), nil
					}
					return []byte(fmt.Sprintf("%d\r\n%s\r\n", len(want), sha(want))), nil
				}
				chunk := append([]byte(nil), want...)
				if mode == "short" {
					chunk = chunk[:len(chunk)-1]
				}
				if mode == "corrupt" {
					chunk[0] ^= 1
				}
				return []byte(base64.StdEncoding.EncodeToString(chunk)), nil
			})
			if mode == "ok" {
				if err != nil || string(got) != string(want) {
					t.Fatalf("got %q: %v", got, err)
				}
			} else if err == nil {
				t.Fatal("accepted incomplete/corrupt evidence")
			}
		})
	}
}
