package mount

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// A Windows pod sees this volume at a container path (C:\data) that the file
// system cannot see: the container filter maps it onto the volume's network
// path. An absolute symlink a pod creates arrives as the drive-letter target
// "X:/rest" and, kept as written, cannot be followed from the pod, because the
// filter resolves it on the host and refuses to cross from the network volume
// back to C:. The CSI node plugin records, at each publish, where every
// container mounts this volume (MountRootsFile); a target under a mount path
// is stored volume-relative ("/rest"), which WinFsp resolves inside the volume
// for any reader and Linux mounts see like any other volume-absolute target.
//
// The creating container is not known, so a target is mapped only when the
// creator's view is certain: every container that mounts the volume on this
// node mounts it at the longest matching path, with the same subPath.

type containerMount struct {
	Path    string `json:"path"`
	SubPath string `json:"subPath,omitempty"`
}

// containerMounts is one container's mounts of this volume.
type containerMounts []containerMount

type mountRootsFile struct {
	// Publishes maps each publish target path to its pod's containers.
	Publishes map[string][]containerMounts `json:"publishes"`
}

func (wfs *WFS) mountRootTarget(target string) string {
	if wfs.option.MountRootsFile == "" {
		return target
	}
	data, err := os.ReadFile(wfs.option.MountRootsFile)
	if err != nil {
		if !os.IsNotExist(err) {
			glog.Warningf("read mount roots %s: %v", wfs.option.MountRootsFile, err)
		}
		return target
	}
	var roots mountRootsFile
	if err := json.Unmarshal(data, &roots); err != nil {
		glog.Warningf("parse mount roots %s: %v", wfs.option.MountRootsFile, err)
		return target
	}
	var views []containerMounts
	for _, containers := range roots.Publishes {
		views = append(views, containers...)
	}
	if mapped, ok := volumeTargetFromMountRoots(target, views); ok {
		return mapped
	}
	return target
}

// volumeTargetFromMountRoots maps the drive-letter target "X:/rest" to the
// volume-relative target it names in every container view, if it is certain.
func volumeTargetFromMountRoots(target string, views []containerMounts) (string, bool) {
	if len(views) == 0 || len(target) < 3 || target[2] != '/' {
		return "", false
	}
	targetParts, ok := drivePathComponents(target)
	if !ok {
		return "", false
	}
	longest := 0
	for _, view := range views {
		for _, m := range view {
			if parts, ok := drivePathComponents(m.Path); ok && len(parts) > longest && hasComponentPrefixFold(targetParts, parts) {
				longest = len(parts)
			}
		}
	}
	// a container mount path is a directory below a drive root
	if longest < 2 {
		return "", false
	}
	var sub []string
	subKnown := false
	for _, view := range views {
		found := false
		for _, m := range view {
			parts, ok := drivePathComponents(m.Path)
			if !ok || len(parts) != longest || !hasComponentPrefixFold(targetParts, parts) {
				continue
			}
			s, ok := subPathComponents(m.SubPath)
			if !ok {
				return "", false
			}
			if !subKnown {
				sub, subKnown = s, true
			} else if strings.Join(sub, "/") != strings.Join(s, "/") {
				return "", false
			}
			found = true
		}
		if !found {
			return "", false
		}
	}
	return "/" + strings.Join(append(sub, targetParts[longest:]...), "/"), true
}

// drivePathComponents splits "X:\a\b" or "X:/a/b" into ["X", "a", "b"],
// rejecting "." and ".." components.
func drivePathComponents(p string) ([]string, bool) {
	p = strings.ReplaceAll(p, `\`, "/")
	if len(p) < 2 || p[1] != ':' || !(('A' <= p[0] && p[0] <= 'Z') || ('a' <= p[0] && p[0] <= 'z')) ||
		(len(p) > 2 && p[2] != '/') {
		return nil, false
	}
	parts := []string{strings.ToUpper(p[:1])}
	for _, c := range strings.Split(p[2:], "/") {
		switch c {
		case "":
			continue
		case ".", "..":
			return nil, false
		}
		parts = append(parts, c)
	}
	return parts, true
}

// subPathComponents splits a pod volumeMount subPath, which is relative to the
// volume root and may not leave it.
func subPathComponents(p string) ([]string, bool) {
	p = strings.ReplaceAll(p, `\`, "/")
	if strings.HasPrefix(p, "/") || strings.Contains(p, ":") {
		return nil, false
	}
	var parts []string
	for _, c := range strings.Split(p, "/") {
		switch c {
		case "":
			continue
		case ".", "..":
			return nil, false
		}
		parts = append(parts, c)
	}
	return parts, true
}

func hasComponentPrefixFold(path, prefix []string) bool {
	if len(prefix) > len(path) {
		return false
	}
	for i := range prefix {
		if !strings.EqualFold(path[i], prefix[i]) {
			return false
		}
	}
	return true
}
