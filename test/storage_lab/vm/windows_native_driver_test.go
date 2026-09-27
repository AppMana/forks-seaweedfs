package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type nativeWinFspManifest struct {
	Revision       string `json:"source_revision"`
	DriverRevision string `json:"driver_source_revision"`
	DriverSHA      string `json:"driver_sha256"`
	DLLSHA         string `json:"dll_sha256"`
	Thumbprint     string `json:"certificate_thumbprint"`
	LabOnly        bool   `json:"lab_only"`
}

func validateNativeWinFsp(manifest, driver, dll, certificate []byte) (nativeWinFspManifest, error) {
	var m nativeWinFspManifest
	// Windows PowerShell writes a UTF-8 BOM on JSON files.
	err := json.Unmarshal(bytes.TrimPrefix(manifest, []byte{0xef, 0xbb, 0xbf}), &m)
	if err != nil {
		return m, err
	}
	revision := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if !m.LabOnly || !revision.MatchString(m.Revision) || !revision.MatchString(m.DriverRevision) {
		return m, fmt.Errorf("explicit lab-only native source revisions required")
	}
	if len(driver) == 0 || len(dll) == 0 || sha(driver) != m.DriverSHA || sha(dll) != m.DLLSHA {
		return m, fmt.Errorf("native driver/DLL digest mismatch")
	}
	cert, err := x509.ParseCertificate(certificate)
	if err != nil {
		return m, err
	}
	if !strings.EqualFold(fmt.Sprintf("%x", sha1.Sum(cert.Raw)), m.Thumbprint) {
		return m, fmt.Errorf("native lab certificate mismatch")
	}
	return m, nil
}

// Reuse a committed fork installer, not an ad-hoc working-tree copy. Its
// revision is retained separately from the compiled package's source revision.
func nativeWinFspInputs(directory, fork string) (map[string][]byte, error) {
	if !filepath.IsAbs(directory) || !filepath.IsAbs(fork) {
		return nil, fmt.Errorf("native package and fork paths must be absolute")
	}
	files := map[string][]byte{}
	for _, name := range []string{"manifest.json", "winfsp-x64.sys", "winfsp-x64.dll", "lab.cer"} {
		b, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return nil, err
		}
		files[`C:\lab\output\`+name] = b
	}
	_, err := validateNativeWinFsp(files[`C:\lab\output\manifest.json`], files[`C:\lab\output\winfsp-x64.sys`], files[`C:\lab\output\winfsp-x64.dll`], files[`C:\lab\output\lab.cer`])
	if err != nil {
		return nil, err
	}
	revision, err := exec.Command("git", "-C", fork, "rev-parse", "--verify", "HEAD^{commit}").Output()
	if err != nil {
		return nil, err
	}
	install, err := exec.Command("git", "-C", fork, "show", strings.TrimSpace(string(revision))+":tools/lab/install.ps1").Output()
	if err != nil {
		return nil, err
	}
	files[`C:\lab\install-native-winfsp.ps1`] = install
	files[`C:\lab\output\installer-revision.txt`] = revision
	files[`C:\lab\winfsp-x64.dll`] = files[`C:\lab\output\winfsp-x64.dll`]
	return files, nil
}

func TestNativeWinFspManifestRejectsMismatchedArtifacts(t *testing.T) {
	// Each rejection happens before starting a VM or trusting a certificate.
	for _, data := range []string{`{}`, `{"lab_only":true}`, `{"lab_only":false,"source_revision":"latest"}`} {
		if _, err := validateNativeWinFsp([]byte(data), []byte("driver"), []byte("dll"), nil); err == nil {
			t.Fatal("invalid manifest accepted")
		}
	}
}

func TestNativeWinFspManifestPairing(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	cert, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	driver, dll := []byte("driver-fixture"), []byte("DLL-fixture")
	m := nativeWinFspManifest{Revision: strings.Repeat("a", 40), DriverRevision: strings.Repeat("b", 40), DriverSHA: sha(driver), DLLSHA: sha(dll), Thumbprint: fmt.Sprintf("%x", sha1.Sum(cert)), LabOnly: true}
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, prefix := range [][]byte{nil, {0xef, 0xbb, 0xbf}} {
		if _, err := validateNativeWinFsp(append(prefix, encoded...), driver, dll, cert); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name              string
		driver, dll, cert []byte
	}{
		{"driver", []byte("other"), dll, cert}, {"dll", driver, []byte("other"), cert}, {"certificate", driver, dll, []byte("not DER")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateNativeWinFsp(encoded, tc.driver, tc.dll, tc.cert); err == nil {
				t.Fatal("mismatch accepted")
			}
		})
	}
	for _, change := range []func(*nativeWinFspManifest){func(m *nativeWinFspManifest) { m.LabOnly = false }, func(m *nativeWinFspManifest) { m.Revision = "HEAD" }, func(m *nativeWinFspManifest) { m.DriverRevision = "" }, func(m *nativeWinFspManifest) { m.Thumbprint = strings.Repeat("0", 40) }} {
		bad := m
		change(&bad)
		b, _ := json.Marshal(bad)
		if _, err := validateNativeWinFsp(b, driver, dll, cert); err == nil {
			t.Fatal("invalid manifest accepted")
		}
	}
}
