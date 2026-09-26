package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	labv1 "github.com/appmana/labcontainers/api/v1"
	"github.com/appmana/labcontainers/pkg/client"
	clab "github.com/appmana/labcontainers/pkg/containerlab"
)

// This is an opt-in native compiler test, not a deployment or driver installer.
func TestWindowsWinFspMSVCBuildLab(t *testing.T) {
	if os.Getenv("SEAWEEDFS_WINDOWS_MSVC_BUILD_LIVE") != "1" {
		t.Skip("set SEAWEEDFS_WINDOWS_MSVC_BUILD_LIVE=1")
	}
	iso := os.Getenv("SEAWEEDFS_WINDOWS_MSVC_ISO")
	want := os.Getenv("SEAWEEDFS_WINDOWS_MSVC_ISO_SHA256")
	if !filepath.IsAbs(iso) || strings.ContainsAny(iso, ":\n\r") || len(want) != 64 {
		t.Fatal("absolute ISO path and SHA-256 required")
	}
	f, err := os.Open(iso)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	closeErr := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if fmt.Sprintf("%x", h.Sum(nil)) != want {
		t.Fatal("compiler ISO digest mismatch")
	}
	img := os.Getenv("LABCONTAINERS_WINDOWS_IMAGE")
	if img == "" {
		t.Fatal("LABCONTAINERS_WINDOWS_IMAGE required")
	}
	out, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "winfsp-msvc-results-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained results: %s", out)
	write := func(name string, b []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(out, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	token := filepath.Base(out)
	write("provenance.txt", []byte(fmt.Sprintf("image=%s\niso_sha256=%s\n", img, want)))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	c, err := client.Launch(ctx, client.Options{LabdPath: os.Getenv("LABCONTAINERS_LABD")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	config := windowsTopologyConfig(img)
	config.Topology.Nodes["vm"].Binds = []string{iso + ":/compiler.iso:ro"}
	config.Topology.Nodes["vm"].Env = map[string]string{"QEMU_ADDITIONAL_ARGS": "-drive file=/compiler.iso,media=cdrom,readonly=on"}
	topology, err := clab.Source(config)
	if err != nil {
		t.Fatal(err)
	}
	lab, err := c.Start(ctx, &labv1.LabSpec{Topology: topology, Nodes: map[string]*labv1.NodeExtension{"vm": {Control: "qga"}}}, 60*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ps := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	_, err = lab.RunTimeline(ctx, &labv1.TimelineAction{Action: &labv1.TimelineAction_WaitExec{WaitExec: &labv1.WaitExec{Exec: &labv1.ExecRequest{Node: &labv1.NodeRef{Node: "vm"}, Argv: []string{ps, "-NoProfile", "-Command", "Write-Output 'compiler-ready-" + token + "'"}, TimeoutMillis: 10000}, TimeoutMillis: 600000, RetryMillis: 2000, StdoutContains: []byte("compiler-ready-" + token)}}})
	if err != nil {
		t.Fatal(err)
	}
	n := lab.Node("vm")
	for _, name := range []string{"build-winfsp-msvc.ps1", "provision-winfsp-msvc-lab.ps1", "winfsp-guid-mount.patch", "winfsp-default-security.patch"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "hack", "appmana", name))
		if err != nil {
			t.Fatal(err)
		}
		write("input-"+name, b)
		if err = n.Put(ctx, `C:\lab\`+name, 0600, b); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("Windows ready; provisioning offline compiler and building baseline/candidate")
	result, buildErr := n.ExecWithTimeout(ctx, 40*time.Minute, ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", `C:\lab\provision-winfsp-msvc-lab.ps1`, "-CompletionToken", token)
	write("command-result.txt", []byte(fmt.Sprintf("%+v\nerror=%v\n", result, buildErr)))
	collectionCtx, stopCollection := context.WithTimeout(context.Background(), 8*time.Minute)
	defer stopCollection()
	exec := func(command string) ([]byte, error) {
		r, e := n.ExecWithTimeout(collectionCtx, time.Minute, ps, "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; "+command)
		if e != nil {
			return nil, e
		}
		if r.GetExitCode() != 0 {
			return nil, fmt.Errorf("guest exit %d: %s", r.GetExitCode(), r.GetStderr())
		}
		return r.GetStdout(), nil
	}
	// Preserve diagnostics even when the compiler fails; missing required files
	// are errors, never an empty artifact silently accepted as a successful build.
	for _, path := range []string{"toolchain.json", "baseline-console.log", "candidate-console.log"} {
		b, e := readWindowsArtifact(`C:\lab\`+path, exec)
		if e != nil {
			t.Error(path, e)
		} else {
			write(path, b)
		}
	}
	if buildErr != nil || result.GetExitCode() != 0 {
		for _, mode := range []string{"baseline", "candidate"} {
			for _, name := range []string{"build.log", "build.binlog", "build-arguments.json", "winfsp-x64.dll.source.patch"} {
				b, e := readWindowsArtifact(`C:\lab\`+mode+`\`+name, exec)
				if e != nil {
					t.Logf("failure evidence %s/%s: %v", mode, name, e)
				} else {
					write(mode+"-"+name, b)
				}
			}
		}
		t.Fatalf("native build failed: transport=%v result=%+v", buildErr, result)
	}
	for _, mode := range []string{"baseline", "candidate"} {
		if !strings.Contains(string(result.GetStdout()), "BUILD_COMPLETE_"+mode+":"+token) {
			t.Fatal("missing fresh build completion marker", mode)
		}
		artifacts := map[string][]byte{}
		for _, name := range []string{"winfsp-x64.dll", "winfsp-x64.dll.manifest.txt", "build.log", "build.binlog", "build-arguments.json"} {
			b, e := readWindowsArtifact(`C:\lab\`+mode+`\`+name, exec)
			if e != nil {
				t.Fatal(mode, name, e)
			}
			artifacts[name] = b
			write(mode+"-"+name, b)
		}
		// Empty baseline patches are valid, unlike empty compiled artifacts.
		patchPath := `C:\lab\` + mode + `\winfsp-x64.dll.source.patch`
		var patch []byte
		if mode == "baseline" {
			b, e := exec("(Get-Item '" + patchPath + "').Length")
			if e != nil || strings.TrimSpace(string(b)) != "0" {
				t.Fatal("baseline patch is not empty", e)
			}
		} else {
			patch, err = readWindowsArtifact(patchPath, exec)
			if err != nil {
				t.Fatal(err)
			}
		}
		write(mode+"-winfsp-x64.dll.source.patch", patch)
		if mode == "candidate" {
			// git diff orders paths lexically: fuse/fuse_intf.c then mount.c.
			securityPatch, e := os.ReadFile(filepath.Join(out, "input-winfsp-default-security.patch"))
			if e != nil {
				t.Fatal(e)
			}
			mountPatch, e := os.ReadFile(filepath.Join(out, "input-winfsp-guid-mount.patch"))
			if e != nil || !bytes.Equal(patch, append(securityPatch, mountPatch...)) {
				t.Fatal("candidate differs from the exact staged patch", e)
			}
		}
		if !strings.Contains(string(artifacts["winfsp-x64.dll.manifest.txt"]), "source_mode="+mode+"\r\n") {
			t.Fatal("wrong manifest source mode", mode)
		}
		if err = validateWinFspLabManifest(artifacts["winfsp-x64.dll.manifest.txt"], artifacts["winfsp-x64.dll"], patch); err != nil {
			t.Fatal(err)
		}
		recipe, e := os.ReadFile(filepath.Join(out, "input-build-winfsp-msvc.ps1"))
		if e != nil {
			t.Fatal(e)
		}
		for key, value := range map[string]string{"build_script_sha256": sha(recipe), "build_toolchain": "msvc", "platform_toolset": "v143", "vc_tools_version": "14.44.35207", "windows_sdk_version": "10.0.26100.0"} {
			if !strings.Contains(string(artifacts["winfsp-x64.dll.manifest.txt"]), key+"="+value+"\r\n") {
				t.Fatal("build evidence differs from input", key)
			}
		}
	}
}
