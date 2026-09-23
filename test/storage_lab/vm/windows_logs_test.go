package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func collectWindowsGuestLogs(exec func(string) ([]byte, error)) ([]byte, error) {
	// Keep the existing per-file tail policy, but materialize it in the guest.
	// Sending every file tail as one exec response exceeds QGA's output limit.
	_, err := exec(`$ErrorActionPreference='Stop'; $files=@(Get-ChildItem C:\lab\smoke-*\logs\*,C:\lab\dependency-install.log -File -ErrorAction SilentlyContinue | Where-Object { $_.Extension -in '.log','.txt' }); if($files.Count -eq 0){throw 'no guest diagnostic logs found'}; $files | ForEach-Object { Write-Output ("FILE: " + $_.FullName); Get-Content -LiteralPath $_.FullName -Tail 2000 -ErrorAction Stop } | Out-File -LiteralPath 'C:\lab\collected-guest-logs.txt' -Encoding UTF8`)
	if err != nil {
		return nil, err
	}
	return readWindowsArtifact(`C:\lab\collected-guest-logs.txt`, exec)
}

func TestWindowsGuestLogsRejectCollectionFailure(t *testing.T) {
	calls := 0
	_, err := collectWindowsGuestLogs(func(string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("guest collection failed")
	})
	if err == nil || calls != 1 {
		t.Fatalf("ignored collection failure: calls=%d err=%v", calls, err)
	}
}

func TestWindowsGuestLogsExceedExecOutputLimit(t *testing.T) {
	want := bytes.Repeat([]byte("diagnostic line\n"), 220000)
	calls, offset := 0, 0
	got, err := collectWindowsGuestLogs(func(command string) ([]byte, error) {
		calls++
		if calls == 1 {
			if !strings.Contains(command, "Out-File -LiteralPath 'C:\\lab\\collected-guest-logs.txt'") {
				return nil, fmt.Errorf("guest output was truncated")
			}
			return nil, nil
		}
		if calls == 2 {
			return []byte(fmt.Sprintf("%d %s", len(want), sha(want))), nil
		}
		count := min(1<<20, len(want)-offset)
		if !strings.Contains(command, fmt.Sprintf("Seek(%d,", offset)) {
			t.Fatal("incorrect chunk offset")
		}
		chunk := []byte(base64.StdEncoding.EncodeToString(want[offset : offset+count]))
		offset += count
		return chunk, nil
	})
	if err != nil || !bytes.Equal(got, want) || offset != len(want) {
		t.Fatalf("collection lost large diagnostic output: calls=%d offset=%d err=%v", calls, offset, err)
	}
}
