package main

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// Select the already-verified package DLL without distributing the private
// driver, installer, or certificate to the stock-driver qualification guest.
func stockWinFspArtifacts(files map[string][]byte, driverSHA string) (map[string][]byte, error) {
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(driverSHA) {
		return nil, fmt.Errorf("stock driver requires exact SHA256")
	}
	selected := map[string][]byte{}
	for _, name := range []string{`C:\lab\winfsp-x64.dll`, `C:\lab\output\manifest.json`} {
		data := files[name]
		if len(data) == 0 {
			return nil, fmt.Errorf("missing verified DLL package evidence %s", name)
		}
		selected[name] = data
	}
	return selected, nil
}

func stockWinFspVerification(driverSHA string) string {
	return `$ErrorActionPreference='Stop'; $drivers=@(Get-CimInstance Win32_SystemDriver | Where-Object {$_.Name -like 'WinFsp*' -and $_.State -eq 'Running'}); if($drivers.Count -ne 1){throw 'expected exactly one running WinFsp driver'}; $path=$drivers[0].PathName.Trim('"'); if($path.StartsWith('\??\')){$path=$path.Substring(4)}; if($path.StartsWith('\SystemRoot\')){$path=Join-Path $env:SystemRoot $path.Substring(12)}; $path=[Environment]::ExpandEnvironmentVariables($path); $hash=(Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant(); if($hash -ne '` + driverSHA + `'){throw "wrong loaded driver: $path $hash"}; $signature=Get-AuthenticodeSignature -LiteralPath $path; if($signature.Status -ne 'Valid'){throw "stock driver signature invalid: $($signature.Status)"}; Write-Output "STOCK_DRIVER_PATH: $path"; Write-Output "STOCK_DRIVER_SIGNER: $($signature.SignerCertificate.Subject)"; Write-Output 'STOCK_DRIVER_READY:` + driverSHA + `'`
}

func TestStockWinFspSelectionNeverStagesPrivateDriver(t *testing.T) {
	files := map[string][]byte{`C:\lab\winfsp-x64.dll`: []byte("dll"), `C:\lab\output\manifest.json`: []byte("manifest"), `C:\lab\output\winfsp-x64.sys`: []byte("private driver"), `C:\lab\output\lab.cer`: []byte("test certificate"), `C:\lab\install-native-winfsp.ps1`: []byte("installer")}
	selected, err := stockWinFspArtifacts(files, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 {
		t.Fatal("unexpected stock inputs", selected)
	}
	for name := range selected {
		if strings.HasSuffix(name, ".sys") || strings.HasSuffix(name, ".cer") || strings.HasSuffix(name, ".ps1") {
			t.Fatal("staged private driver material")
		}
	}
	if _, err := stockWinFspArtifacts(files, "wrong"); err == nil {
		t.Fatal("accepted invalid driver pin")
	}
	delete(files, `C:\lab\winfsp-x64.dll`)
	if _, err := stockWinFspArtifacts(files, strings.Repeat("a", 64)); err == nil {
		t.Fatal("accepted missing DLL")
	}
}
